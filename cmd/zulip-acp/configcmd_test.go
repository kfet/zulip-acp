package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

func noEnv(string) string { return "" }

const goodCfgJSON = `{"site":"https://z.example.com","bot_email":"b@example.com","bot_api_key":"k"}`

// incidentCfgJSON is the 2026-11-09 edit: owners_only with no owner.
const incidentCfgJSON = `{"site":"https://z.example.com","bot_email":"b@example.com","bot_api_key":"k",
"delete":{"bot_email":"d@example.com","key_file":"/k","owners_only":true}}`

func cfgFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUsageListsConfigSubcommands(t *testing.T) {
	var b bytes.Buffer
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.String("config", "", "path to JSON config file")
	usage(&b, fs)
	for _, want := range []string{"check-config", "config-schema", "-config"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("usage lacks %q:\n%s", want, b.String())
		}
	}
}

func TestRunCheckConfig(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		out  string
	}{
		{"ok", []string{"-config", cfgFile(t, goodCfgJSON)}, 0, "OK "},
		{"incident", []string{"-config", cfgFile(t, incidentCfgJSON)}, 1, "update_owner_ids"},
		{"bad flag", []string{"--check"}, 2, "not defined"},
		{"extra arg", []string{"x"}, 2, "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := runCheckConfig(tc.args, noEnv, &out, &errb); code != tc.code {
				t.Fatalf("code %d, want %d (%s%s)", code, tc.code, out.String(), errb.String())
			}
			if !strings.Contains(out.String()+errb.String(), tc.out) {
				t.Fatalf("output lacks %q: %s%s", tc.out, out.String(), errb.String())
			}
		})
	}
}

func TestRunConfigSchema(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runConfigSchema([]string{"delete."}, &out, &errb); code != 0 || !strings.Contains(out.String(), "delete.key_file") {
		t.Fatalf("%d %s", code, out.String())
	}
	out.Reset()
	if code := runConfigSchema(nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "update_owner_ids") {
		t.Fatalf("all: %d", code)
	}
	if code := runConfigSchema([]string{"nope"}, &out, &errb); code != 1 {
		t.Fatalf("unknown prefix: %d", code)
	}
	if code := runConfigSchema([]string{"a", "b"}, &out, &errb); code != 2 {
		t.Fatalf("two args: %d", code)
	}
}

// TestGatedReloadRefusesIncidentConfig: the 2026-11-09 edit never gets
// to the re-exec, and the error says the relay keeps running.
func TestGatedReloadRefusesIncidentConfig(t *testing.T) {
	calls := 0
	reload := func() error { calls++; return nil }

	err := gatedReload(cfgFile(t, incidentCfgJSON), noEnv, reload)()
	if err == nil || calls != 0 || !strings.Contains(err.Error(), "update_owner_ids") ||
		!strings.Contains(err.Error(), "keeps running") || !strings.Contains(err.Error(), "check-config") {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if err := gatedReload(cfgFile(t, goodCfgJSON), noEnv, reload)(); err != nil || calls != 1 {
		t.Fatalf("good: calls=%d err=%v", calls, err)
	}
	if err := gatedReload("", noEnv, reload)(); err != nil || calls != 2 {
		t.Fatalf("no file: calls=%d err=%v", calls, err)
	}
}

func TestReportFallback(t *testing.T) {
	notice := fallbackNotice("/c.json", errors.New("bad key"))
	for _, want := range []string{"/c.json.last-good", "bad key", "check-config -config /c.json"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice lacks %q: %s", want, notice)
		}
	}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	var sentTo []int64
	send := func(_ context.Context, ids []int64, _ string) (int64, error) { sentTo = ids; return 1, nil }

	reportFallback(context.Background(), send, nil, notice, logf)
	if sentTo != nil || len(logs) != 1 {
		t.Fatalf("no owners: sent=%v logs=%q", sentTo, logs)
	}
	reportFallback(context.Background(), send, []int64{7}, notice, logf)
	if len(sentTo) != 1 || sentTo[0] != 7 {
		t.Fatalf("owners not sent: %v", sentTo)
	}
	fail := func(context.Context, []int64, string) (int64, error) { return 0, errors.New("down") }
	logs = nil
	reportFallback(context.Background(), fail, []int64{7}, notice, logf)
	if len(logs) != 2 || !strings.Contains(logs[1], "down") {
		t.Fatalf("send failure not logged: %q", logs)
	}
}

// TestDegraded covers the start of a relay whose config names channels
// Zulip does not have: one loud log line per entry, one owner DM that
// says what is off, and the one case that stays fatal.
func TestDegraded(t *testing.T) {
	archive := "old-archive"
	cfg := &config.Config{
		Channels:       []string{"*"},
		Routes:         []config.Route{{From: "hp-board", To: "a"}},
		Views:          []config.View{{At: "gone/board"}},
		ArchiveChannel: &archive,
	}
	streams := []zulipproto.Stream{{StreamID: 4, Name: "a"}}
	cfg.ResolveRoutes(streams)
	cfg.ResolveViews(streams)
	cfg.ResolveArchiveChannel(streams)
	d := cfg.Degraded()

	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	logDegraded(d, logf)
	if len(logs) != 3 || !strings.HasPrefix(logs[0], "config: DEGRADED routes[0].from: channel \"hp-board\"") {
		t.Fatalf("logs = %q", logs)
	}
	notice := degradedNotice("/c.json", d)
	for _, want := range []string{"started DEGRADED", "3 entries of /c.json are OFF", "stays in #hp-board", "its turns run THERE",
		"board at gone/board is NOT updated", "topic archiving is OFF", "checks only the file", "`!status`"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice lacks %q: %s", want, notice)
		}
	}
	var sent string
	send := func(_ context.Context, _ []int64, s string) (int64, error) { sent = s; return 1, nil }
	dmOwners(context.Background(), send, []int64{7}, notice, logf)
	if sent != notice {
		t.Fatalf("sent %q", sent)
	}

	// Nothing to serve is the one fatal case.
	for _, tc := range []struct {
		explicit    map[int64]string
		follow, dms bool
		ok          bool
	}{{nil, false, false, false}, {map[int64]string{4: "a"}, false, false, true}, {nil, true, false, true}, {nil, false, true, true}} {
		err := servesSomething(tc.explicit, tc.follow, tc.dms, d)
		if (err == nil) != tc.ok {
			t.Fatalf("%+v: err %v", tc, err)
		}
		if err != nil && !strings.Contains(err.Error(), "nothing to serve") || err != nil && !strings.Contains(err.Error(), "hp-board") {
			t.Fatalf("err %v", err)
		}
	}
}

// TestReadyToApply: an invalid config holds an auto-update apply
// instead of letting the refused reload block a good release.
func TestReadyToApply(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	busy := func() bool { return false }
	idle := func() bool { return true }
	if readyToApply(cfgFile(t, goodCfgJSON), noEnv, busy, logf)() {
		t.Fatal("busy relay ready")
	}
	if !readyToApply(cfgFile(t, goodCfgJSON), noEnv, idle, logf)() || len(logs) != 0 {
		t.Fatalf("good config held: %q", logs)
	}
	if readyToApply(cfgFile(t, incidentCfgJSON), noEnv, idle, logf)() || len(logs) != 1 || !strings.Contains(logs[0], "apply held") {
		t.Fatalf("bad config not held: %q", logs)
	}
}
