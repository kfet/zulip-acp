package prompts

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// TestEveryFileParses: each embedded text is a valid template, and
// the one trailing newline of a file is not part of the text.
func TestEveryFileParses(t *testing.T) {
	if len(set) == 0 {
		t.Fatal("no prompt files embedded")
	}
	if got := Note("literal-slash", nil); strings.HasSuffix(got, "\n") {
		t.Fatalf("trailing newline kept: %q", got)
	}
	if !strings.Contains(SystemPrompt(), "{{") {
		t.Fatal("SystemPrompt must be the unrendered template source")
	}
}

func TestRenderers(t *testing.T) {
	if got := System("notes-more", struct{ More int }{3}); got != "- … 3 more: list or grep the dir." {
		t.Fatalf("System = %q", got)
	}
	if got := Tool("react", struct{ History string }{"h"}); !strings.Contains(got, "the `h` tool") {
		t.Fatalf("Tool = %q", got)
	}
	if got := Param("react", "emoji", nil); got != "Zulip emoji name, e.g. one or thumbs_up." {
		t.Fatalf("Param = %q", got)
	}
}

func mustPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	f()
}

// TestMisuseIsABug: an unknown text or a missing value panics, so a
// golden test finds it instead of a model reading "<no value>".
func TestMisuseIsABug(t *testing.T) {
	mustPanic(t, func() { Note("no-such-note", nil) })
	mustPanic(t, func() { Note("rename-hint", struct{}{}) })
}

// errFS fails every read but lists one file.
type errFS struct{}

func (errFS) Open(name string) (fs.File, error) {
	if name == "." {
		return fstest.MapFS{"a.md": {}}.Open(name)
	}
	return nil, errors.New("boom")
}

func TestParseAllErrors(t *testing.T) {
	if _, err := parseAll(fstest.MapFS{"a.md": {Data: []byte("{{")}}); err == nil {
		t.Fatal("bad template parsed")
	}
	if _, err := parseAll(errFS{}); err == nil {
		t.Fatal("read error ignored")
	}
	m, err := parseAll(fstest.MapFS{"d/a.md": {Data: []byte("x\n")}, "d/b.txt": {}})
	if err != nil || len(m) != 1 {
		t.Fatalf("parseAll = %v, %v", m, err)
	}
}
