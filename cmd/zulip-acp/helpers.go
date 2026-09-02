package main

import (
	"log"
	"path/filepath"
	"strings"

	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/skills"
	"github.com/kfet/zulip-acp/internal/sysprompt"
)

// Test seams. Production points these at the real loaders.
var (
	loadBuiltinSkills = skills.LoadBuiltin
	loadDirSkills     = skills.LoadDir
)

// systemPromptProvider returns a func evaluated at every session
// create/resume, so a skill dropped into <config-dir>/skills/ is picked
// up without restarting the relay.
//
// The embedded bundle cannot change at runtime, so builtins are loaded
// exactly once, here: LoadBuiltin extracts files to $TMPDIR with a
// non-atomic read-compare-write, and calling it from concurrently
// created sessions could let the agent observe a half-written SKILL.md.
// Only the host dir — a read-only walk — is rescanned per session.
func systemPromptProvider(cfgPath string, cfg *config.Config) func() string {
	if cfg.DisableSystemPrompt {
		// No prompt at all, so never touch the skill dirs.
		return func() string { return "" }
	}
	builtin, err := loadBuiltinSkills()
	if err != nil {
		log.Printf("skills: builtin load failed (continuing): %v", err)
	}
	dir := config.DefaultConfigDir()
	if cfgPath != "" {
		dir = filepath.Dir(cfgPath)
	}
	hostDir := filepath.Join(dir, "skills")
	return func() string {
		return sysprompt.Resolve(cfg.SystemPrompt, false,
			buildSkillsCatalog(builtin, hostDir), cfg.GetSilentSentinel())
	}
}

// buildSkillsCatalog renders the <available_skills> block injected into
// every session's system prompt. Host skills, read from
// <hostDir>/*/SKILL.md, override same-named builtins — that is the
// disable mechanism.
//
// A host-dir failure is logged and swallowed: a missing or malformed
// skill dir must never cost the relay its system prompt, since the
// agent is still usable without a catalog.
func buildSkillsCatalog(builtin []skills.Skill, hostDir string) string {
	host, err := loadDirSkills(hostDir)
	if err != nil {
		log.Printf("skills: host dir %s: %v (continuing)", hostDir, err)
	}
	merged := skills.Merge([][]skills.Skill{builtin, host}, nil)
	if len(merged) == 0 {
		return ""
	}
	names := make([]string, 0, len(merged))
	for _, s := range merged {
		names = append(names, s.Name)
	}
	log.Printf("skills: %d builtin + %d host -> injected %d (%s)",
		len(builtin), len(host), len(merged), strings.Join(names, ","))
	return skills.FormatCatalog(merged)
}
