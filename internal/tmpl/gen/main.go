// Command gen writes the generated Switch template (go generate ./internal/tmpl).
//
//	go run ./internal/tmpl/gen switch/dibs.template.yaml          # write
//	go run ./internal/tmpl/gen -check switch/dibs.template.yaml   # fail if stale (CI)
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/vivekyarra/dibs/internal/tmpl"
)

func main() {
	check := flag.Bool("check", false, "only check that the file is up to date")
	flag.Parse()
	path := tmpl.OutputPath
	if flag.NArg() > 0 {
		path = flag.Arg(0)
	}
	var err error
	if *check {
		err = tmpl.Check(path)
	} else {
		err = tmpl.Write(path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}
