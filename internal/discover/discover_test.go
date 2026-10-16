package discover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kfet/distkit"
)

type fakeSys struct {
	goos     string
	pid      int
	home     string
	env      map[string]string
	exe      string
	files    map[string]string
	links    map[string]string
	globs    map[string][]string
	execs    map[string]bool
	runs     map[string]string
	symlinks map[string]string
	ran      []string
	signals  []string
	sigErr   error
}

func (f *fakeSys) GOOS() string                    { return f.goos }
func (f *fakeSys) Getpid() int                     { return f.pid }
func (f *fakeSys) Getuid() int                     { return 501 }
func (f *fakeSys) Home() string                    { return f.home }
func (f *fakeSys) Getenv(k string) string          { return f.env[k] }
func (f *fakeSys) IsExec(p string) bool            { return f.execs[p] }
func (f *fakeSys) Glob(p string) ([]string, error) { return f.globs[p], nil }

func (f *fakeSys) Executable() (string, error) {
	if f.exe == "" {
		return "", errors.New("no exe")
	}
	return f.exe, nil
}

func (f *fakeSys) ReadFile(p string) ([]byte, error) {
	if s, ok := f.files[p]; ok {
		return []byte(s), nil
	}
	return nil, os.ErrNotExist
}

func (f *fakeSys) Readlink(p string) (string, error) {
	if s, ok := f.links[p]; ok {
		return s, nil
	}
	return "", os.ErrPermission
}

func (f *fakeSys) EvalSymlinks(p string) (string, error) {
	if s, ok := f.symlinks[p]; ok {
		return s, nil
	}
	if f.execs[p] {
		return p, nil
	}
	return "", os.ErrNotExist
}

func (f *fakeSys) Run(name string, args ...string) (string, error) {
	k := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.ran = append(f.ran, k)
	if s, ok := f.runs[k]; ok {
		return s, nil
	}
	return "", errors.New("exit 1: " + k)
}

func (f *fakeSys) Signal(pid int, s syscall.Signal) error {
	f.signals = append(f.signals, fmt.Sprintf("%d %v", pid, s))
	return f.sigErr
}

const (
	userUnits = "systemctl --user list-units --all --type=service --plain --no-legend --no-pager"
	userFiles = "systemctl --user list-unit-files --type=service --no-legend --no-pager"
	sysUnits  = "systemctl list-units --all --type=service --plain --no-legend --no-pager"
	sysFiles  = "systemctl list-unit-files --type=service --no-legend --no-pager"
)

// linuxHost: a systemd user unit running pid 100 (binary since replaced),
// a bare process pid 200 that ran this command, a stopped system unit, an
// `update` subcommand process, and a second binary on PATH.
func linuxHost() *fakeSys {
	bin := "/home/u/.local/bin/zulip-acp"
	return &fakeSys{
		goos: "linux", pid: 999, home: "/home/u",
		env: map[string]string{"PATH": "/usr/local/bin:/home/u/.local/bin"},
		exe: "/home/u/.local/bin/zulip-acp",
		globs: map[string][]string{
			"/proc/[0-9]*/exe": {"/proc/100/exe", "/proc/200/exe", "/proc/300/exe", "/proc/400/exe", "/proc/999/exe", "/proc/500/exe"},
		},
		links: map[string]string{
			"/proc/100/exe": bin + " (deleted)",
			"/proc/200/exe": "/opt/za/zulip-acp",
			"/proc/300/exe": bin,
			"/proc/400/exe": "/usr/bin/bash",
			"/proc/999/exe": bin,
		},
		files: map[string]string{
			"/proc/100/cmdline":                     bin + "\x00--config\x00/home/u/.config/zulip-acp/config.json\x00",
			"/proc/200/cmdline":                     "/opt/za/zulip-acp\x00-state-dir=/srv/st\x00",
			"/proc/300/cmdline":                     bin + "\x00update\x00",
			"/home/u/.config/zulip-acp/config.json": `{"state_dir":"/home/u/st"}`,
			"/proc/999/stat":                        "999 (zulip acp) S 150 1 1",
			"/proc/150/stat":                        "150 (fir) S 200 1 1",
			"/proc/200/stat":                        "200 (zulip-acp) S 1 1 1",
		},
		execs: map[string]bool{
			bin: true, "/opt/za/zulip-acp": true, "/usr/local/bin/zulip-acp": true,
		},
		symlinks: map[string]string{"/usr/local/bin/zulip-acp": "/usr/local/Cellar/zulip-acp/bin/zulip-acp"},
		runs: map[string]string{
			userUnits: "zulip-acp.service loaded active running relay\nfoo@.service x\n\n",
			userFiles: "zulip-acp.service enabled enabled\nother.service enabled\nnotes.txt\n",
			sysUnits:  "za-sys.service loaded inactive dead\n",
			"systemctl --user show -p Id,MainPID,ExecStart,ExecReload other.service zulip-acp.service": "Id=other.service\nMainPID=0\nExecStart=\n\n" +
				"Id=zulip-acp.service\nMainPID=100\nExecStart={ path=" + bin + " ; argv[]=" + bin + " --config /home/u/.config/zulip-acp/config.json ; ignore_errors=no }\nExecReload={ path=/bin/kill ; argv[]=/bin/kill -HUP $MAINPID }\n",
			"systemctl show -p Id,MainPID,ExecStart,ExecReload za-sys.service": "Id=za-sys.service\nMainPID=0\nExecStart={ path=/opt/za/zulip-acp ; argv[]=/opt/za/zulip-acp -config=/etc/za.json ; }\nExecReload=\n",
			"/proc/100/exe -version":                             "v1.0.0\n",
			"/proc/200/exe -version":                             "v1.1.0\n",
			bin + " -version":                                    "v1.1.0\n",
			"/opt/za/zulip-acp -version":                         "v1.1.0",
			"/usr/local/Cellar/zulip-acp/bin/zulip-acp -version": "\n",
		},
	}
}

func TestDiscoverLinux(t *testing.T) {
	rep := Discover(linuxHost())
	if len(rep.Relays) != 3 {
		t.Fatalf("relays = %+v", rep.Relays)
	}
	u, s, p := rep.Relays[0], rep.Relays[1], rep.Relays[2]
	if u.Manager != Systemd || u.Scope != "user" || u.PID != 100 || !u.Stale || u.Running != "v1.0.0" ||
		u.Config != "/home/u/.config/zulip-acp/config.json" || u.StateDir != "/home/u/st" || !u.CanReload || u.Self {
		t.Errorf("user unit = %+v", u)
	}
	if s.Manager != Systemd || s.Scope != "system" || s.PID != 0 || s.CanReload || s.Config != "/etc/za.json" || s.Running != "" {
		t.Errorf("system unit = %+v", s)
	}
	if p.Manager != Process || p.PID != 200 || !p.Self || p.StateDir != "/srv/st" || p.Running != "v1.1.0" {
		t.Errorf("process = %+v", p)
	}
	want := []Install{
		{"/home/u/.local/bin/zulip-acp", "v1.1.0"},
		{"/opt/za/zulip-acp", "v1.1.0"},
		{"/usr/local/Cellar/zulip-acp/bin/zulip-acp", "?"},
	}
	if fmt.Sprint(rep.Binaries) != fmt.Sprint(want) {
		t.Errorf("binaries = %v", rep.Binaries)
	}
}

func TestDiscoverLinuxNothing(t *testing.T) {
	rep := Discover(&fakeSys{goos: "linux"})
	if len(rep.Relays) != 0 || len(rep.Binaries) != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestSystemdShowFails(t *testing.T) {
	f := &fakeSys{goos: "linux", runs: map[string]string{sysUnits: "a.service x\n"}}
	if rs := (&discoverer{sys: f, versions: map[string]string{}}).systemdUnits("system"); rs != nil {
		t.Fatal(rs)
	}
}

const plistXML = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>EnvironmentVariables</key><dict><key>Label</key><string>nested</string></dict>
<key>ProgramArguments</key><array><string>%s</string><string>--config</string><string>/Users/u/za.json</string></array>
<key>KeepAlive</key><true/>
</dict></plist>`

func darwinHost() *fakeSys {
	agents := "/Users/u/Library/LaunchAgents/*.plist"
	return &fakeSys{
		goos: "darwin", pid: 900, home: "/Users/u",
		env: map[string]string{"PATH": "/opt/homebrew/bin"},
		exe: "/opt/homebrew/bin/zulip-acp",
		globs: map[string][]string{
			agents:                           {"/Users/u/Library/LaunchAgents/a.plist", "/Users/u/Library/LaunchAgents/b.plist", "/Users/u/Library/LaunchAgents/c.plist", "/Users/u/Library/LaunchAgents/d.plist"},
			"/Library/LaunchDaemons/*.plist": {"/Library/LaunchDaemons/sys.plist"},
		},
		execs: map[string]bool{"/opt/homebrew/bin/zulip-acp": true},
		symlinks: map[string]string{
			"/opt/homebrew/bin/zulip-acp":               "/opt/homebrew/Cellar/zulip-acp/1.0.0/bin/zulip-acp",
			"/opt/homebrew/opt/zulip-acp/bin/zulip-acp": "/opt/homebrew/Cellar/zulip-acp/1.0.0/bin/zulip-acp",
		},
		files: map[string]string{"/Users/u/za.json": `{}`},
		runs: map[string]string{
			"ps -axww -o pid=,ppid=,args=": "  PID PPID ARGS\n 10 1 /opt/homebrew/opt/zulip-acp/bin/zulip-acp --config /Users/u/za.json\n 20 1 zulip-acp\n 30 20 zulip-acp mcp-serve\n 900 50 zulip-acp status\n bad 1 x y\n 7 7\n",
			"ps -o ppid= -p 900":           "50\n",
			"ps -o ppid= -p 50":            " 20\n",
			"ps -o ppid= -p 20":            "1\n",
			"launchctl list":               "PID\tStatus\tLabel\n10\t0\thomebrew.mxcl.zulip-acp\n-\t0\tcom.x.stopped\n",
			"plutil -convert xml1 -o - /Users/u/Library/LaunchAgents/a.plist": fmt.Sprintf(plistXML, "homebrew.mxcl.zulip-acp", "/opt/homebrew/opt/zulip-acp/bin/zulip-acp"),
			"plutil -convert xml1 -o - /Users/u/Library/LaunchAgents/b.plist": fmt.Sprintf(plistXML, "com.other", "/usr/bin/other"),
			"plutil -convert xml1 -o - /Users/u/Library/LaunchAgents/c.plist": "<plist><dict><key>Label</key><string>com.x.stopped</string><key>Program</key><string>/usr/local/bin/zulip-acp</string></dict></plist>",
			"plutil -convert xml1 -o - /Library/LaunchDaemons/sys.plist":      "<plist><dict><key>Program</key><string>/x/zulip-acp</string></dict></plist>",
			"/opt/homebrew/Cellar/zulip-acp/1.0.0/bin/zulip-acp -version":     "v1.0.0\n",
		},
	}
}

func TestDiscoverDarwin(t *testing.T) {
	rep := Discover(darwinHost())
	if len(rep.Relays) != 3 {
		t.Fatalf("relays = %+v", rep.Relays)
	}
	a, c, p := rep.Relays[0], rep.Relays[1], rep.Relays[2]
	if a.Manager != Launchd || a.Unit != "homebrew.mxcl.zulip-acp" || a.PID != 10 || a.Running != "v1.0.0" ||
		a.Binary != "/opt/homebrew/Cellar/zulip-acp/1.0.0/bin/zulip-acp" || a.Config != "/Users/u/za.json" || a.Self {
		t.Errorf("brew agent = %+v", a)
	}
	if c.Unit != "com.x.stopped" || c.PID != 0 || c.Binary != "/usr/local/bin/zulip-acp" {
		t.Errorf("stopped agent = %+v", c)
	}
	if p.Manager != Process || p.PID != 20 || !p.Self || p.Binary != "/opt/homebrew/Cellar/zulip-acp/1.0.0/bin/zulip-acp" || p.Running != "v1.0.0" {
		t.Errorf("process = %+v", p)
	}
	if len(rep.Binaries) != 1 || rep.Binaries[0].Version != "v1.0.0" {
		t.Errorf("binaries = %+v", rep.Binaries)
	}
}

func TestDiscoverDarwinNoTools(t *testing.T) {
	f := &fakeSys{goos: "darwin", env: map[string]string{"PATH": "/a"}, runs: map[string]string{
		"ps -axww -o pid=,ppid=,args=": "5 1 zulip-acp\n",
		"ps -o ppid= -p 0":             "x",
	}}
	rep := Discover(f)
	if len(rep.Relays) != 1 || rep.Relays[0].Binary != "zulip-acp" {
		t.Fatalf("%+v", rep)
	}
}

func TestArgPaths(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		c, s string
	}{
		{[]string{"z", "-config", "a", "--state-dir", "b"}, "a", "b"},
		{[]string{"z", "--config=a", "-state-dir=b"}, "a", "b"},
		{[]string{"z", "x", "-agent-cmd", "fir", "-config"}, "", ""},
		{nil, "", ""},
	} {
		if c, s := argPaths(tc.argv); c != tc.c || s != tc.s {
			t.Errorf("%v: %q %q", tc.argv, c, s)
		}
	}
}

func TestLinuxParentShortStat(t *testing.T) {
	d := &discoverer{sys: &fakeSys{files: map[string]string{"/proc/1/stat": "1 (x) S"}}}
	if d.linuxParent(1) != 0 {
		t.Fatal("short stat")
	}
}

func TestParsePlistBad(t *testing.T) {
	if l, a := parsePlist("<plist><dict><key>Label"); l != "" || a != nil {
		t.Fatal(l, a)
	}
}

// ---- commands ----------------------------------------------------------

func deps(f *fakeSys, latest string, latestErr error) (Deps, *[]string) {
	var swapped []string
	return Deps{
		Sys: f,
		Config: func(v string) distkit.Config {
			return distkit.Config{Repo: "kfet/zulip-acp", Binary: Binary, Version: v}
		},
		Latest: func(context.Context, distkit.Config) (string, error) { return latest, latestErr },
		Swap: func(_ context.Context, c distkit.Config) (distkit.Result, error) {
			p, _ := c.ExecPath()
			swapped = append(swapped, p+"@"+c.Version)
			if strings.Contains(p, "Cellar") {
				return distkit.Result{}, errors.New("brew failed")
			}
			return distkit.Result{Updated: true}, nil
		},
		Legacy: func(args []string, stdout, _ io.Writer) int {
			fmt.Fprintf(stdout, "legacy %q", args)
			return 7
		},
		Interactive: func() bool { return true },
	}, &swapped
}

func TestStatus(t *testing.T) {
	var out, errb bytes.Buffer
	d, _ := deps(linuxHost(), "v1.1.0", nil)
	if rc := Status(nil, "v1.1.0", &out, &errb, d); rc != distkit.ExitUpdateAvailable {
		t.Fatalf("rc=%d", rc)
	}
	for _, w := range []string{"latest release: v1.1.0", "systemd/user", "reload pending", "(this session)", "stopped", "current", "update available → v1.1.0"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("missing %q in:\n%s", w, out.String())
		}
	}
}

func TestStatusCurrentAndBehind(t *testing.T) {
	f := &fakeSys{goos: "linux"}
	d, _ := deps(f, "v1.0.0", nil)
	var out bytes.Buffer
	if rc := Status(nil, "v1", &out, io.Discard, d); rc != 0 || !strings.Contains(out.String(), "none found") {
		t.Fatalf("rc=%d %s", rc, out.String())
	}
	// A running relay behind the release, its binary already current.
	f = linuxHost()
	delete(f.globs, "/proc/[0-9]*/exe")
	f.globs["/proc/[0-9]*/exe"] = []string{"/proc/200/exe"}
	f.runs["/proc/200/exe -version"] = "v0.9.0"
	f.runs = without(f.runs, userUnits, userFiles, sysUnits)
	f.execs = map[string]bool{"/opt/za/zulip-acp": true}
	f.exe = ""
	d, _ = deps(f, "v1.1.0", nil)
	out.Reset()
	if rc := Status(nil, "v1", &out, io.Discard, d); rc != distkit.ExitUpdateAvailable || !strings.Contains(out.String(), "(behind)") {
		t.Fatalf("rc=%d %s", rc, out.String())
	}
}

func without(m map[string]string, keys ...string) map[string]string {
	for _, k := range keys {
		delete(m, k)
	}
	return m
}

func TestStatusErrors(t *testing.T) {
	d, _ := deps(&fakeSys{goos: "linux"}, "", errors.New("offline"))
	var out, errb bytes.Buffer
	if rc := Status(nil, "v1", &out, &errb, d); rc != 1 || !strings.Contains(errb.String(), "offline") || !strings.Contains(out.String(), "latest release: -") {
		t.Fatalf("rc=%d %s %s", rc, out.String(), errb.String())
	}
	if rc := Status([]string{"-nope"}, "v1", &out, &errb, d); rc != 2 {
		t.Fatal(rc)
	}
	if rc := Status([]string{"-h"}, "v1", &out, &errb, d); rc != 0 {
		t.Fatal(rc)
	}
	// unknown binary version with no latest
	f := &fakeSys{goos: "linux", exe: "/b/zulip-acp", execs: map[string]bool{"/b/zulip-acp": true}}
	d, _ = deps(f, "", errors.New("offline"))
	out.Reset()
	Status(nil, "v1", &out, io.Discard, d)
	if !strings.Contains(out.String(), "unknown") {
		t.Fatal(out.String())
	}
}

func TestUpdateLegacyAndFlags(t *testing.T) {
	d, _ := deps(&fakeSys{goos: "linux"}, "v1", nil)
	var out bytes.Buffer
	if rc := Update([]string{"-check", "-y"}, "v1", nil, &out, io.Discard, d); rc != 7 || out.String() != `legacy ["-check"]` {
		t.Fatalf("rc=%d %s", rc, out.String())
	}
	out.Reset()
	if rc := Update([]string{"-y", "-restart-cmd", "x"}, "v1", nil, &out, io.Discard, d); rc != 7 || out.String() != `legacy ["-restart-cmd" "x"]` {
		t.Fatalf("rc=%d %s", rc, out.String())
	}
	if rc := Update([]string{"-bogus"}, "v1", nil, io.Discard, io.Discard, d); rc != 2 {
		t.Fatal(rc)
	}
	if got := withoutYes([]string{"-y"}); got == nil || len(got) != 0 {
		t.Fatal(got)
	}
}

func TestUpdateResolveFails(t *testing.T) {
	d, _ := deps(&fakeSys{goos: "linux"}, "", errors.New("offline"))
	var errb bytes.Buffer
	if rc := Update(nil, "v1", nil, io.Discard, &errb, d); rc != 1 || !strings.Contains(errb.String(), "offline") {
		t.Fatal(rc, errb.String())
	}
}

func TestUpdateNothingToDo(t *testing.T) {
	d, _ := deps(&fakeSys{goos: "linux"}, "1.0.0", nil)
	var out bytes.Buffer
	if rc := Update(nil, "v1", nil, &out, io.Discard, d); rc != 0 || !strings.Contains(out.String(), "target is v1.0.0") {
		t.Fatal(rc, out.String())
	}
}

func TestUpdateDeclined(t *testing.T) {
	d, sw := deps(linuxHost(), "v1.1.0", nil)
	var out bytes.Buffer
	if rc := Update(nil, "v1", strings.NewReader(""), &out, io.Discard, d); rc != 1 || len(*sw) != 0 || !strings.Contains(out.String(), "nothing was changed") {
		t.Fatal(rc, out.String())
	}
}

func TestUpdateLinuxPlan(t *testing.T) {
	f := linuxHost()
	f.runs["systemctl --user reload zulip-acp.service"] = ""
	f.files["/proc/999/stat"] = "999 (x) S 1 1"
	// pid 200 is behind and on a binary that will be swapped.
	f.runs["/proc/200/exe -version"] = "v1.0.0"
	f.runs["/opt/za/zulip-acp -version"] = "v1.0.0"
	d, sw := deps(f, "v1.1.0", nil)
	var out, errb bytes.Buffer
	rc := Update(nil, "v1", strings.NewReader("yes\n"), &out, &errb, d)
	if rc != 0 {
		t.Fatalf("rc=%d\n%s\n%s", rc, out.String(), errb.String())
	}
	if fmt.Sprint(*sw) != "[/opt/za/zulip-acp@v1.0.0]" {
		t.Errorf("swapped %v", *sw)
	}
	for _, w := range []string{"skip    /usr/local/Cellar/zulip-acp/bin/zulip-acp does not report a version", "swap    /opt/za/zulip-acp  v1.0.0 → v1.1.0", "reload  systemd zulip-acp.service pid 100  (running v1.0.0, disk is newer)",
		"note    systemd za-sys.service is stopped", "reload  systemctl --user reload zulip-acp.service", "reload  kill -HUP 200"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("missing %q in:\n%s", w, out.String())
		}
	}
	if fmt.Sprint(f.signals) != "[200 hangup]" {
		t.Errorf("signals %v", f.signals)
	}
}

func TestUpdateSelfLastAndFailures(t *testing.T) {
	f := linuxHost()
	f.runs["/proc/200/exe -version"] = "v1.0.0"
	f.sigErr = errors.New("eperm")
	// user unit reload fails; brew binary fails, and a relay on it is skipped.
	f.globs["/proc/[0-9]*/exe"] = append(f.globs["/proc/[0-9]*/exe"], "/proc/600/exe")
	f.links["/proc/600/exe"] = "/usr/local/bin/zulip-acp"
	f.runs["/proc/600/exe -version"] = "v1.1.0"
	f.runs["/usr/local/Cellar/zulip-acp/bin/zulip-acp -version"] = "v1.0.0"
	d, _ := deps(f, "v1.1.0", nil)
	var out, errb bytes.Buffer
	if rc := Update([]string{"-y"}, "v1", nil, &out, &errb, d); rc != 1 {
		t.Fatal(rc)
	}
	o := out.String()
	if !strings.Contains(o, "skip    process pid 600") || !strings.Contains(errb.String(), "eperm") || !strings.Contains(errb.String(), "exit 1: systemctl --user reload") {
		t.Errorf("%s\n%s", o, errb.String())
	}
	if strings.Index(o, "reload  process pid 200") < strings.Index(o, "reload  systemd") {
		t.Errorf("self not last:\n%s", o)
	}
}

func TestReload(t *testing.T) {
	f := &fakeSys{runs: map[string]string{"systemctl reload z.service": ""}}
	var out bytes.Buffer
	if err := reload(f, Relay{Manager: Systemd, Scope: "system", Unit: "z.service", CanReload: true, Self: true}, &out); err != nil ||
		!strings.Contains(out.String(), "after the current turn") {
		t.Fatal(err, out.String())
	}
	if err := reload(f, Relay{Manager: Systemd, Unit: "z.service"}, &out); err == nil || !strings.Contains(err.Error(), "ExecReload") {
		t.Fatal(err)
	}
	if err := reload(f, Relay{Manager: Launchd, Unit: "l", PID: 4}, &out); err != nil || fmt.Sprint(f.signals) != "[4 hangup]" {
		t.Fatal(err, f.signals)
	}
}

func TestDescribeAndDefault(t *testing.T) {
	if describe(Relay{Manager: Process}) != "process" {
		t.Fatal()
	}
	d := Default(func(v string) distkit.Config {
		return distkit.Config{Repo: "kfet/zulip-acp", Binary: Binary, Version: v, APIBase: "http://127.0.0.1:1", DownloadBase: "http://127.0.0.1:1", Anonymous: true}
	}, nil)
	if _, err := d.Latest(context.Background(), d.Config("v1")); err == nil {
		t.Fatal("want error")
	}
	if d.Sys == nil || d.Swap == nil {
		t.Fatal()
	}
}

func TestDefaultLatestOK(t *testing.T) {
	srv := newReleaseServer(t)
	d := Default(nil, nil)
	tag, err := d.Latest(context.Background(), distkit.Config{Repo: "kfet/zulip-acp", Binary: Binary, Version: "v1", APIBase: srv, DownloadBase: srv, Anonymous: true})
	if err != nil || tag != "v9.9.9" {
		t.Fatal(tag, err)
	}
}

func TestOSSys(t *testing.T) {
	s := OS()
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	if err := os.WriteFile(p, []byte("hi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p, p+"l"); err != nil {
		t.Fatal(err)
	}
	if b, err := s.ReadFile(p); err != nil || string(b) != "hi" {
		t.Fatal(err)
	}
	if l, err := s.Readlink(p + "l"); err != nil || l != p {
		t.Fatal(err)
	}
	if g, _ := s.Glob(filepath.Join(dir, "*")); len(g) != 2 {
		t.Fatal(g)
	}
	if r, _ := s.EvalSymlinks(p + "l"); r != p {
		t.Fatal(r)
	}
	if !s.IsExec(p) || s.IsExec(dir) {
		t.Fatal("isexec")
	}
	if out, err := s.Run("echo", "a"); err != nil || out != "a\n" {
		t.Fatal(out, err)
	}
	if e, err := s.Executable(); err != nil || e == "" {
		t.Fatal(err)
	}
	if s.GOOS() == "" || s.Getpid() == 0 || s.Home() == "" || s.Getenv("PATH") == "" {
		t.Fatal("basics")
	}
	if err := s.Signal(os.Getpid(), 0); err != nil {
		t.Fatal(err)
	}
}

func newReleaseServer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/kfet/zulip-acp/releases/tag/v9.9.9", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDiscoverDarwinNoPS(t *testing.T) {
	if rep := Discover(&fakeSys{goos: "darwin"}); len(rep.Relays) != 0 {
		t.Fatal(rep)
	}
	if firstNonEmpty("a", "b") != "a" || firstNonEmpty("", "b") != "b" {
		t.Fatal("firstNonEmpty")
	}
}

// A unit running as another user (root) is still running: its PID is
// kept, its version is Unknown, and only a swap of its binary reloads it.
func TestForeignUnit(t *testing.T) {
	f := linuxHost()
	k := "systemctl show -p Id,MainPID,ExecStart,ExecReload za-sys.service"
	f.runs[k] = strings.Replace(f.runs[k], "MainPID=0", "MainPID=77", 1)
	rep := Discover(f)
	s := rep.Relays[1]
	if s.PID != 77 || s.Running != Unknown || behind(s, "v1.1.0") {
		t.Fatalf("%+v", s)
	}
	d, _ := deps(f, "v1.1.0", nil)
	var out bytes.Buffer
	Update(nil, "v1", strings.NewReader(""), &out, io.Discard, d)
	if strings.Contains(out.String(), "za-sys") {
		t.Fatalf("foreign unit planned:\n%s", out.String())
	}
}

func TestUpdateNonInteractiveIsLegacy(t *testing.T) {
	d, _ := deps(linuxHost(), "v1.1.0", nil)
	d.Interactive = func() bool { return false }
	var out bytes.Buffer
	if rc := Update([]string{"-version", "v1.1.0"}, "v1", nil, &out, io.Discard, d); rc != 7 || out.String() != `legacy ["-version" "v1.1.0"]` {
		t.Fatal(rc, out.String())
	}
	if d := Default(nil, nil); d.Interactive == nil {
		t.Fatal()
	} else {
		d.Interactive() // go test's stdin is not a terminal; must not panic
	}
}

func TestUpdateSkipsDevBuild(t *testing.T) {
	f := &fakeSys{goos: "linux", exe: "/b/zulip-acp", execs: map[string]bool{"/b/zulip-acp": true},
		runs: map[string]string{"/b/zulip-acp -version": "1.0.0-dev+abc"}}
	d, sw := deps(f, "v1.1.0", nil)
	var out bytes.Buffer
	if rc := Update(nil, "v1", nil, &out, io.Discard, d); rc != 0 || len(*sw) != 0 || !strings.Contains(out.String(), "dev build") {
		t.Fatal(rc, out.String())
	}
	out.Reset()
	if rc := Update([]string{"-y", "-version", "v1.1.0"}, "v1", nil, &out, io.Discard, d); rc != 0 || len(*sw) != 1 {
		t.Fatal(rc, out.String())
	}
}

func TestLookPathRelative(t *testing.T) {
	d := &discoverer{sys: &fakeSys{}}
	if d.lookPath("./zulip-acp") != "./zulip-acp" || d.lookPath("zulip-acp") != "zulip-acp" {
		t.Fatal()
	}
}
