package skills

import "testing"

// TestWrappersDelegate confirms the thin wrappers compile and forward
// to acp-kit/skills as expected. The zulip-acp embedded bundle always
// ships at least one builtin SKILL.md, so an empty result is a
// regression (frontmatter typo, bundle path mismatch, or kit import
// drift) rather than a tolerable edge case.
func TestWrappersDelegate(t *testing.T) {
	got, err := LoadBuiltin()
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
