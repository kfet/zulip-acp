package selfupdate

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
)

// RestartHint is the graceful recycle for this relay: SIGHUP makes it
// stop polling without deleting the Zulip event queue, drain in-flight
// turns, and re-exec the new on-disk binary IN PLACE — same PID, cursor
// carried over, nothing lost and nothing delivered twice. zulip-acp has
// no master/worker supervisor and needs none (docs/graceful-reload.md);
// `restart` is only for a unit-file change or a dead service, and it
// silently eats every message posted before the fresh queue registers.
const RestartHint = "systemctl --user reload zulip-acp"

// Main implements the `zulip-acp update` subcommand. args is the
// argument list AFTER the subcommand word. seed carries test overrides
// (API base, exec path, writers); production passes a zero Options and
// the flags fill it in. Returns the process exit code.
func Main(args []string, currentVersion string, seed Options) int {
	if seed.Stdout == nil {
		seed.Stdout = os.Stdout
	}
	if seed.Stderr == nil {
		seed.Stderr = os.Stderr
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(seed.Stderr)
	check := fs.Bool("check", false, "report whether an update is available; do not install")
	ver := fs.String("version", "", "install a specific version (e.g. v0.19.0); default: latest")
	repo := fs.String("repo", DefaultRepo, "github owner/repo to update from")
	restartCmd := fs.String("restart-cmd", "",
		"shell command to run after a successful update; prefer the graceful reload, e.g. \""+RestartHint+"\"")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	seed.Repo, seed.Version, seed.CheckOnly = *repo, *ver, *check
	res, err := Run(currentVersion, seed)
	if err != nil {
		fmt.Fprintln(seed.Stderr, "update:", err)
		return 1
	}
	if !res.Updated {
		return 0
	}

	// A swapped binary is inert until the relay re-execs: the running
	// process still has the old inode mapped.
	if *restartCmd == "" {
		fmt.Fprintf(seed.Stdout, "recycle the relay to run it:\n  %s\n", RestartHint)
		return 0
	}
	fmt.Fprintf(seed.Stdout, "recycling: %s\n", *restartCmd)
	c := exec.Command("sh", "-c", *restartCmd)
	c.Stdout, c.Stderr = seed.Stdout, seed.Stderr
	if err := c.Run(); err != nil {
		fmt.Fprintln(seed.Stderr, "recycle:", err)
		return 1
	}
	return 0
}
