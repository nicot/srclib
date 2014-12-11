package src

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/kr/fs"

	"sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
	"sourcegraph.com/sourcegraph/srclib/buildstore"
	"sourcegraph.com/sourcegraph/srclib/dep"
	"sourcegraph.com/sourcegraph/srclib/graph"
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
	AddDeps  bool   `long:"add-deps" description:"add dependency repos to remote if not present (specify this if you get a 'repo not found' error)"`
	RepoURI  string `short:"r" long:"repo" description:"repository URI (defaults to current VCS repository 'srclib' or 'origin' remote URL)" required:"yes"`
	CommitID string `short:"c" long:"commit" description:"commit ID of repository to search (defaults to current repository's commit if build data is present, otherwise newest built remote commit on default branch)"`
}

var queryCmd QueryCmd

func (c *QueryCmd) Execute(args []string) error {
	repo := openCurrentRepo()
	buildStore, err := buildstore.LocalRepo(repo.RootDir)
	if err != nil {
		return err
	}
	commitFS := buildStore.Commit(repo.CommitID)
	exists, err := buildstore.BuildDataExistsForCommit(buildStore, repo.CommitID)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("No build data found. Try running `src config` first.")
	}

	cl := NewAPIClientWithAuthIfPresent()

	repoAndDepURIs := []string{c.RepoURI}

	// Read deps.
	depSuffix := buildstore.DataTypeSuffix([]*dep.ResolvedDep{})
	w := fs.WalkFS(".", commitFS)
	seenDepURI := map[string]bool{}
	for w.Step() {
		depfile := w.Path()
		if strings.HasSuffix(depfile, depSuffix) {
			var deps []*dep.Resolution
			f, err := commitFS.Open(depfile)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := json.NewDecoder(f).Decode(&deps); err != nil {
				return fmt.Errorf("%s: %s", depfile, err)
			}
			for _, d := range deps {
				if d.Target != nil && d.Target.ToRepoCloneURL != "" {
					depURI := graph.MakeURI(d.Target.ToRepoCloneURL)
					if !seenDepURI[depURI] {
						repoAndDepURIs = append(repoAndDepURIs, depURI)
						seenDepURI[depURI] = true
					}
				}
			}
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
	fmt.Fprint(&queryBuf, strings.Join(args, " "))
	query := queryBuf.String()
	if GlobalOpt.Verbose {
		log.Printf("# Query: %q", query)
	}

	res, _, err := cl.Search.Search(&sourcegraph.SearchOptions{
		Query:       query,
		Defs:        true,
		ListOptions: sourcegraph.ListOptions{PerPage: 25},
	})
	if err != nil {
		return err
	}
	defs := res.Defs

	// HACK: until we get the indexed_globally fix in, filter out dupes
	seen := map[string]bool{}

	for _, def := range defs {
		seenKey := def.Repo + def.UnitType + def.Unit + string(def.Path)
		if seen[seenKey] {
			continue
		}
		seen[seenKey] = true

		// Fetch docs and stats.
		def, _, err = cl.Defs.Get(def.DefSpec(), &sourcegraph.DefGetOptions{Doc: true})
		if err != nil {
			return err
		}

		if f := def.FmtStrings; f != nil {
			fromDep := !graph.URIEqual(def.Repo, c.RepoURI)

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

			fmt.Printf("%s%s%s%s\n", kw, bold(red(name)), f.NameAndTypeSeparator, bold(typ))
		} else {
			fmt.Printf("(unable to format: %s from %s)\n", def.Name, def.Repo)
		}

		if doc := strings.TrimSpace(stripHTML(def.DocHTML)); doc != "" {
			fmt.Println(doc, "    ")
		}

		src := fmt.Sprintf("@ %s : %s", def.Repo, def.File)

		var stat string
		if def.RRefs() > 0 || def.XRefs() > 0 {
			stat = fmt.Sprintf("%d xrefs %d rrefs", def.XRefs(), def.RRefs())
		}

		fmt.Printf("%-50s %s\n", fade(src), fade(stat))

		fmt.Println()
	}
	return nil
}

func stripHTML(html string) string {
	s := strings.Replace(strings.Replace(html, "<p>", "", -1), "</p>", "", -1)
	return strings.Replace(s, "\n\n", "\n", -1)
}
