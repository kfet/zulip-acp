package prompts

import (
	"fmt"
	"io/fs"
	"text/template"
)

// mustParseAll is parseAll for the embedded files: a file that does
// not parse must stop the binary at start, not at the first turn.
func mustParseAll(fsys fs.FS) map[string]*template.Template {
	m, err := parseAll(fsys)
	mustOK(err)
	return m
}

// mustExist panics: a caller asked for a text that is not embedded.
func mustExist(path string) {
	panic(fmt.Sprintf("prompts: no file %s", path))
}

func mustOK(err error) {
	if err != nil {
		panic(err)
	}
}
