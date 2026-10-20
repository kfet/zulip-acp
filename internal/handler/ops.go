package handler

// Out-of-band operator commands: `!fir`, `!skills` and `!run`.
//
// They run a program on the relay host and post its raw output. No
// agent and no LLM is involved, so they still work when the agent is
// rate-limited, logged out or dead — which is when `!fir auth refresh`
// is wanted. They run outside the turn loop, in their own goroutine, so
// a turn in flight does not block them, and they never touch the live
// session: each run gets a fresh scratch directory as its cwd, which is
// removed afterwards.
//
// They are owner-only (Config.OpsOwners, the `update_owner_ids` of
// `!update`): `!fir logout` or a skill script can break the relay host
// for every conversation, not only the sender's.
//
// Every run has a hard timeout (the whole process group is killed) and
// an output cap, so one command cannot hang the relay or flood a topic.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/convo"
	"github.com/kfet/zulip-acp/internal/journal"
)

const (
	firVerb    = "fir"
	skillsVerb = "skills"
	runVerb    = "run"

	// DefaultOpsTimeout bounds one `!fir` / `!run` process.
	DefaultOpsTimeout = 2 * time.Minute
	// DefaultOpsOutputCap bounds the output shown, in code points.
	// With the stderr cap it keeps a reply well under Zulip's 10000.
	DefaultOpsOutputCap = 6000
	// opsStderrCap bounds the stderr shown for a failed run.
	opsStderrCap = 2000
	// skillScanDepth is how deep a skill directory is searched for
	// scripts: the directory itself, scripts/, and one level more.
	skillScanDepth = 3
)

// opsHelp is the `!help` text for the operator commands.
const opsHelp = "- `" + command.DisplaySigil + firVerb + " <args…>` — owner only: run the `fir` CLI on the relay host (no agent, no LLM; works when the agent is down). It runs in a scratch directory and cannot change the live session\n" +
	"- `" + command.DisplaySigil + skillsVerb + " [name]` — owner only: list skills that hold scripts, or one skill's scripts\n" +
	"- `" + command.DisplaySigil + runVerb + " <skill> [script] [args…]` — owner only: run a skill's script on the relay host (no agent, no LLM)\n"

// OpsResult is the outcome of one process run.
type OpsResult struct {
	Stdout, Stderr []byte
	// Code is the exit code; -1 when the process did not exit normally.
	Code int
	// Err is a failure to start, or the timeout.
	Err error
}

// OpsRunner runs argv in dir with a deadline. It is a seam for tests.
type OpsRunner func(ctx context.Context, dir string, argv []string) OpsResult

// execRunner is the real OpsRunner. The process gets its own process
// group, and the timeout kills the whole group, so a script's children
// die with it.
func execRunner(ctx context.Context, dir string, argv []string) OpsResult {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	res := OpsResult{Stdout: out.Bytes(), Stderr: errb.Bytes(), Code: cmd.ProcessState.ExitCode()}
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		res.Err = fmt.Errorf("killed after timeout")
	case err != nil && !errors.As(err, &ee):
		res.Err = err
	}
	return res
}

// opsArgs reports whether text is `!<verb> …` and returns the fields
// after the verb.
func opsArgs(text, verb string) ([]string, bool) {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	if !ok {
		return nil, false
	}
	f := strings.Fields(body)
	if len(f) == 0 || !strings.EqualFold(f[0], verb) {
		return nil, false
	}
	return f[1:], true
}

// opsEnabled reports whether the operator commands can work here.
// Like `!update`, they need an owner.
func (h *Handler) opsEnabled() bool { return len(h.cfg.FirCmd) > 0 && len(h.cfg.OpsOwners) > 0 }

// opsVerbs are the owner-only verbs, in the order opsFilter tries them.
var opsVerbs = []string{firVerb, skillsVerb, runVerb}

// opsFilter is the before-filter for `!fir`, `!skills` and `!run`.
func (h *Handler) opsFilter(ctx context.Context, in *convo.In) convo.Verdict {
	if !h.opsEnabled() {
		return convo.Pass
	}
	md := metaOf(in)
	key := md.key
	if !slices.Contains(h.cfg.OpsOwners, md.m.SenderID) {
		for _, v := range opsVerbs {
			if _, ok := opsArgs(in.Text, v); ok {
				// The refusal style of `!update` (acp-kit/update).
				h.reply(ctx, key, "⛔ `"+command.DisplaySigil+v+"` is owner-only on this relay.")
				return convo.Handled
			}
		}
		return convo.Pass
	}
	if args, ok := opsArgs(in.Text, firVerb); ok {
		if len(args) == 0 {
			h.reply(ctx, key, "Usage: `"+command.DisplaySigil+firVerb+" <args…>`, e.g. `"+command.DisplaySigil+firVerb+" auth refresh`.")
			return convo.Handled
		}
		h.goOps(ctx, key, func(ctx context.Context) string { return h.firCommand(ctx, args) })
		return convo.Handled
	}
	if args, ok := opsArgs(in.Text, skillsVerb); ok {
		h.goOps(ctx, key, func(ctx context.Context) string { return h.skillsCommand(ctx, args) })
		return convo.Handled
	}
	if args, ok := opsArgs(in.Text, runVerb); ok {
		if len(args) == 0 {
			h.reply(ctx, key, "Usage: `"+command.DisplaySigil+runVerb+" <skill> [script] [args…]`. `"+command.DisplaySigil+skillsVerb+"` lists them.")
			return convo.Handled
		}
		h.goOps(ctx, key, func(ctx context.Context) string { return h.runCommand(ctx, args) })
		return convo.Handled
	}
	return convo.Pass
}

// goOps runs fn off the event loop and posts its reply. The context is
// detached from the event that started it: a command is not cancelled
// because the event loop moved on.
func (h *Handler) goOps(ctx context.Context, key journal.Key, fn func(context.Context) string) {
	ctx = context.WithoutCancel(ctx)
	h.ops.Add(1)
	go func() {
		defer h.ops.Done()
		// One deadline for the whole command, so `!skills` and its
		// many fir calls cannot together outlast the timeout.
		run, cancel := context.WithTimeout(ctx, h.cfg.OpsTimeout)
		defer cancel()
		h.reply(ctx, key, fn(run))
	}()
}

// opsRun executes argv in a fresh scratch directory under the timeout.
func (h *Handler) opsRun(ctx context.Context, argv []string) OpsResult {
	dir, err := os.MkdirTemp(h.cfg.Sessions.StateDir(), "ops-")
	if err != nil {
		return OpsResult{Code: -1, Err: fmt.Errorf("scratch dir: %w", err)}
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, h.cfg.OpsTimeout)
	defer cancel()
	return h.cfg.OpsRunner(ctx, dir, argv)
}

// firCommand runs `fir <args…>`.
func (h *Handler) firCommand(ctx context.Context, args []string) string {
	argv := append(slices.Clone(h.cfg.FirCmd), args...)
	return h.formatRun("fir "+strings.Join(args, " "), h.opsRun(ctx, argv))
}

// formatRun renders a run as a header line and a tagged code block.
// A failed run shows its capped stderr too.
func (h *Handler) formatRun(title string, r OpsResult) string {
	var sb strings.Builder
	status := fmt.Sprintf("exit %d", r.Code)
	if r.Err != nil {
		status = r.Err.Error()
	}
	ok := r.Err == nil && r.Code == 0
	mark := "✅"
	if !ok {
		mark = "❌"
	}
	fmt.Fprintf(&sb, "%s `%s` — %s\n", mark, title, status)
	out := string(r.Stdout)
	if ok && strings.TrimSpace(out) == "" {
		// Many CLIs report on stderr; with nothing else to show, show it.
		out = string(r.Stderr)
	}
	if strings.TrimSpace(out) != "" {
		sb.WriteString(codeBlock("stdout", capText(out, h.cfg.OpsOutputCap)))
	}
	if !ok && strings.TrimSpace(string(r.Stderr)) != "" {
		sb.WriteString(codeBlock("stderr", capText(string(r.Stderr), opsStderrCap)))
	}
	return sb.String()
}

// capText keeps the first max code points of s and says how much was
// cut.
func capText(s string, max int) string {
	n := utf8.RuneCountInString(s)
	if n <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + fmt.Sprintf("\n… (%d more characters cut)", n-max)
}

// codeBlock wraps s in a fenced block tagged tag. The fence is longer
// than any backtick run in s, so output can never close it early.
func codeBlock(tag, s string) string {
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + tag + "\n" + strings.TrimRight(s, "\n") + "\n" + fence + "\n"
}

// skillInfo is one skill that holds scripts.
type skillInfo struct {
	name    string
	dir     string
	scripts []string // relative to dir, sorted
}

// discoverSkills asks fir for its skills, so they resolve exactly the
// way a fir session resolves them (the builtin cache directory changes
// with every fir version), and keeps those whose directory holds at
// least one script.
func (h *Handler) discoverSkills(ctx context.Context) ([]skillInfo, error) {
	list := h.opsRun(ctx, append(slices.Clone(h.cfg.FirCmd), "skills", "list"))
	if list.Err != nil || list.Code != 0 {
		return nil, fmt.Errorf("`fir skills list` failed: %s", strings.TrimSpace(h.formatRun("fir skills list", list)))
	}
	var out []skillInfo
	seen := map[string]bool{}
	lines := strings.Split(string(list.Stdout), "\n")
	for _, line := range lines[1:] { // lines[0] is the header
		f := strings.Fields(line)
		if len(f) < 2 || seen[f[1]] {
			continue
		}
		seen[f[1]] = true
		p := h.opsRun(ctx, append(slices.Clone(h.cfg.FirCmd), "skills", "show", f[0], "--path"))
		path := strings.TrimSpace(string(p.Stdout))
		if p.Err != nil || p.Code != 0 || path == "" {
			continue
		}
		dir := filepath.Dir(path)
		if scripts := findScripts(dir); len(scripts) > 0 {
			out = append(out, skillInfo{name: f[1], dir: dir, scripts: scripts})
		}
	}
	return out, nil
}

// interpreters maps a script extension to the program that runs a
// script without its executable bit.
var interpreters = map[string]string{
	".sh":   "bash",
	".bash": "bash",
	".py":   "python3",
	".js":   "node",
	".mjs":  "node",
	".rb":   "ruby",
	".pl":   "perl",
}

// isScript is the mechanical test: an executable regular file, or one
// with a known script extension.
func isScript(path string, mode fs.FileMode) bool {
	if !mode.IsRegular() {
		return false
	}
	_, known := interpreters[filepath.Ext(path)]
	return known || mode.Perm()&0o111 != 0
}

// findScripts lists the scripts under dir, relative to it. Hidden
// entries are skipped.
func findScripts(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if strings.Count(rel, string(filepath.Separator)) >= skillScanDepth-1 {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Stat(p)
		if err == nil && isScript(p, info.Mode()) {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

// skillsCommand answers `!skills [name]`.
func (h *Handler) skillsCommand(ctx context.Context, args []string) string {
	skills, err := h.discoverSkills(ctx)
	if err != nil {
		return "❌ " + err.Error()
	}
	s := command.DisplaySigil
	if len(args) > 0 {
		sk, ok := findSkill(skills, args[0])
		if !ok {
			return fmt.Sprintf("No skill `%s` with scripts. `%s%s` lists them.", args[0], s, skillsVerb)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "**%s** — `%s`\n", sk.name, sk.dir)
		for _, sc := range sk.scripts {
			fmt.Fprintf(&sb, "- `%s%s %s %s`\n", s, runVerb, sk.name, sc)
		}
		return sb.String()
	}
	if len(skills) == 0 {
		return "No skill holds a script."
	}
	var sb strings.Builder
	sb.WriteString("**Skills with scripts**\n")
	for _, sk := range skills {
		fmt.Fprintf(&sb, "- `%s` — %d script(s)\n", sk.name, len(sk.scripts))
	}
	fmt.Fprintf(&sb, "\n`%s%s <name>` lists a skill's scripts; `%s%s <skill> [script] [args…]` runs one.\n", s, skillsVerb, s, runVerb)
	return sb.String()
}

// findSkill looks a skill up by name.
func findSkill(skills []skillInfo, name string) (skillInfo, bool) {
	for _, sk := range skills {
		if sk.name == name {
			return sk, true
		}
	}
	return skillInfo{}, false
}

// pickScript chooses the script `!run` names. A script may be given by
// its relative path or its base name, with or without the extension.
// A skill with ONE script needs no name: every argument goes to it.
func pickScript(sk skillInfo, args []string) (script string, rest []string, err error) {
	if len(args) > 0 {
		var hits []string
		for _, sc := range sk.scripts {
			base := filepath.Base(sc)
			if args[0] == sc || args[0] == base || args[0] == strings.TrimSuffix(base, filepath.Ext(base)) {
				hits = append(hits, sc)
			}
		}
		if len(hits) == 1 {
			return hits[0], args[1:], nil
		}
		if len(hits) > 1 {
			return "", nil, fmt.Errorf("`%s` matches more than one script: `%s`", args[0], strings.Join(hits, "`, `"))
		}
	}
	if len(sk.scripts) == 1 {
		return sk.scripts[0], args, nil
	}
	return "", nil, fmt.Errorf("skill `%s` has several scripts; name one: `%s`", sk.name, strings.Join(sk.scripts, "`, `"))
}

// runCommand answers `!run <skill> [script] [args…]`.
func (h *Handler) runCommand(ctx context.Context, args []string) string {
	skills, err := h.discoverSkills(ctx)
	if err != nil {
		return "❌ " + err.Error()
	}
	sk, ok := findSkill(skills, args[0])
	if !ok {
		return fmt.Sprintf("No skill `%s` with scripts. `%s%s` lists them.", args[0], command.DisplaySigil, skillsVerb)
	}
	script, rest, err := pickScript(sk, args[1:])
	if err != nil {
		return "❌ " + err.Error()
	}
	path := filepath.Join(sk.dir, filepath.FromSlash(script))
	argv := []string{path}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o111 == 0 {
		argv = []string{interpreters[filepath.Ext(path)], path}
	}
	argv = append(argv, rest...)
	title := strings.TrimSpace(sk.name + " " + script + " " + strings.Join(rest, " "))
	return h.formatRun("run "+title, h.opsRun(ctx, argv))
}
