package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/journal"
)

// importStatesCmd is the subcommand that stages a one-time topic state
// import. See internal/journal/stateimport.go: it does not write the
// journal, because the running relay owns that file. The relay applies
// the staged copy at its next start or graceful reload.
const importStatesCmd = "import-states"

// runImportStates is `zulip-acp import-states [-config F] [-state-dir D] <file>`.
func runImportStates(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(importStatesCmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to JSON config file (for state_dir)")
	stateDir := fs.String("state-dir", "", "the relay's state dir (default: config state_dir, else the default)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: zulip-acp %s [-config F] [-state-dir D] <topic-states.json>\n", importStatesCmd)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	dir := *stateDir
	if dir == "" && *configPath != "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(stderr, "config: %v\n", err)
			return 1
		}
		dir = cfg.StateDir
	}
	if dir == "" {
		dir = config.DefaultStateDir()
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	n, skipped, err := journal.StageStateImport(dir, b)
	for _, s := range skipped {
		fmt.Fprintf(stderr, "skipped %s\n", s)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Staged %d topic state entries in %s.\n"+
		"The relay applies them at its next start or graceful reload (SIGHUP), to topics that have no state yet.\n",
		n, dir)
	return 0
}
