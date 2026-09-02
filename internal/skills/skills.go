// Package skills owns the zulip-acp embedded skill bundle and re-exports
// the catalog primitives from acp-kit/skills. The wrapper lives here so
// the rest of the relay only depends on `internal/skills` regardless of
// where the implementation moves; it also pins the `"zulip-acp"` tmp-dir
// prefix used by LoadBuiltin so multiple relays sharing a host never
// collide in $TMPDIR.
package skills

import (
	"embed"

	kitskills "github.com/kfet/acp-kit/skills"
)

//go:embed all:bundle
var bundleFS embed.FS

// Skill is one entry in a fir-style skills catalog.
type Skill = kitskills.Skill

// LoadBuiltin walks the embedded zulip-acp bundle and extracts builtin
// SKILL.md files to a per-content-hash dir under $TMPDIR.
func LoadBuiltin() ([]Skill, error) { return kitskills.LoadBuiltin(bundleFS, "zulip-acp") }

// LoadDir walks <path>/*/SKILL.md and returns a fir-style catalog.
func LoadDir(path string) ([]Skill, error) { return kitskills.LoadDir(path) }

// Merge layers skill lists with last-wins-by-name semantics and drops
// names listed in disable.
func Merge(layers [][]Skill, disable []string) []Skill { return kitskills.Merge(layers, disable) }

// FormatCatalog renders a fir-style <available_skills> block ready for
// system-prompt injection.
func FormatCatalog(s []Skill) string { return kitskills.FormatCatalog(s) }
