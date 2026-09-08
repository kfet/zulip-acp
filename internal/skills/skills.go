// Package skills owns the zulip-acp embedded skill bundle and re-exports
// the catalog primitives from acp-kit/skills. The wrapper lives here so
// the rest of the relay only depends on `internal/skills` regardless of
// where the implementation moves; it also pins the `"zulip-acp"` dir
// prefix used by LoadBuiltin so multiple relays sharing a host never
// collide, and the state-dir subtree the bundle is extracted into.
package skills

import (
	"embed"
	"errors"
	"path/filepath"

	kitskills "github.com/kfet/acp-kit/skills"
)

//go:embed all:bundle
var bundleFS embed.FS

// DirName is the StateDir subdirectory holding extracted skill bundles.
const DirName = "skills"

// Skill is one entry in a fir-style skills catalog.
type Skill = kitskills.Skill

// LoadBuiltin walks the embedded zulip-acp bundle and extracts builtin
// SKILL.md files to a per-content-hash dir under <stateDir>/skills.
//
// Under the STATE dir, not $TMPDIR, and that is the whole point: the
// system prompt tells the agent these paths are absolute and stable for
// the lifetime of the session, and a graceful reload replaces the
// process image mid-session (internal/reload). A temp dir keyed to the
// process made that promise false — the agent's own prompt pointed at a
// path from the previous image — and left the old extraction behind
// forever, one per released version. acp-kit collects those leftovers,
// here and in the legacy $TMPDIR location, as a side effect of this
// call. Re-extraction is idempotent: identical content is not rewritten.
//
// An empty stateDir is an ERROR rather than a relative path. main always
// resolves one (config.DefaultStateDir), so an empty value means a
// caller skipped that step — and "skills/" relative to whatever the
// process's working directory happens to be is how a test extracted the
// bundle into the source tree.
func LoadBuiltin(stateDir string) ([]Skill, error) {
	if stateDir == "" {
		return nil, errors.New("skills: empty state dir")
	}
	return kitskills.LoadBuiltinIn(filepath.Join(stateDir, DirName), bundleFS, "zulip-acp")
}

// LoadDir walks <path>/*/SKILL.md and returns a fir-style catalog.
func LoadDir(path string) ([]Skill, error) { return kitskills.LoadDir(path) }

// Merge layers skill lists with last-wins-by-name semantics and drops
// names listed in disable.
func Merge(layers [][]Skill, disable []string) []Skill { return kitskills.Merge(layers, disable) }

// FormatCatalog renders a fir-style <available_skills> block ready for
// system-prompt injection.
func FormatCatalog(s []Skill) string { return kitskills.FormatCatalog(s) }
