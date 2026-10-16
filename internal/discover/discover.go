package discover

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Binary is the installed file name this package looks for.
const Binary = "zulip-acp"

// Unknown is the version of an image this user cannot run -version on.
const Unknown = "?"

// Manager names what starts a relay.
const (
	Systemd = "systemd"
	Launchd = "launchd"
	Process = "process" // running with no service manager we recognise
)

// Relay is one relay instance: a running process, a service unit, or both.
type Relay struct {
	Manager string // Systemd, Launchd or Process
	Scope   string // "user" or "system" for a service manager; "" otherwise
	Unit    string // systemd unit or launchd label
	PID     int    // 0 when not running
	Binary  string // on-disk path the relay runs, symlinks resolved
	// Running is the version of the running image; "" when not running.
	Running string
	// Stale is set when the running image was replaced on disk: the
	// new binary takes effect only after a reload.
	Stale    bool
	Config   string
	StateDir string
	// CanReload is false for a systemd unit with no ExecReload, where
	// `systemctl reload` would fail.
	CanReload bool
	// Self is set when the command doing the discovery runs inside this
	// relay (an agent turn ran it): a reload then waits for that turn.
	Self bool
}

// Install is one distinct binary on disk.
type Install struct {
	Path    string // symlinks resolved
	Version string // Unknown when it does not answer -version
}

// Report is everything Discover found.
type Report struct {
	Relays   []Relay
	Binaries []Install
}

type proc struct {
	pid, ppid int
	exe       string
	stale     bool
	argv      []string
}

// Discover finds the relays and binaries on this host. It is best effort:
// a probe that fails (no systemd user bus, no launchctl) contributes
// nothing rather than failing the whole report.
func Discover(sys Sys) Report {
	d := discoverer{sys: sys, versions: map[string]string{}}
	return d.run()
}

type discoverer struct {
	sys      Sys
	versions map[string]string
}

func (d *discoverer) run() Report {
	var procs []proc
	var units []Relay
	if d.sys.GOOS() == "darwin" {
		procs = d.darwinProcs()
		units = d.launchdUnits()
	} else {
		procs = d.linuxProcs()
		units = append(d.systemdUnits("user"), d.systemdUnits("system")...)
	}
	anc := ancestors(d.sys.Getpid(), d.parentFn())

	var rep Report
	claimed := map[int]bool{}
	byPID := map[int]proc{}
	for _, p := range procs {
		byPID[p.pid] = p
	}
	for _, u := range units {
		if p, ok := byPID[u.PID]; ok && u.PID > 0 {
			claimed[p.pid] = true
			d.fill(&u, p, anc)
		} else if u.PID > 0 {
			// Running, but as another user: its image cannot be read.
			u.Running = Unknown
		}
		rep.Relays = append(rep.Relays, u)
	}
	for _, p := range procs {
		if claimed[p.pid] {
			continue
		}
		r := Relay{Manager: Process, CanReload: true}
		d.fill(&r, p, anc)
		rep.Relays = append(rep.Relays, r)
	}
	rep.Binaries = d.binaries(rep.Relays)
	return rep
}

// fill copies a process's facts into a relay.
func (d *discoverer) fill(r *Relay, p proc, anc map[int]bool) {
	r.PID = p.pid
	r.Binary = d.resolve(p.exe)
	r.Stale = p.stale
	r.Self = anc[p.pid]
	if c, s := argPaths(p.argv); c != "" || s != "" {
		// The live argv wins over the unit file: it is what is running.
		r.Config, r.StateDir = c, s
	}
	if r.StateDir == "" {
		r.StateDir = d.configStateDir(r.Config)
	}
	if d.sys.GOOS() == "linux" {
		// /proc/<pid>/exe still opens a deleted image, so this is the
		// version actually running, not the one on disk.
		r.Running = d.version("/proc/" + strconv.Itoa(p.pid) + "/exe")
	} else {
		r.Running = d.version(r.Binary)
	}
}

func (d *discoverer) resolve(p string) string {
	if r, err := d.sys.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// version asks a binary for its version, once per path.
func (d *discoverer) version(bin string) string {
	if v, ok := d.versions[bin]; ok {
		return v
	}
	v := Unknown
	if out, err := d.sys.Run(bin, "-version"); err == nil {
		if line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]); line != "" {
			v = line
		}
	}
	d.versions[bin] = v
	return v
}

// configStateDir reads state_dir from a relay's JSON config.
func (d *discoverer) configStateDir(path string) string {
	if path == "" {
		return ""
	}
	b, err := d.sys.ReadFile(path)
	if err != nil {
		return ""
	}
	var c struct {
		StateDir string `json:"state_dir"`
	}
	_ = json.Unmarshal(b, &c)
	return c.StateDir
}

// argPaths takes -config and -state-dir out of a relay argv, in every
// spelling the flag package accepts.
func argPaths(argv []string) (config, stateDir string) {
	for i := 1; i < len(argv); i++ {
		name, val, hasVal := strings.Cut(strings.TrimLeft(argv[i], "-"), "=")
		if !strings.HasPrefix(argv[i], "-") || (name != "config" && name != "state-dir") {
			continue
		}
		if !hasVal {
			if i+1 >= len(argv) {
				continue
			}
			i++
			val = argv[i]
		}
		if name == "config" {
			config = val
		} else {
			stateDir = val
		}
	}
	return config, stateDir
}

// isRelay reports whether argv is a relay, not a subcommand of the same
// binary (this command, or the agent's MCP redirector).
func isRelay(exe string, argv []string) bool {
	if filepath.Base(exe) != Binary {
		return false
	}
	if len(argv) > 1 {
		switch argv[1] {
		case "update", "status", "mcp-serve":
			return false
		}
	}
	return true
}

// ---- Linux: /proc ------------------------------------------------------

func (d *discoverer) linuxProcs() []proc {
	links, _ := d.sys.Glob("/proc/[0-9]*/exe")
	self := d.sys.Getpid()
	var out []proc
	for _, l := range links {
		pid, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(l, "/proc/"), "/exe"))
		exe, err := d.sys.Readlink(l)
		if err != nil || pid == self {
			continue
		}
		p := proc{pid: pid}
		p.exe, p.stale = strings.CutSuffix(exe, " (deleted)")
		raw, _ := d.sys.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		p.argv = strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if isRelay(p.exe, p.argv) {
			out = append(out, p)
		}
	}
	return out
}

// linuxParent reads the parent pid from /proc/<pid>/stat. The command
// name in field 2 may hold spaces and parentheses, so the fields are
// counted from the LAST ')'.
func (d *discoverer) linuxParent(pid int) int {
	b, err := d.sys.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

func (d *discoverer) parentFn() func(int) int {
	if d.sys.GOOS() == "darwin" {
		return d.darwinParent
	}
	return d.linuxParent
}

// ancestors is the set of pids above pid, so a relay that ran this
// command (through its agent) can be told apart.
func ancestors(pid int, parent func(int) int) map[int]bool {
	anc := map[int]bool{}
	for p := parent(pid); p > 1 && !anc[p]; p = parent(p) {
		anc[p] = true
	}
	return anc
}

// ---- systemd -----------------------------------------------------------

func scopeArgs(scope string) []string {
	if scope == "user" {
		return []string{"--user"}
	}
	return nil
}

func (d *discoverer) systemdUnits(scope string) []Relay {
	sa := scopeArgs(scope)
	names := map[string]bool{}
	for _, list := range [][]string{
		{"list-units", "--all", "--type=service", "--plain", "--no-legend", "--no-pager"},
		{"list-unit-files", "--type=service", "--no-legend", "--no-pager"},
	} {
		out, err := d.sys.Run("systemctl", append(append([]string{}, sa...), list...)...)
		if err != nil {
			continue
		}
		for _, n := range unitNames(out) {
			names[n] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	args := append(append([]string{}, sa...), "show", "-p", "Id,MainPID,ExecStart,ExecReload")
	out, err := d.sys.Run("systemctl", append(args, sorted...)...)
	if err != nil {
		return nil
	}
	var rs []Relay
	for _, u := range parseSystemdShow(out) {
		if !isRelay(u.argv0(), u.argv) {
			continue
		}
		c, s := argPaths(u.argv)
		rs = append(rs, Relay{
			Manager: Systemd, Scope: scope, Unit: u.id, PID: u.pid,
			Binary: d.resolve(u.argv0()), Config: c, StateDir: firstNonEmpty(s, d.configStateDir(c)),
			CanReload: u.reload,
		})
	}
	return rs
}

// unitNames takes the first column of list-units / list-unit-files,
// dropping template units, which `show` cannot describe.
func unitNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasSuffix(f[0], ".service") || strings.HasSuffix(f[0], "@.service") {
			continue
		}
		names = append(names, f[0])
	}
	return names
}

type unitShow struct {
	id     string
	pid    int
	argv   []string
	reload bool
}

func (u unitShow) argv0() string {
	if len(u.argv) == 0 {
		return ""
	}
	return u.argv[0]
}

// parseSystemdShow reads `systemctl show` output for many units: one
// block per unit, separated by a blank line. ExecStart looks like
// "{ path=/x ; argv[]=/x --config /c ; ignore_errors=no ; ... }".
func parseSystemdShow(out string) []unitShow {
	var us []unitShow
	for _, block := range strings.Split(out, "\n\n") {
		var u unitShow
		for _, line := range strings.Split(block, "\n") {
			k, v, _ := strings.Cut(line, "=")
			switch k {
			case "Id":
				u.id = v
			case "MainPID":
				u.pid, _ = strconv.Atoi(v)
			case "ExecStart":
				if _, rest, ok := strings.Cut(v, "argv[]="); ok {
					a, _, _ := strings.Cut(rest, " ;")
					u.argv = strings.Fields(a)
				}
			case "ExecReload":
				u.reload = strings.TrimSpace(v) != ""
			}
		}
		if u.id != "" {
			us = append(us, u)
		}
	}
	return us
}

// ---- macOS: ps + launchd -----------------------------------------------

func (d *discoverer) psTable() []proc {
	out, err := d.sys.Run("ps", "-axww", "-o", "pid=,ppid=,args=")
	if err != nil {
		return nil
	}
	return parsePS(out)
}

// parsePS reads `ps -o pid=,ppid=,args=`. ps prints no exe path, so argv0
// stands in for it; a bare name is resolved against PATH later.
func parsePS(out string) []proc {
	var ps []proc
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		ps = append(ps, proc{pid: pid, ppid: ppid, exe: f[2], argv: f[2:]})
	}
	return ps
}

func (d *discoverer) darwinProcs() []proc {
	self := d.sys.Getpid()
	var out []proc
	for _, p := range d.psTable() {
		if p.pid == self || !isRelay(p.exe, p.argv) {
			continue
		}
		if !filepath.IsAbs(p.exe) {
			p.exe = d.lookPath(p.exe)
		}
		out = append(out, p)
	}
	return out
}

func (d *discoverer) darwinParent(pid int) int {
	out, err := d.sys.Run("ps", "-o", "ppid=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

func (d *discoverer) lookPath(name string) string {
	if strings.ContainsRune(name, '/') {
		return name // relative to a cwd we do not know
	}
	for _, dir := range filepath.SplitList(d.sys.Getenv("PATH")) {
		if p := filepath.Join(dir, name); d.sys.IsExec(p) {
			return p
		}
	}
	return name
}

func (d *discoverer) launchdUnits() []Relay {
	pids := map[string]int{}
	if out, err := d.sys.Run("launchctl", "list"); err == nil {
		pids = parseLaunchctlList(out)
	}
	dirs := []struct{ dir, scope string }{
		{filepath.Join(d.sys.Home(), "Library", "LaunchAgents"), "user"},
		{"/Library/LaunchAgents", "user"},
		{"/Library/LaunchDaemons", "system"},
	}
	var rs []Relay
	for _, dir := range dirs {
		files, _ := d.sys.Glob(filepath.Join(dir.dir, "*.plist"))
		for _, f := range files {
			// plutil turns a binary plist into XML; an XML one passes through.
			out, err := d.sys.Run("plutil", "-convert", "xml1", "-o", "-", f)
			if err != nil {
				continue
			}
			label, argv := parsePlist(out)
			if label == "" || len(argv) == 0 || !isRelay(argv[0], argv) {
				continue
			}
			c, s := argPaths(argv)
			rs = append(rs, Relay{
				Manager: Launchd, Scope: dir.scope, Unit: label, PID: pids[label],
				Binary: d.resolve(argv[0]), Config: c, StateDir: firstNonEmpty(s, d.configStateDir(c)),
				CanReload: true,
			})
		}
	}
	return rs
}

// parseLaunchctlList reads `launchctl list`: "PID\tStatus\tLabel", with
// "-" for a job that is not running.
func parseLaunchctlList(out string) map[string]int {
	m := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			m[f[2]] = pid
		}
	}
	return m
}

// parsePlist takes Label and the command line (ProgramArguments, else
// Program) out of an XML property list.
func parsePlist(doc string) (label string, argv []string) {
	dec := xml.NewDecoder(strings.NewReader(doc))
	depth := 0 // depth of <dict>; only the top-level dict's keys count
	key, program := "", ""
	inArgs := false
	for {
		tok, err := dec.Token()
		if err == io.EOF || err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "dict":
				depth++
			case "array":
				inArgs = depth == 1 && key == "ProgramArguments"
			case "key", "string":
				var s string
				_ = dec.DecodeElement(&s, &t)
				switch {
				case t.Name.Local == "key" && depth == 1:
					key = s
				case inArgs:
					argv = append(argv, s)
				case depth == 1 && key == "Label":
					label = s
				case depth == 1 && key == "Program":
					program = s
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "dict":
				depth--
			case "array":
				inArgs = false
			}
		}
	}
	if len(argv) == 0 && program != "" {
		argv = []string{program}
	}
	return label, argv
}

// ---- binaries ----------------------------------------------------------

func (d *discoverer) binaries(relays []Relay) []Install {
	var cands []string
	if exe, err := d.sys.Executable(); err == nil {
		cands = append(cands, exe)
	}
	for _, r := range relays {
		cands = append(cands, r.Binary)
	}
	dirs := filepath.SplitList(d.sys.Getenv("PATH"))
	dirs = append(dirs, filepath.Join(d.sys.Home(), ".local", "bin"))
	for _, dir := range dirs {
		cands = append(cands, filepath.Join(dir, Binary))
	}
	seen := map[string]bool{}
	var out []Install
	for _, c := range cands {
		if c == "" || filepath.Base(c) != Binary || !d.sys.IsExec(c) {
			continue
		}
		p := d.resolve(c)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, Install{Path: p, Version: d.version(p)})
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
