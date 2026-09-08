package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWrappersDelegate confirms the thin wrappers compile and forward
// to acp-kit/skills as expected. The zulip-acp embedded bundle always
// ships at least one builtin SKILL.md, so an empty result is a
// regression (frontmatter typo, bundle path mismatch, or kit import
// drift) rather than a tolerable edge case.
func TestWrappersDelegate(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // contain the legacy-location sweep
	got, err := LoadBuiltin(t.TempDir())
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("LoadBuiltin: empty catalog; the embedded zulip-acp bundle " +
			"should always carry at least one builtin: true SKILL.md")
	}
	if dir, err := LoadDir(""); err != nil || dir != nil {
		t.Fatalf("LoadDir empty path: got %v %v want nil nil", dir, err)
	}
	merged := Merge([][]Skill{got, nil}, nil)
	if len(merged) != len(got) {
		t.Fatalf("Merge len = %d want %d", len(merged), len(got))
	}
	if FormatCatalog(merged) == "" {
		t.Fatal("FormatCatalog empty for non-empty input")
	}
}

// TestBuiltinPathsAreStableUnderTheStateDir is the regression guard for
// a promise the system prompt makes to the agent: skill body paths are
// absolute and stable for the lifetime of the session. A reload
// replaces the process image mid-session, so an extraction under
// $TMPDIR — which also leaked one directory per released version —
// made that promise false the moment the relay updated itself.
func TestBuiltinPathsAreStableUnderTheStateDir(t *testing.T) {
	// LoadBuiltin also collects this app's leftovers from the LEGACY
	// $TMPDIR location, which is production behaviour we do not want a
	// test run on a live host to perform on the running relay.
	t.Setenv("TMPDIR", t.TempDir())
	state := t.TempDir()

	first, err := LoadBuiltin(state)
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	second, err := LoadBuiltin(state)
	if err != nil {
		t.Fatalf("LoadBuiltin (repeat): %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("catalog size changed on re-extraction: %d -> %d", len(first), len(second))
	}
	base := filepath.Join(state, DirName) + string(filepath.Separator)
	for i, s := range first {
		if s.Path != second[i].Path {
			t.Fatalf("%s moved between calls: %q -> %q", s.Name, s.Path, second[i].Path)
		}
		if !strings.HasPrefix(s.Path, base) {
			t.Fatalf("%s extracted to %q, which is not under %q", s.Name, s.Path, base)
		}
		if _, err := os.Stat(s.Path); err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
	}
}

// TestLoadBuiltinRefusesAnEmptyStateDir: filepath.Join("", "skills") is
// "skills" — a RELATIVE path — so an unset state dir silently extracted
// the bundle into whatever the process's working directory happened to
// be. A test doing that dropped the bundle into the source tree.
func TestLoadBuiltinRefusesAnEmptyStateDir(t *testing.T) {
	if _, err := LoadBuiltin(""); err == nil {
		t.Fatal("LoadBuiltin(\"\") = nil error; an empty state dir must not become a relative path")
	}
}
