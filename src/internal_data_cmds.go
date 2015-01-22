package src

import (
	"encoding/json"
	"log"
	"os"

	"sourcegraph.com/sourcegraph/srclib/grapher"
)

func init() {
	c, err := CLI.AddCommand("internal", "(internal subcommands - do not use)", "Internal subcommands. Do not use.", &struct{}{})
	if err != nil {
		log.Fatal(err)
	}

	_, err = c.AddCommand("normalize-graph-data", "", "", &normalizeGraphDataCmd)
	if err != nil {
		log.Fatal(err)
	}
}

type NormalizeGraphDataCmd struct {
	UnitType string `long:"unit-type" description:"source unit type (e.g., GoPackage)"`
	Dir      string `long:"dir" description:"directory of source unit (SourceUnit.Dir field)"`
	OffsetTy string `long:"offset-type" description:"does the toolchain output byte or character offsets?"`
}

var normalizeGraphDataCmd NormalizeGraphDataCmd

func (c *NormalizeGraphDataCmd) Execute(args []string) error {
	in := os.Stdin

	var o *grapher.Output
	if err := json.NewDecoder(in).Decode(&o); err != nil {
		return err
	}

	var offsetTy grapher.OffsetType
	if c.OffsetTy == "byte" {
		offsetTy = grapher.OffsetByte
	} else if c.OffsetTy == "character" {
		offsetTy = grapher.OffsetChar
	} else {
		offsetTy = grapher.OffsetUnspecified
	}

	if err := grapher.NormalizeData(offsetTy, c.UnitType, c.Dir, o); err != nil {
		return err
	}

	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}

	if _, err := os.Stdout.Write(data); err != nil {
		return err
	}

	return nil
}
