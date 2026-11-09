package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kfet/zulip-acp/internal/config"
)

// Config subcommands. They exist because an agent that edits the relay
// config needs two things it could not get before (incident
// 2026-11-09): a way to check a file before it reloads the relay, and
// the list of keys, without `strings` on the binary.
const (
	checkConfigCmd  = "check-config"
	configSchemaCmd = "config-schema"
)

// usageText is the top of `zulip-acp -h`. Go's flag package lists only
// flags, so without it the subcommands are not discoverable: a
// `zulip-acp --check` that printed this help verified nothing.
const usageText = `usage: zulip-acp [flags]                 run the relay
       zulip-acp check-config [-config F]  check a config file; exit 1 if invalid (run before every reload)
       zulip-acp config-schema [prefix]    print every config key with its type and doc
       zulip-acp status | update [...]     show / update installed relays on this host
       zulip-acp import-states [...] F     stage a topic state import

flags:
`

// usage writes the top-level help.
func usage(w io.Writer, fs *flag.FlagSet) {
	io.WriteString(w, usageText)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// runCheckConfig is `zulip-acp check-config [-config F]`. It runs
// config.CheckFile, the same validation a start and a reload use.
func runCheckConfig(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(checkConfigCmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultConfigPath(), "config file to check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "usage: zulip-acp %s [-config F]\n", checkConfigCmd)
		return 2
	}
	if _, err := config.CheckFile(*path, getenv); err != nil {
		fmt.Fprintf(stderr, "INVALID %s: %v\nDo not reload: fix the file and check again.\n", *path, err)
		return 1
	}
	fmt.Fprintf(stdout, "OK %s\n(The server-side checks — login, channel names, the delete bot's login — run at start.)\n", *path)
	return 0
}

// runConfigSchema is `zulip-acp config-schema [prefix]`.
func runConfigSchema(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprintf(stderr, "usage: zulip-acp %s [key-prefix]\n", configSchemaCmd)
		return 2
	}
	prefix := ""
	if len(args) == 1 {
		prefix = strings.TrimSpace(args[0])
	}
	if config.WriteSchema(stdout, prefix) == 0 {
		fmt.Fprintf(stderr, "no config key starts with %q\n", prefix)
		return 1
	}
	return 0
}

// gatedReload returns a reload that first checks the config file at
// path and refuses an invalid one, so the relay keeps running on the
// config it has instead of re-execing into a crash. With no config
// file there is nothing to check.
func gatedReload(path string, getenv func(string) string, reload func() error) func() error {
	return func() error {
		if err := checkForReload(path, getenv); err != nil {
			return err
		}
		return reload()
	}
}

// checkForReload is the reload gate. Its error is the text the caller
// shows the user.
func checkForReload(path string, getenv func(string) string) error {
	if path == "" {
		return nil
	}
	if _, err := config.CheckFile(path, getenv); err != nil {
		return fmt.Errorf("reload refused: config %s is invalid: %v. The relay keeps running on its current config. Fix the file, run `zulip-acp check-config -config %s`, then reload again", path, err, path)
	}
	return nil
}

// fallbackNotice is the text of a start on the last good config.
func fallbackNotice(path string, why error) string {
	return fmt.Sprintf("⚠️ zulip-acp started on the LAST GOOD copy of its config (%s), because %s is invalid: %v\n"+
		"Fix the file, check it with `zulip-acp check-config -config %s`, then reload. Until then, edits to the file have no effect.",
		config.LastGoodPath(path), path, why, path)
}

// reportFallback makes a start on the last good config loud: it logs
// the notice and sends it as a DM to the update owners, if any.
func reportFallback(ctx context.Context, send func(context.Context, []int64, string) (int64, error), owners []int64, notice string, logf func(string, ...any)) {
	logf("config: ERROR %s", notice)
	if len(owners) == 0 {
		return
	}
	if _, err := send(ctx, owners, notice); err != nil {
		logf("config: cannot DM the owners about the config fallback: %v", err)
	}
}

// readyToApply is the auto-update idle gate. An invalid config file
// holds the apply back, as a running turn does: the apply's reload
// would be refused, and acp-kit/autoupdate treats a failed reload as a
// bad release (it restores .prev and blocks the version). The cause is
// logged each time it holds an apply.
func readyToApply(path string, getenv func(string) string, idle func() bool, logf func(string, ...any)) func() bool {
	return func() bool {
		if !idle() {
			return false
		}
		if err := checkForReload(path, getenv); err != nil {
			logf("autoupdate: apply held: %v", err)
			return false
		}
		return true
	}
}
