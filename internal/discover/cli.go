package discover

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/kfet/distkit"
)

// Deps is what the commands need from outside this package. Production
// passes Default; tests pass fakes.
type Deps struct {
	Sys Sys
	// Config returns the distkit configuration for a binary at version.
	Config func(version string) distkit.Config
	// Latest resolves the release tag `update` would install.
	Latest func(ctx context.Context, cfg distkit.Config) (string, error)
	// Swap is distkit.Update: verify, then atomically replace one binary.
	Swap func(ctx context.Context, cfg distkit.Config) (distkit.Result, error)
	// Legacy is the plain single-binary `update` (distkit.Main), used for
	// -check and -restart-cmd so both keep their exact behaviour.
	Legacy func(args []string, stdout, stderr io.Writer) int
	// Interactive reports whether stdin is a terminal a human can answer.
	Interactive func() bool
}

// Default returns the production Deps around cfg.
func Default(cfg func(string) distkit.Config, legacy func([]string, io.Writer, io.Writer) int) Deps {
	return Deps{
		Sys:    OS(),
		Config: cfg,
		Latest: func(ctx context.Context, c distkit.Config) (string, error) {
			st, err := distkit.Check(ctx, c)
			if err != nil {
				return "", err
			}
			return st.Target, nil
		},
		Swap:   distkit.Update,
		Legacy: legacy,
		Interactive: func() bool {
			fi, err := os.Stdin.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		},
	}
}

// Status implements `zulip-acp status`. Exit codes: 0 everything current,
// 3 something is behind the latest release (the -check convention), 1 the
// latest release could not be resolved, 2 bad flags.
func Status(args []string, version string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("zulip-acp status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", deps.Config(version).Repo, "github owner/repo to compare against")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rep := Discover(deps.Sys)
	cfg := deps.Config(version)
	cfg.Repo, cfg.Stdout, cfg.Stderr = *repo, stdout, stderr
	latest, err := deps.Latest(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(stderr, "status: latest release: %v\n", err)
	}
	printReport(stdout, rep, latest)
	if err != nil {
		return 1
	}
	for _, b := range rep.Binaries {
		if !same(b.Version, latest) {
			return distkit.ExitUpdateAvailable
		}
	}
	for _, r := range rep.Relays {
		if r.PID > 0 && behind(r, latest) {
			return distkit.ExitUpdateAvailable
		}
	}
	return 0
}

// behind reports whether a running relay needs a reload to reach tag. An
// image whose version cannot be read is left alone.
func behind(r Relay, tag string) bool {
	return r.Stale || (tag != "" && r.Running != Unknown && !same(r.Running, tag))
}

func parseExit(err error) int {
	if err == flag.ErrHelp {
		return 0
	}
	return 2
}

func same(v, tag string) bool {
	return tag != "" && distkit.EnsureV(v) == distkit.EnsureV(tag)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printReport(w io.Writer, rep Report, latest string) {
	fmt.Fprintf(w, "latest release: %s\n\n", dash(latest))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(rep.Relays) == 0 {
		fmt.Fprintln(w, "relays: none found on this host")
	} else {
		fmt.Fprintln(tw, "MANAGER\tUNIT\tPID\tRUNNING\tBINARY\tCONFIG\tSTATE DIR")
		for _, r := range rep.Relays {
			pid, run := "-", "stopped"
			if r.PID > 0 {
				pid, run = strconv.Itoa(r.PID), r.Running
				if r.Stale {
					run += " (binary replaced; reload pending)"
				} else if behind(r, latest) {
					run += " (behind)"
				}
				if r.Self {
					run += " (this session)"
				}
			}
			mgr := r.Manager
			if r.Scope != "" {
				mgr += "/" + r.Scope
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				mgr, dash(r.Unit), pid, run, dash(r.Binary), dash(r.Config), dash(r.StateDir))
		}
		tw.Flush()
	}
	fmt.Fprintln(w)
	if len(rep.Binaries) == 0 {
		fmt.Fprintln(w, "binaries: none found on PATH or in ~/.local/bin")
		return
	}
	fmt.Fprintln(tw, "BINARY\tVERSION\tSTATUS")
	for _, b := range rep.Binaries {
		st := "unknown"
		switch {
		case same(b.Version, latest):
			st = "current"
		case latest != "":
			st = "update available → " + latest
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Path, b.Version, st)
	}
	tw.Flush()
}

// reloadStep is one relay to recycle after the swaps.
type reloadStep struct {
	r      Relay
	needs  string // the binary whose swap this reload depends on; "" none
	reason string
}

// Update implements `zulip-acp update`. With -check or -restart-cmd it is
// the plain single-binary distkit update, unchanged. Otherwise it
// discovers every install on this host, prints a plan, asks once, swaps
// each distinct binary and reloads each running relay gracefully.
func Update(args []string, version string, stdin io.Reader, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("zulip-acp update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := deps.Config(version)
	check := fs.Bool("check", false, "report whether an update is available for this binary; install nothing")
	pin := fs.String("version", "", "install a specific version (e.g. v0.19.0); default: latest")
	repo := fs.String("repo", base.Repo, "github owner/repo to update from")
	restart := fs.String("restart-cmd", "", "update only this binary, then run this shell command (no discovery)")
	yes := fs.Bool("y", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	// With no terminal and no -y nobody can answer the plan: a script
	// (converge.sh's `update --version vX` over ssh) gets the plain
	// single-binary update it always got.
	if *check || *restart != "" || (!*yes && !deps.Interactive()) {
		return deps.Legacy(withoutYes(args), stdout, stderr)
	}

	ctx := context.Background()
	rep := Discover(deps.Sys)
	cfg := deps.Config(version)
	cfg.Repo, cfg.TargetVersion, cfg.Stdout, cfg.Stderr = *repo, *pin, stdout, stderr
	target, err := deps.Latest(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "update: resolve release: %v\n", err)
		return 1
	}
	target = distkit.EnsureV(target)

	var swaps []Install
	var skipped []string
	swapSet := map[string]bool{}
	for _, b := range rep.Binaries {
		switch {
		case same(b.Version, target):
		case b.Version == Unknown:
			skipped = append(skipped, b.Path+" does not report a version")
		case distkit.IsDevBuild(b.Version) && *pin == "":
			skipped = append(skipped, b.Path+" is a dev build ("+b.Version+"); pin -version to replace it")
		default:
			swaps = append(swaps, b)
			swapSet[b.Path] = true
		}
	}
	var reloads []reloadStep
	var stopped []Relay
	for _, r := range rep.Relays {
		switch {
		case r.PID == 0:
			if swapSet[r.Binary] {
				stopped = append(stopped, r)
			}
		case swapSet[r.Binary]:
			reloads = append(reloads, reloadStep{r, r.Binary, "binary " + r.Binary + " is swapped"})
		case behind(r, target):
			reloads = append(reloads, reloadStep{r, "", "running " + r.Running + ", disk is newer"})
		}
	}
	// The relay that runs this command reloads last: its drain waits for
	// the current turn, and that turn is the one printing this report.
	sort.SliceStable(reloads, func(a, b int) bool { return !reloads[a].r.Self && reloads[b].r.Self })

	for _, n := range skipped {
		fmt.Fprintf(stdout, "skip    %s\n", n)
	}
	if len(swaps) == 0 && len(reloads) == 0 {
		fmt.Fprintf(stdout, "nothing to do: target is %s.\n", target)
		return 0
	}
	fmt.Fprintf(stdout, "target release: %s\n\nplan:\n", target)
	for _, b := range swaps {
		fmt.Fprintf(stdout, "  swap    %s  %s → %s\n", b.Path, b.Version, target)
	}
	for _, s := range reloads {
		fmt.Fprintf(stdout, "  reload  %s  (%s)\n", describe(s.r), s.reason)
	}
	for _, r := range stopped {
		fmt.Fprintf(stdout, "  note    %s is stopped; it starts on the new binary\n", describe(r))
	}
	if !*yes && !confirm(stdin, stdout) {
		fmt.Fprintln(stdout, "nothing was changed. (run with -y to skip this question)")
		return 1
	}

	rc := 0
	failed := map[string]bool{}
	for _, b := range swaps {
		c := deps.Config(b.Version)
		c.Repo, c.TargetVersion, c.Stdout, c.Stderr = *repo, *pin, stdout, stderr
		path := b.Path
		c.ExecPath = func() (string, error) { return path, nil }
		if _, err := deps.Swap(ctx, c); err != nil {
			fmt.Fprintf(stderr, "update %s: %v\n", path, err)
			failed[path] = true
			rc = 1
		}
	}
	for _, s := range reloads {
		if failed[s.needs] {
			fmt.Fprintf(stdout, "skip    %s: its binary was not swapped\n", describe(s.r))
			continue
		}
		if err := reload(deps.Sys, s.r, stdout); err != nil {
			fmt.Fprintf(stderr, "reload %s: %v\n", describe(s.r), err)
			rc = 1
		}
	}
	return rc
}

func withoutYes(args []string) []string {
	var out []string
	for _, a := range args {
		switch a {
		case "-y", "--y", "-y=true", "--y=true", "-y=false", "--y=false":
		default:
			out = append(out, a)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func describe(r Relay) string {
	s := r.Manager
	if r.Unit != "" {
		s += " " + r.Unit
	}
	if r.PID > 0 {
		s += " pid " + strconv.Itoa(r.PID)
	}
	return s
}

// confirm asks once. No terminal (an agent's tool call) reads EOF, and
// EOF is "no".
func confirm(stdin io.Reader, stdout io.Writer) bool {
	fmt.Fprint(stdout, "\nproceed? [y/N] ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	fmt.Fprintln(stdout)
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// reload recycles one relay gracefully. Every path ends in SIGHUP to the
// relay: it stops polling without deleting its event queue, drains the
// in-flight turns, and re-execs the new binary in place (same PID). A
// hard restart (`launchctl kickstart -k`, `systemctl restart`) is never
// used: it loses every message posted before the fresh queue registers.
func reload(sys Sys, r Relay, w io.Writer) error {
	if r.Manager == Systemd {
		if !r.CanReload {
			return fmt.Errorf("unit has no ExecReload; add `ExecReload=/bin/kill -HUP $MAINPID`, daemon-reload, then reload it")
		}
		args := append(scopeArgs(r.Scope), "reload", r.Unit)
		fmt.Fprintf(w, "reload  systemctl %s\n", strings.Join(args, " "))
		if _, err := sys.Run("systemctl", args...); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "reload  kill -HUP %d (%s)\n", r.PID, describe(r))
		if err := sys.Signal(r.PID, syscall.SIGHUP); err != nil {
			return err
		}
	}
	if r.Self {
		fmt.Fprintln(w, "        this command runs inside that relay: it re-execs after the current turn ends.")
	}
	return nil
}
