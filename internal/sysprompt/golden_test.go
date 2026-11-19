package sysprompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGolden proves the embedded template renders byte-identical text
// to the prompt that Go code built before the move. The files in
// testdata were made by that code, for every option combination.
func TestGolden(t *testing.T) {
	i := 0
	for _, s := range []string{"", "<<SILENT>>"} {
		for _, r := range []bool{false, true} {
			for _, e := range []string{"", "boom"} {
				for _, w := range []string{"", "https://zulip.example.com"} {
					for _, ts := range []bool{false, true} {
						want, err := os.ReadFile(fmt.Sprintf("testdata/golden-%02d.md", i))
						if err != nil {
							t.Fatal(err)
						}
						if got := Resolve("", false, "", s, r, e, w, ts); got != string(want) {
							t.Errorf("golden-%02d differs:\n%s", i, got)
						}
						i++
					}
				}
			}
		}
	}
}

func TestLoad(t *testing.T) {
	if got, err := Load(""); err != nil || got != Default {
		t.Fatalf("Load(\"\") = %v, %v", got, err)
	}
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "missing.md")); err == nil {
		t.Fatal("missing file loaded")
	}
	p := filepath.Join(dir, "p.md")
	os.WriteFile(p, []byte("Custom. {{if .Sentinel}}Say {{.Sentinel}}.{{end}} Delete with :{{.DeleteEmoji}}:."), 0o644)
	tp, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := tp.Resolve("extra", false, "", "<<S>>", false, "", "", false)
	if !strings.HasPrefix(got, "Custom. Say <<S>>. Delete with :fire:.") || !strings.Contains(got, "extra") {
		t.Fatalf("override not rendered: %q", got)
	}
	for _, bad := range []string{"{{if}", "{{.NoSuchField}}", "{{if .Sentinel}}{{.Bogus}}{{end}}"} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("Load(%q) err = %v", bad, err)
		}
	}
}
