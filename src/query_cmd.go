package src

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/bobappleyard/readline"
	"github.com/kr/fs"
	"github.com/kr/text"

	"sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
	"sourcegraph.com/sourcegraph/srclib/buildstore"
	"sourcegraph.com/sourcegraph/srclib/dep"
	"sourcegraph.com/sourcegraph/srclib/graph"
	"sourcegraph.com/sourcegraph/srclib/util"
	"sourcegraph.com/sourcegraph/vcsstore/vcsclient"
)

func init() {
	queryGroup, err := CLI.AddCommand("query",
		"search code in current project and dependencies",
		"The query (q) command searches for code in the current project and its dependencies. The results include documentation, definitions, etc.",
		&queryCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
	queryGroup.Aliases = append(queryGroup.Aliases, "q")
	if repo := openCurrentRepo(); repo != nil {
		SetOptionDefaultValue(queryGroup.Group, "repo", repo.URI())
	}
}

type QueryCmd struct {
	AddDeps      bool   `long:"add-deps" description:"add dependency repos to remote if not present (specify this if you get a 'repo not found' error)"`
	RepoURI      string `short:"r" long:"repo" description:"repository URI (defaults to current VCS repository 'srclib' or 'origin' remote URL)" required:"yes"`
	CommitID     string `short:"c" long:"commit" description:"commit ID of repository to search (defaults to current repository's commit if build data is present, otherwise newest built remote commit on default branch)"`
	Def          bool   `short:"d" long:"def" description:"show definitions"`
	Refs         int    `short:"x" long:"refs" description:"show this many references/examples"`
	ContextLines int    `short:"L" long:"context-lines" description:"number of surrounding context lines to show in ref/example code snippets" default:"3"`
	Authors      bool   `short:"A" long:"authors" description:"show authors/committers of each def"`

	Terse bool `short:"1" long:"terse" description:"terse output mode (one-line per def and ref result)"`
}

var queryCmd QueryCmd

func (c *QueryCmd) Execute(args []string) error {
	repo := openCurrentRepo()

	cl := NewAPIClientWithAuthIfPresent()

	depTargets, err := listRepoDependencies(cl, repo, c.RepoURI)
	if err != nil {
		return err
	}
	seenDepURI := map[string]bool{}
	repoAndDepURIs := []string{c.RepoURI}
	for d := range depTargets {
		uri := graph.MakeURI(d.ToRepoCloneURL)
		if !seenDepURI[uri] {
			repoAndDepURIs = append(repoAndDepURIs, uri)
			seenDepURI[uri] = true
		}
	}

	if c.AddDeps {
		// Ensure all dep repos are added.
		for _, repoURI := range repoAndDepURIs {
			_, _, err := cl.Repos.GetOrCreate(sourcegraph.RepoSpec{URI: repoURI}, nil)
			if err != nil {
				return fmt.Errorf("get/create repo %s: %s", repoURI, err)
			}
		}
	}

	// Determine which commit to search on the server.
	var commitID string
	if c.CommitID != "" {
		commitID = c.CommitID
	} else if repo != nil {
		commitID = repo.CommitID
	}
	// Check if the commit has build data on the server. If commitID
	// == "", this will check the default branch.
	repoRevSpec := sourcegraph.RepoRevSpec{RepoSpec: sourcegraph.RepoSpec{URI: c.RepoURI}, Rev: commitID}
	b, _, err := cl.Repos.GetBuild(repoRevSpec, &sourcegraph.RepoGetBuildOptions{Exact: true})
	if err != nil {
		return err
	}
	if b.Exact != nil && b.Exact.CommitID == commitID {
		// The remote has a build for the commit we want.
	} else if b.LastSuccessfulCommit == nil {
		log.Printf("# Warning: no search index for %s because it has no successful remote builds.", c.RepoURI)
	} else {
		log.Printf("# Searching in commit %s (%d commits behind) because commit %s is not built.", b.LastSuccessfulCommit.ID, b.CommitsBehind, commitID)
		commitID = string(b.LastSuccessfulCommit.ID)
	}

	var queryBuf bytes.Buffer
	for _, repoURI := range repoAndDepURIs {
		fmt.Fprint(&queryBuf, "repo:", repoURI)
		if repoURI == c.RepoURI { // current repo
			fmt.Fprint(&queryBuf, "@", commitID)
		}
		fmt.Fprint(&queryBuf, " ")
	}
	queryConstraints := queryBuf.String()

	if len(args) > 0 {
		queryString := strings.Join(args, " ")
		return query(c, cl, queryConstraints, queryString)
	}

	// Readline completion from names of defs in all relevant repos.
	var (
		comps   []string
		compc   = make(chan string)
		compsMu sync.Mutex
	)
	readline.Completer = func(query, ctx string) []string {
		compsMu.Lock()
		defer compsMu.Unlock()
		var matches []string
		for _, comp := range comps {
			if strings.HasPrefix(strings.ToLower(comp), strings.ToLower(query)) {
				matches = append(matches, comp)
			}
		}
		return matches
	}
	go func() {
		for comp := range compc {
			compsMu.Lock()
			comps = append(comps, comp)
			compsMu.Unlock()
		}
	}()
	for _, repoURI := range repoAndDepURIs {
		compc <- path.Base(repoURI)
	}
	for depTarget := range depTargets {
		go func(dt dep.ResolvedTarget) {
			repoRevSpec := sourcegraph.RepoRevSpec{
				RepoSpec: sourcegraph.RepoSpec{URI: graph.MakeURI(dt.ToRepoCloneURL)},
				Rev:      dt.ToRevSpec,
			}
			b, _, err := cl.Repos.GetBuild(repoRevSpec, nil)
			if err != nil || b == nil {
				if GlobalOpt.Verbose {
					log.Printf("Warning: unable to get build for %s (for query completion): %s.", dt.ToRepoCloneURL, err)
				}
				return
			}
			if b.LastSuccessful == nil {
				if GlobalOpt.Verbose {
					log.Printf("Warning: no successful builds for %s (for query completion).", dt.ToRepoCloneURL)
				}
				return
			}

			repoRev := graph.MakeURI(dt.ToRepoCloneURL)
			if b.LastSuccessful.CommitID != "" {
				repoRev += "@" + b.LastSuccessful.CommitID
			}
			defs, _, err := cl.Defs.List(&sourcegraph.DefListOptions{
				RepoRevs:    []string{repoRev},
				UnitTypes:   []string{dt.ToUnitType},
				Unit:        dt.ToUnit,
				Exported:    true,
				Sort:        "xrefs",
				Direction:   "desc",
				ListOptions: sourcegraph.ListOptions{PerPage: 500},
			})
			if err != nil {
				if GlobalOpt.Verbose {
					log.Printf("Warning: unable to list defs for %s (for query completion): %s.", dt.ToRepoCloneURL, err)
				}
				return
			}
			if GlobalOpt.Verbose {
				log.Println("Got completions for", dt.ToRepoCloneURL, dt.ToUnitType)
			}
			for _, def := range defs {
				compc <- def.Name
				if def.FmtStrings != nil {
					qname := def.FmtStrings.Name.DepQualified
					if strings.Count(qname, ".") < 2 && !strings.Contains(qname, "(") {
						// Only complete on simple selectors for now.
						compc <- qname
					}
				}
			}
		}(depTarget)
	}

	defer readline.Cleanup()
	histFile := filepath.Join(util.CurrentUserHomeDir(), ".src-query-history")
	if err := readline.LoadHistory(histFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	defer func() {
		if err := readline.SaveHistory(histFile); err != nil {
			log.Printf("Warning: unable to save query history to %s: %s.", histFile, err)
		}
	}()
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, syscall.SIGINT)
	readline.CatchSigint = true
	errc := make(chan error)
	done := make(chan struct{})
	go func() {
		for {
			line, err := readline.String(cyan("✱") + " ")
			if err != nil {
				if err == io.EOF {
					close(done)
				} else {
					errc <- err
				}
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			readline.AddHistory(line)
			if err := query(c, cl, queryConstraints, line); err != nil {
				errc <- err
			}
		}
	}()
	select {
	case <-sigint:
		return nil
	case err := <-errc:
		return err
	case <-done:
	}
	return nil
}

func query(c *QueryCmd, cl *sourcegraph.Client, queryConstraints, queryString string) error {
	query := queryConstraints + " " + queryString
	if GlobalOpt.Verbose {
		log.Printf("# Query: %q", query)
	}

	res, _, err := cl.Search.Search(&sourcegraph.SearchOptions{
		Query:       query,
		Defs:        true,
		ListOptions: sourcegraph.ListOptions{PerPage: 12},
	})
	if err != nil {
		return err
	}
	defs := res.Defs

	if len(defs) == 0 {
		fmt.Printf(red("No results for: %s\n"), queryString)
		return nil
	}

	// HACK: until we get the indexed_globally fix in, filter out dupes
	seen := map[string]bool{}

	for _, def := range defs {
		seenKey := def.Repo + def.UnitType + def.Unit + string(def.Path)
		if seen[seenKey] {
			continue
		}
		seen[seenKey] = true

		printDefSummary(c, def)

		if c.Def {
			// Show definition.
			entrySpec := sourcegraph.TreeEntrySpec{
				RepoRev: sourcegraph.RepoRevSpec{
					RepoSpec: sourcegraph.RepoSpec{URI: def.Repo},
					Rev:      def.CommitID,
					CommitID: def.CommitID,
				},
				Path: def.File,
			}
			opt := &sourcegraph.RepoTreeGetOptions{
				GetFileOptions: vcsclient.GetFileOptions{
					FileRange: vcsclient.FileRange{StartByte: def.DefStart, EndByte: def.DefEnd},
					FullLines: true,
				},
			}
			entry, _, err := cl.RepoTree.Get(entrySpec, opt)
			if err == nil {
				entry.Contents = bytes.Replace(entry.Contents, []byte(def.Name), []byte(bold(yellow(def.Name))), -1)
				fmt.Println(text.Indent(string(entry.Contents), "  "))
			} else {
				log.Printf("Error fetching def %s in %s. Skipping.", def.Path, def.Repo)
				if GlobalOpt.Verbose {
					log.Println(err)
				}
			}
			if !c.Terse {
				fmt.Println()
			}
		}

		if c.Authors {
			opt := &sourcegraph.DefListAuthorsOptions{ListOptions: sourcegraph.ListOptions{PerPage: 50}}
			authors, _, err := cl.Defs.ListAuthors(def.DefSpec(), opt)
			if err == nil {
				for _, a := range authors {
					fmt.Println(a.AuthorEmail)
				}
			} else {
				log.Printf("Error listing authors for %s in %s unit %s. Skipping.", def.Path, def.Repo, def.Unit)
				if GlobalOpt.Verbose {
					log.Println(err)
				}
				log.Println()
			}
		}

		if c.Refs > 0 {
			opt := &sourcegraph.DefListRefsOptions{ListOptions: sourcegraph.ListOptions{PerPage: c.Refs}}
			xs, _, err := cl.Defs.ListRefs(def.DefSpec(), opt)
			if err != nil {
				log.Printf("Error listing refs for %s in %s unit %s. Skipping.", def.Path, def.Repo, def.Unit)
				if GlobalOpt.Verbose {
					log.Println(err)
				}
				log.Println()
				continue
			}
			if !c.Terse {
				fmt.Println()
			}
			for i, x := range xs {
				if !c.Terse {
					fmt.Print("\t")
				}

				var refLabel string
				if i == 0 {
					refLabel = "Ref"
				} else {
					refLabel = strings.Repeat(" ", len("Ref"))
				}

				var src string
				fromDep := !graph.URIEqual(def.Repo, x.Repo)
				if fromDep {
					src = fmt.Sprintf("%s @ %s : %s", refLabel, x.Repo, x.File)
				} else {
					src = fmt.Sprintf("%s @ %s", refLabel, x.File)
				}
				fmt.Println(fade(src))

				if !c.Terse {
					entrySpec := sourcegraph.TreeEntrySpec{
						RepoRev: sourcegraph.RepoRevSpec{
							RepoSpec: sourcegraph.RepoSpec{URI: x.Repo},
							Rev:      x.CommitID,
							CommitID: x.CommitID,
						},
						Path: x.File,
					}
					opt := &sourcegraph.RepoTreeGetOptions{
						GetFileOptions: vcsclient.GetFileOptions{
							FileRange:          vcsclient.FileRange{StartByte: x.Start, EndByte: x.End},
							ExpandContextLines: c.ContextLines,
							FullLines:          true,
						},
					}
					entry, _, err := cl.RepoTree.Get(entrySpec, opt)
					if err != nil {
						log.Printf("Error fetching example in %s at %s. Skipping.", x.Repo, x.File)
						if GlobalOpt.Verbose {
							log.Println(err)
						}
						log.Println()
						continue
					}

					entry.Contents = bytes.Replace(entry.Contents, []byte(def.Name), []byte(bold(yellow(def.Name))), -1)
					fmt.Println(text.Indent(string(entry.Contents), "\t"))

					fmt.Println()
				}
			}
		}

		if !c.Terse || c.Refs > 0 {
			fmt.Println()
		}
	}
	return nil
}

func printDefSummary(c *QueryCmd, def *sourcegraph.Def) {
	fromDep := !graph.URIEqual(def.Repo, c.RepoURI)

	var title string
	if f := def.FmtStrings; f != nil {

		kw := f.DefKeyword
		if kw != "" {
			kw += " "
		}

		var name string
		if fromDep {
			name = f.Name.LanguageWideQualified
		} else {
			name = f.Name.DepQualified
		}

		var typ string
		if fromDep {
			typ = f.Type.RepositoryWideQualified
		} else {
			typ = f.Type.DepQualified
		}

		title = fmt.Sprintf("%s%s%s%s", kw, bold(red(name)), f.NameAndTypeSeparator, bold(typ))
	} else {
		if GlobalOpt.Verbose {
			log.Printf("No def format strings found for def %v.", def.DefKey)
		}
		title = fmt.Sprintf("%s: %s", def.Unit, def.Name)
	}

	var src string
	if fromDep {
		src = fmt.Sprintf("@ %s : %s", def.Repo, def.File)
	} else {
		src = fmt.Sprintf("@ %s", def.File)
	}

	if c.Terse {
		fmt.Println(title, src)
	} else {
		fmt.Println(title)

		if doc := strings.TrimSpace(stripHTML(def.DocHTML)); doc != "" {
			fmt.Println(text.Indent(doc, "    "))
		}

		// TODO(sqs): we'd need to fetch the def separately to get
		// stats; stats are not included in the search result.
		var stat string
		if def.RRefs() > 0 || def.XRefs() > 0 {
			stat = fmt.Sprintf("%d xrefs %d rrefs", def.XRefs(), def.RRefs())
		}

		fmt.Printf("%-50s %s\n", fade(src), fade(stat))
	}
}

func stripHTML(html string) string {
	s := strings.Replace(strings.Replace(html, "<p>", "", -1), "</p>", "", -1)
	return strings.Replace(s, "\n\n", "\n", -1)
}

// listRepoDependencies determines the dependencies of the current
// repo either by looking them up in the depresolve build data files
// locally or by querying the Sourcegraph API.
func listRepoDependencies(cl *sourcegraph.Client, repo *Repo, uri string) (map[dep.ResolvedTarget]struct{}, error) {
	depTargets := map[dep.ResolvedTarget]struct{}{}

	tryLocal := repo != nil && repo.URI() == uri

	// Try determining dependencies locally.
	if tryLocal {
		buildStore, err := buildstore.LocalRepo(repo.RootDir)
		if err != nil {
			return nil, err
		}
		commitFS := buildStore.Commit(repo.CommitID)
		exists, err := buildstore.BuildDataExistsForCommit(buildStore, repo.CommitID)
		if err != nil {
			return nil, err
		}
		if exists {
			depSuffix := buildstore.DataTypeSuffix([]*dep.ResolvedDep{})
			w := fs.WalkFS(".", commitFS)
			hasDepResolveFile := false
			for w.Step() {
				depfile := w.Path()
				if strings.HasSuffix(depfile, depSuffix) {
					var deps []*dep.Resolution
					f, err := commitFS.Open(depfile)
					if err != nil {
						return nil, err
					}
					defer f.Close()
					if err := json.NewDecoder(f).Decode(&deps); err != nil {
						return nil, fmt.Errorf("%s: %s", depfile, err)
					}
					hasDepResolveFile = true
					for _, d := range deps {
						if d.Target != nil && d.Target.ToRepoCloneURL != "" {
							depTargets[*d.Target] = struct{}{}
						}
					}
				}
			}
			if hasDepResolveFile {
				if GlobalOpt.Verbose {
					log.Printf("# Found %d resolved deps locally.", len(depTargets))
				}
				return depTargets, nil
			}
		}
	}

	// Unable to determine deps locally. Try looking them up on Sourcegraph.
	var commitID string
	repoRevSpec := sourcegraph.RepoRevSpec{RepoSpec: sourcegraph.RepoSpec{URI: uri}, Rev: repo.CommitID}
	b, _, err := cl.Repos.GetBuild(repoRevSpec, &sourcegraph.RepoGetBuildOptions{Exact: true})
	if err != nil {
		return nil, err
	}
	if b.Exact != nil && b.Exact.CommitID == repo.CommitID {
		// The remote has a build for the commit we want.
		commitID = repo.CommitID
		if GlobalOpt.Verbose {
			log.Printf("# Remote build #%d found for current commit %s.", b.Exact.BID, repo.CommitID)
		}
	} else if b.LastSuccessfulCommit != nil {
		if GlobalOpt.Verbose {
			log.Printf("# Finding dependencies in commit %s (%d commits behind) because commit %s is not built.", b.LastSuccessfulCommit.ID, b.CommitsBehind, repo.CommitID)
		}
		commitID = string(b.LastSuccessfulCommit.ID)
	}

	if commitID != "" {
		deps, _, err := cl.Repos.ListDependencies(repoRevSpec, &sourcegraph.RepoListDependenciesOptions{
			ListOptions: sourcegraph.ListOptions{PerPage: 50},
		})
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			tgt := dep.ResolvedTarget{ToRepoCloneURL: d.ToRepo}
			depTargets[tgt] = struct{}{}
		}
		if GlobalOpt.Verbose {
			log.Printf("# Found %d dependencies on remote.", len(depTargets))
		}
		return depTargets, nil
	}

	return nil, fmt.Errorf("No dependencies found for repo %s commit %s. Try running `src config && src make` to build this commit locally, or run `src remote build` to trigger a build on Sourcegraph.", uri, repo.CommitID)
}
