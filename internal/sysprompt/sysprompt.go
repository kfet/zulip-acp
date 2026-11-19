// Package sysprompt composes the durable per-session system prompt.
//
// The built-in block lives in sysprompt.md, a text/template embedded
// into the binary. The config key system_prompt_file can replace it
// with an operator file, so a wording change needs no rebuild.
// Persona and project rules belong in the operator's system_prompt,
// which is appended to the rendered block.
package sysprompt

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"text/template"

	kit "github.com/kfet/acp-kit/sysprompt"
)

//go:embed sysprompt.md
var embedded string

// Template is a parsed system-prompt template.
type Template struct{ t *template.Template }

// Default is the embedded built-in template.
var Default = mustParse(embedded)

// Parse parses src as a system-prompt template.
func Parse(src string) (*Template, error) {
	t, err := template.New("sysprompt").Parse(src)
	if err != nil {
		return nil, err
	}
	// Execute with every field off and every field on, so a bad field
	// reference in either branch fails here, at load, and not in a
	// new session.
	for _, d := range []data{{}, {Sentinel: "x", Reactions: true, DeleteEmoji: "x", WidgetsRoot: "x", TopicState: true}} {
		if err := t.Execute(new(strings.Builder), d); err != nil {
			return nil, err
		}
	}
	return &Template{t: t}, nil
}

// Load returns Default when path is "", else the template in the
// file at path.
func Load(path string) (*Template, error) {
	if path == "" {
		return Default, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := Parse(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// data holds the template fields. sysprompt.md documents them.
type data struct {
	Sentinel    string
	Reactions   bool
	DeleteEmoji string
	WidgetsRoot string
	TopicState  bool
}

// render executes the template. Parse proved that it executes, and
// the data has a fixed type, so an error here is a bug.
func (t *Template) render(d data) string {
	var b strings.Builder
	mustOK(t.t.Execute(&b, d))
	return b.String()
}

// Resolve composes the final durable system prompt: the rendered
// template, the operator's extra text, and the skills catalog.
// Returns "" when the operator disabled injection entirely.
// sentinel is the abstain sentinel, "" for none. reactions is on when
// the relay delivers reactions. deleteEmoji confirms a topic delete;
// "" means :fire:. widgetsSite is the realm root URL when inline
// widgets are on, else "". topicState is on when the relay offers the
// `topic_state` tool.
func (t *Template) Resolve(extra string, disabled bool, catalog, sentinel string, reactions bool, deleteEmoji, widgetsSite string, topicState bool) string {
	if deleteEmoji == "" {
		deleteEmoji = "fire"
	}
	root := ""
	if widgetsSite != "" {
		root = strings.TrimRight(widgetsSite, "/") + "/"
	}
	return kit.Resolve(t.render(data{
		Sentinel: sentinel, Reactions: reactions, DeleteEmoji: deleteEmoji,
		WidgetsRoot: root, TopicState: topicState,
	}), extra, disabled, catalog)
}

// Resolve is Default.Resolve.
func Resolve(extra string, disabled bool, catalog, sentinel string, reactions bool, deleteEmoji, widgetsSite string, topicState bool) string {
	return Default.Resolve(extra, disabled, catalog, sentinel, reactions, deleteEmoji, widgetsSite, topicState)
}
