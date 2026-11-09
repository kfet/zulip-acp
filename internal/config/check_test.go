package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

// writeCfg writes a config file and returns its path.
func writeCfg(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodCfg = `{"site":"https://z.example.com","bot_email":"b@example.com","bot_api_key":"k"}`

// incidentCfg is the 2026-11-09 edit: a delete block with owners_only
// and no update_owner_ids.
func incidentCfg(keyFile string) string {
	return fmt.Sprintf(`{"site":"https://z.example.com","bot_email":"b@example.com","bot_api_key":"k",
"delete":{"bot_email":"d@example.com","key_file":%q,"owners_only":true}}`, keyFile)
}

func TestCheckFileIncident(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := CheckFile(writeCfg(t, dir, incidentCfg(key)), noEnv)
	if err == nil || !strings.Contains(err.Error(), "update_owner_ids") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckFile(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, body, want string }{
		{"ok", goodCfg, ""},
		{"parse", `{"nope":1}`, "unknown field"},
		{"credentials", `{"site":"https://z.example.com"}`, "bot_email"},
		{"unsafe key file", `{"site":"https://z.example.com","bot_email":"b@example.com","bot_api_key":"k","update_owner_ids":[1],
"delete":{"bot_email":"d@example.com","key_file":"` + key + `"}}`, "key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CheckFile(writeCfg(t, t.TempDir(), tc.body), noEnv)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error leaks the key")
			}
		})
	}
	if _, err := CheckFile(filepath.Join(dir, "missing.json"), noEnv); err == nil {
		t.Fatal("missing file passed")
	}
}

func TestCheckFileEnvCredentials(t *testing.T) {
	p := writeCfg(t, t.TempDir(), `{}`)
	env := map[string]string{EnvSite: "https://z.example.com", EnvEmail: "b@example.com", EnvAPIKey: "k"}
	c, err := CheckFile(p, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Site != env[EnvSite] || c.BotEmail != env[EnvEmail] || c.BotAPIKey != "k" {
		t.Fatalf("env not applied: %+v", c)
	}
}

func TestCheckValidateFirst(t *testing.T) {
	c := &Config{SessionIdleTimeoutSeconds: -1}
	if err := c.Check(); err == nil || !strings.Contains(err.Error(), "session_idle") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadOrLastGood(t *testing.T) {
	dir := t.TempDir()
	p := writeCfg(t, dir, goodCfg)
	var logs bytes.Buffer
	logf := func(f string, a ...any) { fmt.Fprintf(&logs, f+"\n", a...) }

	// A good file is used and saved, owner-only.
	c, fb, err := LoadOrLastGood(p, noEnv, logf)
	if err != nil || fb != nil || c.BotEmail != "b@example.com" {
		t.Fatalf("good: %v %v", fb, err)
	}
	st, err := os.Stat(LastGoodPath(p))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("last good copy: %v %v", st, err)
	}

	// A bad edit falls back to the saved copy and says why.
	writeCfg(t, dir, incidentCfg("/k"))
	c, fb, err = LoadOrLastGood(p, noEnv, logf)
	if err != nil || fb == nil || !strings.Contains(fb.Error(), "update_owner_ids") || c.BotEmail != "b@example.com" {
		t.Fatalf("fallback: %v %v", fb, err)
	}
	// ...and does not overwrite the good copy with the bad file.
	if b, _ := os.ReadFile(LastGoodPath(p)); string(b) != goodCfg {
		t.Fatalf("last good copy overwritten: %s", b)
	}

	// No usable copy: the error for the file itself.
	os.Remove(LastGoodPath(p))
	if _, _, err := LoadOrLastGood(p, noEnv, logf); err == nil || !strings.Contains(err.Error(), "update_owner_ids") {
		t.Fatalf("no copy: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected logs: %s", logs.String())
	}
}

func TestLoadOrLastGoodSaveFails(t *testing.T) {
	dir := t.TempDir()
	p := writeCfg(t, dir, goodCfg)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	var logs []string
	c, fb, err := LoadOrLastGood(p, noEnv, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	if err != nil || fb != nil || c == nil {
		t.Fatalf("%v %v", fb, err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "last good copy") {
		t.Fatalf("logs = %q", logs)
	}
}

func TestSaveLastGoodErrors(t *testing.T) {
	if err := SaveLastGood(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing source saved")
	}
	defer func(old func(string, string) (*os.File, error)) { createTemp = old }(createTemp)
	dir := t.TempDir()
	p := writeCfg(t, dir, goodCfg)
	createTemp = func(d, pat string) (*os.File, error) {
		f, err := os.CreateTemp(d, pat)
		if err == nil {
			f.Close() // a closed file fails the Write
		}
		return f, err
	}
	if err := SaveLastGood(p); err == nil {
		t.Fatal("failed write not reported")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".last-good-*")); len(m) != 0 {
		t.Fatalf("temp file left: %v", m)
	}
}

func TestWriteSchema(t *testing.T) {
	var b bytes.Buffer
	if n := WriteSchema(&b, "delete."); n != 5 || !strings.Contains(b.String(), "delete.key_file (string)\n") {
		t.Fatalf("n=%d\n%s", n, b.String())
	}
	if WriteSchema(&b, "") != len(Schema) || WriteSchema(&b, "zzz") != 0 {
		t.Fatal("filter wrong")
	}
}
