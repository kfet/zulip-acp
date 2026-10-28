package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDelete(t *testing.T) {
	f := false
	cases := []struct {
		name string
		d    *Delete
		own  []int64
		want string
	}{
		{"absent", nil, nil, ""},
		{"ok", &Delete{BotEmail: "del-bot@example.com", KeyFile: "/k", Scope: "topic"}, []int64{1}, ""},
		{"no email", &Delete{KeyFile: "/k"}, []int64{1}, "bot_email"},
		{"relative key", &Delete{BotEmail: "d@example.com", KeyFile: "k"}, []int64{1}, "absolute"},
		{"bad scope", &Delete{BotEmail: "d@example.com", KeyFile: "/k", Scope: "realm"}, []int64{1}, "scope"},
		{"owners only, no owners", &Delete{BotEmail: "d@example.com", KeyFile: "/k"}, nil, "update_owner_ids"},
		{"anyone, no owners", &Delete{BotEmail: "d@example.com", KeyFile: "/k", OwnersOnly: &f}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Delete: tc.d, UpdateOwnerIDs: tc.own}
			err := c.validateDelete()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateRunsDelete(t *testing.T) {
	c := &Config{Delete: &Delete{}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "delete.") {
		t.Fatalf("err = %v", err)
	}
}

func TestGetOwnersOnly(t *testing.T) {
	f := false
	if !(&Delete{}).GetOwnersOnly() || (&Delete{OwnersOnly: &f}).GetOwnersOnly() {
		t.Fatal("owners_only default or override wrong")
	}
}

func TestReadDeleteKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if k, err := (&Delete{KeyFile: write("ok", " abc\n", 0o600)}).ReadDeleteKey(); err != nil || k != "abc" {
		t.Fatalf("key = %q, %v", k, err)
	}
	for name, d := range map[string]*Delete{
		"0600":           {KeyFile: write("loose", "abc", 0o644)},
		"is empty":       {KeyFile: write("empty", "\n", 0o600)},
		"no such file":   {KeyFile: filepath.Join(dir, "missing")},
		"is a directory": {KeyFile: mkdir(t, dir)},
		"permission den": {KeyFile: write("unreadable", "abc", 0o000)},
	} {
		if _, err := d.ReadDeleteKey(); err == nil || !strings.Contains(err.Error(), name) {
			if name == "permission den" && os.Geteuid() == 0 {
				continue
			}
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func mkdir(t *testing.T, dir string) string {
	p := filepath.Join(dir, "sub")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}
