// Package updater is this relay's half of `zulip-acp update`: the
// distribution facts that name THIS binary, and nothing else.
//
// The mechanism — GitHub REST API for the release AND the asset bytes,
// with a token when one can be discovered (the shape that worked while
// this repo was private and needs none now it is public), sha256
// verification against the release's checksums.txt, the ETXTBSY-safe
// atomic swap of
// a sibling temp file over the running binary, the Homebrew keg
// hand-off, and the refusal of an install we do not own — lives in
// github.com/kfet/distkit, shared with fir, harb, mintick and the other
// relays. It used to live here as internal/selfupdate; keeping a fourth
// copy of it was how the family ended up with four implementations each
// missing a different piece.
//
// What stays here is the four strings that are actually about zulip-acp,
// in one place so the binary and the generated install.sh cannot
// disagree about them (see the drift test next door).
package updater

import (
	"io"

	"github.com/kfet/distkit"
)

// Repo is the github "owner/name" releases are taken from.
const Repo = "kfet/zulip-acp"

// Binary is both the release-asset stem and the installed file name.
const Binary = "zulip-acp"

// RestartHint is the graceful recycle for this relay: SIGHUP makes it
// stop polling without deleting the Zulip event queue, drain in-flight
// turns, and re-exec the new on-disk binary IN PLACE — same PID, cursor
// carried over, nothing lost and nothing delivered twice. zulip-acp has
// no master/worker supervisor and needs none (docs/graceful-reload.md);
// `restart` is only for a unit-file change or a dead service, and it
// silently eats every message posted before the fresh queue registers.
const RestartHint = "systemctl --user reload zulip-acp"

// Config returns the distkit configuration for this binary. version is
// the compiled-in version from main.
func Config(version string) distkit.Config {
	return distkit.Config{
		Repo:        Repo,
		Binary:      Binary,
		AssetStem:   Binary,
		Version:     version,
		RestartHint: RestartHint,
	}
}

// Main implements the `zulip-acp update` subcommand and returns the
// process exit code. args is the argument list AFTER the subcommand
// word, and stdout/stderr are where progress and diagnostics go;
// passing all three explicitly rather than letting distkit reach for
// os.Args and the process's own writers is what keeps this drivable
// from a test.
//
// Exit codes are distkit's: 0 success (including "already up to date"),
// 1 the update failed or was refused, 2 the flags did not parse, and 3
// for a `-check` run that found a newer release — so a systemd timer or
// a fleet sweep can act on the result without parsing stdout.
func Main(args []string, version string, stdout, stderr io.Writer) int {
	cfg := Config(version)
	// Non-nil, always: distkit falls back to os.Args[2:] on a nil Args,
	// and this package's contract is that the caller says what the
	// arguments are.
	if args == nil {
		args = []string{}
	}
	cfg.Args = args
	cfg.Stdout, cfg.Stderr = stdout, stderr
	return distkit.Main(cfg)
}
