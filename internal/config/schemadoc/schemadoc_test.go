package schemadoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckedInSchemaIsCurrent fails when a config key was added or its
// doc changed without `go generate ./internal/config`.
func TestCheckedInSchemaIsCurrent(t *testing.T) {
	want, err := Generate("..")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "schema_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("internal/config/schema_gen.go is stale: run `go generate ./internal/config`")
	}
}

// TestIncidentKeysDocumented: the keys an agent had to find with
// `strings` on 2026-11-09 are in the reference.
func TestIncidentKeysDocumented(t *testing.T) {
	fields, err := Fields("..")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]Field{}
	for _, f := range fields {
		seen[f.Key] = f
	}
	for _, k := range []string{"delete", "delete.bot_email", "delete.key_file", "delete.owners_only", "update_owner_ids", "quick_models[].emoji", "mcp_apps.listen"} {
		if seen[k].Doc == "" {
			t.Errorf("key %s missing or undocumented", k)
		}
	}
	if seen["delete"].Type != "object" || seen["quick_models"].Type != "list of objects" || seen["delete.owners_only"].Type != "bool" {
		t.Errorf("types: %+v %+v %+v", seen["delete"], seen["quick_models"], seen["delete.owners_only"])
	}
}

func write(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFieldsErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Fields(dir); err == nil || !strings.Contains(err.Error(), "no struct Config") {
		t.Fatalf("empty dir: %v", err)
	}
	if _, err := Generate(dir); err == nil {
		t.Fatal("Generate on empty dir")
	}
	write(t, dir, "x_test.go", "this is not go")
	write(t, dir, "a.go", "package p\ntype Config struct {\n\t// A doc.\n\tA int `json:\"a\"`\n\tB int `json:\"b\"`\n\n\tC int `json:\"c\"`\n}\n")
	if _, err := Fields(dir); err == nil || !strings.Contains(err.Error(), "c has no doc") {
		t.Fatalf("undocumented: %v", err)
	}
	write(t, dir, "a.go", "package p\ntype Config struct {\n\t// A doc.\n\tA int `json:\"a\"`\n\tx int\n\tY int `json:\"-\"`\n\t// Sub doc.\n\tS *Sub `json:\"s\"`\n}\ntype Sub struct {\n\tZ int `json:\"z\"`\n}\n")
	if _, err := Fields(dir); err == nil || !strings.Contains(err.Error(), "s.z has no doc") {
		t.Fatalf("nested undocumented: %v", err)
	}
	write(t, dir, "b.go", "package p\nfunc (")
	if _, err := Fields(dir); err == nil {
		t.Fatal("parse error not reported")
	}
}

func TestBlockSharesDoc(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "package p\ntype Config struct {\n\t// Shared doc.\n\tA int `json:\"a\"`\n\tB int `json:\"b,omitempty\"`\n}\n")
	f, err := Fields(dir)
	if err != nil || len(f) != 2 || f[1].Doc != "Shared doc." || f[1].Key != "b" {
		t.Fatalf("%+v %v", f, err)
	}
}

// TestReadmeTableHasEveryTopLevelKey keeps the README config table from
// drifting: every top-level key has a row. Nested keys are in
// `zulip-acp config-schema`.
func TestReadmeTableHasEveryTopLevelKey(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	fields, err := Fields("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if strings.ContainsAny(f.Key, ".[") {
			continue
		}
		if !strings.Contains(string(readme), "\n| `"+f.Key+"` |") {
			t.Errorf("README config table has no row for %s", f.Key)
		}
	}
}
