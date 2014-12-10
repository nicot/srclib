package src

import (
	"fmt"
	"log"
	"time"

	"sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
)

func init() {
	deltaGroup, err := CLI.AddCommand("delta",
		"summarize changes and impacts between any 2 commits",
		"The delta command and its subcommands show summaries of changes and their impact on this project and projects that depend on it.",
		&deltaCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = deltaGroup.AddCommand("defs",
		"list defs that changed between commits",
		"The `src delta defs` subcommand lists definitions (functions, types, etc.) that changed between any 2 commits.",
		&deltaDefsCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
}

type DeltaCmd struct{}

var deltaCmd DeltaCmd

func (c *DeltaCmd) Execute(args []string) error {
	return nil
}

type DeltaDefsCmd struct {
	Base string `short:"f" long:"from" description:"base commit" required:"yes"`
	Head string `short:"t" long:"to" description:"head commit" default:"master"`

	Stat bool `long:"stat" description:"show statistics (# added/changed/removed)"`
}

var deltaDefsCmd DeltaDefsCmd

func (c *DeltaDefsCmd) Execute(args []string) error {
	cl := NewAPIClientWithAuthIfPresent()

	repo, err := OpenRepo(".")
	if err != nil {
		return err
	}

	ds := sourcegraph.DeltaSpec{
		Base: sourcegraph.RepoRevSpec{RepoSpec: repo.RepoRevSpec().RepoSpec, Rev: c.Base},
		Head: sourcegraph.RepoRevSpec{RepoSpec: repo.RepoRevSpec().RepoSpec, Rev: c.Head},
	}

	delta, _, err := cl.Deltas.Get(ds, nil)
	if err != nil {
		return err
	}

	if GlobalOpt.Verbose {
		log.Printf("# Resolved delta:")
		buildStr := func(b *sourcegraph.Build) string {
			if b == nil {
				return "(none)"
			}
			if b.EndedAt.Valid && b.Success {
				return fmt.Sprintf("%d (finished %s ago)", b.BID, time.Since(b.EndedAt.Time))
			}
			return fmt.Sprintf("%d (not ready)", b.BID)
		}
		log.Printf("# Base: %s@%s, build %s", delta.Base.RepoSpec.URI, delta.Base.CommitID, buildStr(delta.BaseBuild))
		log.Printf("# Head: %s@%s, build %s", delta.Head.RepoSpec.URI, delta.Head.CommitID, buildStr(delta.HeadBuild))
	}

	fmt.Printf(fade("# From %s..%s\n"), delta.Base.CommitID, delta.Head.CommitID)

	deltaDefs, _, err := cl.Deltas.ListDefs(ds, nil)
	if err != nil {
		return err
	}

	if c.Stat {
		fmt.Println(bold(green(fmt.Sprintf("+ %d", deltaDefs.DiffStat.Added))))
		fmt.Println(bold(yellow(fmt.Sprintf("▲ %d", deltaDefs.DiffStat.Changed))))
		fmt.Println(bold(red(fmt.Sprintf("- %d", deltaDefs.DiffStat.Deleted))))
		fmt.Println()
	}

	for _, deltaDef := range deltaDefs.Defs {

		if deltaDef.Added() {
			fmt.Println(bold(green("+")), fmtDeltaDefName(deltaDef.Head))
		}
		if deltaDef.Changed() {
			fmt.Println(bold(yellow("▲")), fmtDeltaDefName(deltaDef.Base))
		}
		if deltaDef.Deleted() {
			fmt.Println(bold(red("+")), fmtDeltaDefName(deltaDef.Base))
		}
	}

	return nil
}

func fmtDeltaDefName(d *sourcegraph.Def) string {
	f := d.FmtStrings
	if f == nil {
		return d.Name
	}

	kw := f.DefKeyword
	if kw != "" {
		kw += " "
	}

	name := f.Name.DepQualified
	typ := f.Type.DepQualified

	return fmt.Sprint(kw, bold(name), f.NameAndTypeSeparator, typ)
}
