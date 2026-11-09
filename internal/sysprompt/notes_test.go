package sysprompt

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestNotesIndexEmpty(t *testing.T) {
	// An empty store costs no prompt.
	if got := NotesIndex(fstest.MapFS{"x.txt": {}}, "/n"); got != "" {
		t.Fatalf("got %q", got)
	}
	got := NotesIndex(fstest.MapFS{"a.md": {Data: []byte("# A")}}, "/n")
	if !strings.Contains(got, "`/n`") || !strings.Contains(got, "OVERRIDES your assumptions") {
		t.Fatalf("rule missing: %q", got)
	}
}

func TestNotesIndexSummaries(t *testing.T) {
	long := strings.Repeat("é", 200)
	fsys := fstest.MapFS{
		"a.md":                {Data: []byte("---\nname: x\ndescription: \"Front desc\"\n---\n# ignored\n")},
		"b.md":                {Data: []byte("\n## Build box\nbody\n")},
		"c.md":                {Data: []byte("plain first line\nsecond\n")},
		"d.md":                {Data: []byte("---\nname: y\n---\n# After front\n")},
		"e.md":                {Data: []byte("")},
		"f.md":                {Data: []byte("# " + long)},
		"x.txt":               {Data: []byte("# not a note")},
		".hidden.md":          {Data: []byte("# hidden")},
		".stfolder/z.md":      {Data: []byte("# hidden dir")},
		"fleet/docs/notes.MD": {Data: []byte("# Deep")},
	}
	got := NotesIndex(fsys, "/n")
	for _, want := range []string{
		"- a.md — Front desc\n",
		"- b.md — Build box\n",
		"- c.md — plain first line\n",
		"- d.md — After front\n",
		"- e.md\n",
		"- fleet/docs/notes.MD — Deep",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	for _, bad := range []string{"x.txt", "hidden", "ignored"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "- f.md — "+strings.Repeat("é", notesMaxSummary-1)+"…") {
		t.Errorf("long summary not clipped:\n%s", got)
	}
}

func TestNotesIndexCaps(t *testing.T) {
	fsys := fstest.MapFS{}
	for i := 0; i < notesMaxEntries+5; i++ {
		fsys[filepath.Join("n", strings.Repeat("a", 3)+string(rune('A'+i%26))+string(rune('a'+i/26))+".md")] = &fstest.MapFile{Data: []byte("# t")}
	}
	got := NotesIndex(fsys, "/n")
	if !strings.Contains(got, "… and 5 more") {
		t.Fatalf("entry cap: %s", got)
	}
	big := fstest.MapFS{}
	for i := 0; i < 50; i++ {
		big[string(rune('a'+i%26))+string(rune('a'+i/26))+".md"] = &fstest.MapFile{Data: []byte("# " + strings.Repeat("w", 110))}
	}
	got = NotesIndex(big, "/n")
	if n := len([]rune(got)); n > notesMaxBlock+200 {
		t.Fatalf("block too big: %d", n)
	}
	if !strings.Contains(got, "more: list the directory") {
		t.Fatalf("size cap: %s", got)
	}
}

// TestNotesIndexSymlinks covers what fs.WalkDir would miss: a shared
// fleet/ store is often a symlinked directory. It also covers a broken
// link and a link loop.
func TestNotesIndexSymlinks(t *testing.T) {
	root := t.TempDir()
	shared := t.TempDir()
	must(t, os.WriteFile(filepath.Join(shared, "build.md"), []byte("# Build box"), 0o600))
	must(t, os.Symlink(shared, filepath.Join(root, "fleet")))
	must(t, os.Symlink(filepath.Join(root, "nope"), filepath.Join(root, "broken")))
	must(t, os.Symlink(root, filepath.Join(shared, "loop")))
	got := NotesIndex(os.DirFS(root), root)
	if !strings.Contains(got, "- fleet/build.md — Build box") {
		t.Fatalf("symlinked dir not followed:\n%s", got)
	}
	if NotesIndex(os.DirFS(filepath.Join(root, "missing")), "x") != "" {
		t.Fatal("missing dir must give nothing")
	}
}

type openFailFS struct{ fstest.MapFS }

func (f openFailFS) Open(name string) (fs.File, error) {
	if name == "bad.md" {
		return nil, errors.New("boom")
	}
	return f.MapFS.Open(name)
}

func TestNotesIndexOpenError(t *testing.T) {
	fsys := openFailFS{fstest.MapFS{"bad.md": {Data: []byte("# x"), ModTime: time.Now()}}}
	if got := NotesIndex(fsys, "/n"); !strings.Contains(got, "- bad.md\n") && !strings.HasSuffix(got, "- bad.md") {
		t.Fatalf("got %s", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
