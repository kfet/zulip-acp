package handler

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOps is a scripted OpsRunner. It records every argv and the
// directory it ran in, and checks that the directory exists then.
type fakeOps struct {
	mu    sync.Mutex
	calls [][]string
	dirs  []string
	fn    func(argv []string) OpsResult
}

func (f *fakeOps) run(_ context.Context, dir string, argv []string) OpsResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := os.Stat(dir); err != nil {
		panic("scratch dir missing during run: " + err.Error())
	}
	f.calls = append(f.calls, slices.Clone(argv))
	f.dirs = append(f.dirs, dir)
	return f.fn(argv)
}

func (f *fakeOps) last() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func opsHarness(t *testing.T, fn func(argv []string) OpsResult) (*harness, *fakeOps) {
	t.Helper()
	f := &fakeOps{fn: fn}
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) {
		c.FirCmd = []string{"fir", "--agent-dir", "/d"}
		c.OpsRunner = f.run
	})
	return hh, f
}

// ops sends a command and waits for its goroutine to post.
func (hh *harness) ops(t *testing.T, text string) string {
	t.Helper()
	hh.z.reset()
	hh.deliverDM(t, humanID, text, humanID, botID)
	hh.h.ops.Wait()
	return hh.only(t)
}

func ok(out string) OpsResult { return OpsResult{Stdout: []byte(out)} }

func TestFirRunsTheCLIInAScratchDir(t *testing.T) {
	hh, f := opsHarness(t, func([]string) OpsResult { return ok("refreshed\n") })
	got := hh.ops(t, "!fir auth refresh")
	if want := []string{"fir", "--agent-dir", "/d", "auth", "refresh"}; !slices.Equal(f.last(), want) {
		t.Fatalf("argv = %q, want %q", f.last(), want)
	}
	if !strings.HasPrefix(f.dirs[0], hh.s.dir) {
		t.Fatalf("dir %q is not under the state dir", f.dirs[0])
	}
	if _, err := os.Stat(f.dirs[0]); !os.IsNotExist(err) {
		t.Fatalf("scratch dir not removed: %v", err)
	}
	if want := "✅ `fir auth refresh` — exit 0\n```stdout\nrefreshed\n```\n"; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
	if len(hh.a.prompted()) != 0 {
		t.Fatal("the agent was prompted")
	}
}

func TestFirUsage(t *testing.T) {
	hh, _ := opsHarness(t, nil)
	if got := hh.ops(t, "!fir"); !strings.Contains(got, "Usage") {
		t.Fatalf("reply = %q", got)
	}
}

func TestFirFailureShowsStderr(t *testing.T) {
	hh, _ := opsHarness(t, func([]string) OpsResult {
		return OpsResult{Stdout: []byte("partial"), Stderr: []byte("bad ``` thing"), Code: 2}
	})
	got := hh.ops(t, "!fir nope")
	for _, want := range []string{"❌ `fir nope` — exit 2", "```stdout\npartial\n```", "````stderr\nbad ``` thing\n````"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reply %q is missing %q", got, want)
		}
	}
}

func TestFirSuccessWithOnlyStderrShowsIt(t *testing.T) {
	hh, _ := opsHarness(t, func([]string) OpsResult { return OpsResult{Stderr: []byte("note")} })
	if got := hh.ops(t, "!fir x"); !strings.Contains(got, "```stdout\nnote\n```") {
		t.Fatalf("reply = %q", got)
	}
}

func TestFirRunErrorIsReported(t *testing.T) {
	hh, _ := opsHarness(t, func([]string) OpsResult {
		return OpsResult{Code: -1, Err: context.DeadlineExceeded}
	})
	if got := hh.ops(t, "!fir x"); got != "❌ `fir x` — context deadline exceeded\n" {
		t.Fatalf("reply = %q", got)
	}
}

func TestFirScratchDirFailure(t *testing.T) {
	hh, f := opsHarness(t, nil)
	hh.s.mu.Lock()
	hh.s.dir = filepath.Join(t.TempDir(), "missing", "deeper")
	hh.s.mu.Unlock()
	if got := hh.ops(t, "!fir x"); !strings.Contains(got, "scratch dir") {
		t.Fatalf("reply = %q", got)
	}
	if len(f.calls) != 0 {
		t.Fatal("ran without a scratch dir")
	}
}

func TestOpsDisabledWithoutFir(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!fir x", humanID, botID)
	if !strings.Contains(panelBody(t, hh), "Unknown command `!fir`") {
		t.Fatalf("panel = %q", panelBody(t, hh))
	}
	hh.z.reset()
	if out := hh.h.decorate("!help", "- `!help` — x\n- `!skills`\n"); strings.Contains(out, opsHelp) || !strings.Contains(out, "`!skills`") {
		t.Fatalf("help = %q", out)
	}
}

func TestOpsHelpReplacesTheSkillsPassthrough(t *testing.T) {
	hh, _ := opsHarness(t, nil)
	out := hh.h.decorate("!help", "- `!help` — x\n\nAgent commands:\n\n- `!skills`\n- `!reload`\n")
	if !strings.Contains(out, opsHelp) || strings.Contains(out, "- `!skills`\n") || !strings.Contains(out, "- `!reload`\n") {
		t.Fatalf("help = %q", out)
	}
}

func TestCapText(t *testing.T) {
	if got := capText("héllo", 10); got != "héllo" {
		t.Fatalf("got %q", got)
	}
	if got := capText("héllo", 2); got != "hé\n… (3 more characters cut)" {
		t.Fatalf("got %q", got)
	}
}

func TestFirOutputIsCapped(t *testing.T) {
	f := &fakeOps{fn: func([]string) OpsResult { return ok(strings.Repeat("x", 50)) }}
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) {
		c.FirCmd = []string{"fir"}
		c.OpsRunner = f.run
		c.OpsOutputCap = 10
	})
	if got := hh.ops(t, "!fir x"); !strings.Contains(got, "(40 more characters cut)") {
		t.Fatalf("reply = %q", got)
	}
}

// --- skills ----------------------------------------------------------

// skillTree builds skill directories and a fake fir that reports them.
func skillTree(t *testing.T) (func(argv []string) OpsResult, map[string]string) {
	t.Helper()
	root := t.TempDir()
	write := func(rel string, mode os.FileMode) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("alpha/SKILL.md", 0o644)
	write("alpha/scripts/usage.sh", 0o644)
	write("alpha/scripts/.hidden.sh", 0o755)
	write("alpha/.git/hook.sh", 0o755)
	write("alpha/tool", 0o755)
	write("alpha/notes.txt", 0o644)
	write("alpha/a/b/deep.sh", 0o755)
	write("alpha/a/b/c/too-deep.sh", 0o755)
	if err := os.Symlink(filepath.Join(root, "alpha", "a"), filepath.Join(root, "alpha", "dirlink.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "alpha", "dangling.sh")); err != nil {
		t.Fatal(err)
	}
	write("beta/SKILL.md", 0o644)
	write("gamma/SKILL.md", 0o644)
	write("gamma/run.py", 0o644)
	write("dup/SKILL.md", 0o644)
	write("dup/x.sh", 0o644)
	write("dup/x.py", 0o644)
	paths := map[string]string{
		"id_alpha": filepath.Join(root, "alpha", "SKILL.md"),
		"id_beta":  filepath.Join(root, "beta", "SKILL.md"),
		"id_gamma": filepath.Join(root, "gamma", "SKILL.md"),
		"id_dup":   filepath.Join(root, "dup", "SKILL.md"),
		"id_gone":  filepath.Join(root, "gone", "SKILL.md"),
	}
	list := "ID NAME SOURCE DESCRIPTION\n" +
		"id_alpha alpha built-in Alpha\n" +
		"id_alpha2 alpha user Shadowed\n" +
		"id_beta beta user Beta\n" +
		"id_gamma gamma user Gamma\n" +
		"id_dup dup user Dup\n" +
		"id_gone gone user Gone\n" +
		"id_fail fail user Fail\n" +
		"short\n"
	return func(argv []string) OpsResult {
		switch {
		case slices.Equal(argv[1:], []string{"skills", "list"}):
			return ok(list)
		case len(argv) == 5 && argv[2] == "show":
			if p, found := paths[argv[3]]; found {
				return ok(p + "\n")
			}
			return OpsResult{Code: 1, Stderr: []byte("no such skill")}
		}
		return ok("ran " + strings.Join(argv, " "))
	}, paths
}

func skillsHarness(t *testing.T) (*harness, *fakeOps, map[string]string) {
	t.Helper()
	fn, paths := skillTree(t)
	f := &fakeOps{fn: fn}
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) {
		c.FirCmd = []string{"fir"}
		c.OpsRunner = f.run
	})
	return hh, f, paths
}

func TestSkillsListsOnlySkillsWithScripts(t *testing.T) {
	hh, _, _ := skillsHarness(t)
	got := hh.ops(t, "!skills")
	for _, want := range []string{"`alpha` — 3 script(s)", "`gamma` — 1 script(s)", "`dup` — 2 script(s)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reply %q is missing %q", got, want)
		}
	}
	for _, bad := range []string{"beta", "gone", "fail", "short"} {
		if strings.Contains(got, "`"+bad+"`") {
			t.Fatalf("reply %q lists %s", got, bad)
		}
	}
}

func TestSkillsShowsOneSkill(t *testing.T) {
	hh, _, paths := skillsHarness(t)
	got := hh.ops(t, "!skills alpha")
	want := "**alpha** — `" + filepath.Dir(paths["id_alpha"]) + "`\n" +
		"- `!run alpha a/b/deep.sh`\n- `!run alpha scripts/usage.sh`\n- `!run alpha tool`\n"
	if got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
	if got := hh.ops(t, "!skills nope"); !strings.Contains(got, "No skill `nope`") {
		t.Fatalf("reply = %q", got)
	}
}

func TestSkillsNoneHaveScripts(t *testing.T) {
	hh, _ := opsHarness(t, func([]string) OpsResult { return ok("ID NAME\n") })
	if got := hh.ops(t, "!skills"); got != "No skill holds a script." {
		t.Fatalf("reply = %q", got)
	}
}

func TestSkillsListFailure(t *testing.T) {
	hh, _ := opsHarness(t, func([]string) OpsResult { return OpsResult{Code: 1, Stderr: []byte("dead")} })
	if got := hh.ops(t, "!skills"); !strings.HasPrefix(got, "❌ `fir skills list` failed") {
		t.Fatalf("reply = %q", got)
	}
	if got := hh.ops(t, "!run x"); !strings.HasPrefix(got, "❌ `fir skills list` failed") {
		t.Fatalf("reply = %q", got)
	}
}

func TestRunUsage(t *testing.T) {
	hh, _ := opsHarness(t, nil)
	if got := hh.ops(t, "!run"); !strings.Contains(got, "Usage") {
		t.Fatalf("reply = %q", got)
	}
}

func TestRunPicksAScript(t *testing.T) {
	hh, f, paths := skillsHarness(t)
	gamma := filepath.Dir(paths["id_gamma"])
	alpha := filepath.Dir(paths["id_alpha"])
	for _, tc := range []struct {
		cmd  string
		argv []string
	}{
		// One script: every argument is the script's.
		{"!run gamma a b", []string{"python3", filepath.Join(gamma, "run.py"), "a", "b"}},
		{"!run gamma run", []string{"python3", filepath.Join(gamma, "run.py")}},
		{"!run alpha usage --x", []string{"bash", filepath.Join(alpha, "scripts", "usage.sh"), "--x"}},
		{"!run alpha tool", []string{filepath.Join(alpha, "tool")}},
		{"!run alpha a/b/deep.sh", []string{filepath.Join(alpha, "a", "b", "deep.sh")}},
	} {
		got := hh.ops(t, tc.cmd)
		if !slices.Equal(f.last(), tc.argv) {
			t.Fatalf("%s: argv = %q, want %q", tc.cmd, f.last(), tc.argv)
		}
		if !strings.HasPrefix(got, "✅ `run ") {
			t.Fatalf("%s: reply = %q", tc.cmd, got)
		}
	}
}

func TestRunRefusals(t *testing.T) {
	hh, _, _ := skillsHarness(t)
	for cmd, want := range map[string]string{
		"!run nope":      "No skill `nope`",
		"!run alpha":     "has several scripts",
		"!run alpha zzz": "has several scripts",
		"!run dup x":     "matches more than one script",
	} {
		if got := hh.ops(t, cmd); !strings.Contains(got, want) {
			t.Fatalf("%s: reply = %q, want %q", cmd, got, want)
		}
	}
}

// --- the real runner -------------------------------------------------

func TestExecRunner(t *testing.T) {
	dir := t.TempDir()
	r := execRunner(context.Background(), dir, []string{"sh", "-c", "pwd; echo err >&2; exit 3"})
	if r.Err != nil || r.Code != 3 || strings.TrimSpace(string(r.Stderr)) != "err" {
		t.Fatalf("result = %+v", r)
	}
	if got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(r.Stdout))); got != mustEval(t, dir) {
		t.Fatalf("cwd = %q, want %q", got, dir)
	}
	r = execRunner(context.Background(), dir, []string{filepath.Join(dir, "missing")})
	if r.Err == nil || r.Code != -1 {
		t.Fatalf("result = %+v", r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r = execRunner(ctx, dir, []string{"sh", "-c", "sleep 30 & wait"})
	if r.Err == nil || r.Err.Error() != "killed after timeout" {
		t.Fatalf("result = %+v", r)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOpsArgsAndPass(t *testing.T) {
	if _, ok := opsArgs("fir x", firVerb); ok {
		t.Fatal("prose parsed as a command")
	}
	hh, f := opsHarness(t, nil)
	hh.deliverDM(t, humanID, "!status", humanID, botID)
	hh.h.ops.Wait()
	if len(f.calls) != 0 {
		t.Fatal("!status ran a process")
	}
}

// TestWaitIdleDrainsOps: a graceful reload must not cut a running
// `!fir` off before it has posted.
func TestWaitIdleDrainsOps(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	hh, _ := opsHarness(t, func([]string) OpsResult {
		close(entered)
		<-release
		return ok("done")
	})
	hh.h.Handle(context.Background(), dmEvent(humanID, "!fir x", humanID, botID))
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hh.h.WaitIdle(ctx); err == nil {
		t.Fatal("WaitIdle returned while a run was in flight")
	}
	close(release)
	if err := hh.h.WaitIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hh.z.lastBody(), "done") {
		t.Fatalf("reply = %q", hh.z.lastBody())
	}
}

// TestFirAnswersWhileATurnIsInFlight: the point of `!fir` is that it
// works when the agent does not.
func TestFirAnswersWhileATurnIsInFlight(t *testing.T) {
	agent := newAgent("x")
	agent.block = make(chan struct{})
	agent.entered = make(chan struct{}, 1)
	f := &fakeOps{fn: func([]string) OpsResult { return ok("alive") }}
	hh := dmCmdHarness(t, agent, func(c *Config) {
		c.FirCmd = []string{"fir"}
		c.OpsRunner = f.run
	})
	hh.h.Handle(context.Background(), dmEvent(humanID, "think hard", humanID, botID))
	<-agent.entered
	hh.h.Handle(context.Background(), dmEvent(humanID, "!fir auth refresh", humanID, botID))
	hh.h.ops.Wait()
	found := false
	for _, b := range hh.z.stored() {
		found = found || strings.Contains(b, "alive")
	}
	if !found {
		t.Fatalf("no !fir reply while the turn ran: %q", hh.z.stored())
	}
	close(agent.block)
	waitIdle(t, hh)
}
