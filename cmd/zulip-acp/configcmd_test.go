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
