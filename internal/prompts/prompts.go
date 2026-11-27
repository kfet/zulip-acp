// Package prompts holds every text a model reads that the relay
// writes: the built-in system prompt, the per-turn notes the handler
// adds to a prompt, and the descriptions of the relay's MCP tools and
// their parameters.
//
// Each text is one file, embedded into the binary:
//
//	system/<name>.md        parts of the durable system prompt
//	notes/<name>.md         per-turn notes the handler adds to a prompt
//	tools/<tool>/description.md   a tool's description
//	tools/<tool>/<param>.md       the description of one parameter
//
// A file is a text/template. The caller gives the values it needs as
// data; a missing value is an error, not "<no value>". The one
// trailing newline that ends each file is not part of the text, so a
// file can end like any other text file. The separators around a text
// (the blank line before a note, a leading space) stay in the Go code
// that joins the texts, because they are layout, not wording.
//
// Keep wording changes here, and keep the goldens of the callers
// (internal/handler, internal/zulipmcp, internal/sysprompt) in step:
// they prove which bytes the model gets.
package prompts

import (
	"embed"
	"io/fs"
	"strings"
	"text/template"
)

//go:embed system notes tools
var files embed.FS

// set holds every file, parsed once, by its path in files.
var set = mustParseAll(files)

// parseAll parses every .md file of fsys as one template, named by
// its path.
func parseAll(fsys fs.FS) (map[string]*template.Template, error) {
	out := map[string]*template.Template{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		t, err := template.New(p).Option("missingkey=error").Parse(strings.TrimSuffix(string(b), "\n"))
		if err != nil {
			return err
		}
		out[p] = t
		return nil
	})
	return out, err
}

// render executes the template at path with data. An unknown path or
// a failed execution is a bug in the caller, and the goldens catch it.
func render(path string, data any) string {
	t, ok := set[path]
	if !ok {
		mustExist(path)
	}
	var b strings.Builder
	mustOK(t.Execute(&b, data))
	return b.String()
}

// SystemPrompt returns the source of the built-in system prompt block,
// unrendered. internal/sysprompt parses it with its own fields, the
// same way it parses an operator's system_prompt_file.
func SystemPrompt() string {
	b, err := files.ReadFile("system/prompt.md")
	mustOK(err)
	return string(b)
}

// System renders system/<name>.md, a part of the system prompt that
// Go code assembles, such as the notes index.
func System(name string, data any) string { return render("system/"+name+".md", data) }

// Note renders notes/<name>.md, a note the handler adds to a prompt.
func Note(name string, data any) string { return render("notes/"+name+".md", data) }

// Tool renders the description of tool.
func Tool(tool string, data any) string { return render("tools/"+tool+"/description.md", data) }

// Param renders the description of param of tool.
func Param(tool, param string, data any) string {
	return render("tools/"+tool+"/"+param+".md", data)
}
