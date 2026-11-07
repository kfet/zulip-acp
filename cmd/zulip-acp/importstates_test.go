package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
)

func TestRunImportStates(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "in.json")
	_ = os.WriteFile(file, []byte(`{"c1":{"topic":"a","stream_id":1,"state":"fyi"},"c2":{"state":"x"}}`), 0o600)
	state := filepath.Join(dir, "state")

	var out, errw bytes.Buffer
	if rc := runImportStates([]string{"-state-dir", state, file}, &out, &errw); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errw.String())
	}
	if !strings.Contains(out.String(), "Staged 1 topic state entries in "+state) || !strings.Contains(errw.String(), "skipped c2") {
		t.Fatalf("out=%q err=%q", out.String(), errw.String())
	}
	if _, err := os.Stat(filepath.Join(state, journal.StagedImportName)); err != nil {
		t.Fatal(err)
	}

	// The state dir from the config file.
	cfgDir := filepath.Join(dir, "fromcfg")
	cfgFile := filepath.Join(dir, "cfg.json")
	_ = os.WriteFile(cfgFile, []byte(`{"state_dir":"`+cfgDir+`"}`), 0o600)
	out.Reset()
	if rc := runImportStates([]string{"-config", cfgFile, file}, &out, &errw); rc != 0 || !strings.Contains(out.String(), cfgDir) {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}

	// The default state dir.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg"))
	out.Reset()
	if rc := runImportStates([]string{file}, &out, &errw); rc != 0 {
		t.Fatalf("default dir: rc=%d err=%s", rc, errw.String())
	}
}

func TestRunImportStatesRefusals(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{`), 0o600)
	badCfg := filepath.Join(dir, "cfg.json")
	_ = os.WriteFile(badCfg, []byte(`{"nope":1}`), 0o600)
	for _, tc := range []struct {
		args []string
		rc   int
	}{
		{[]string{"-bogus"}, 2},
		{nil, 2},
		{[]string{"a", "b"}, 2},
		{[]string{"-config", badCfg, bad}, 1},
		{[]string{"-state-dir", dir, filepath.Join(dir, "missing")}, 1},
		{[]string{"-state-dir", dir, bad}, 1},
	} {
		var out, errw bytes.Buffer
		if rc := runImportStates(tc.args, &out, &errw); rc != tc.rc {
			t.Errorf("%v: rc=%d, want %d (%s)", tc.args, rc, tc.rc, errw.String())
		}
	}
}
