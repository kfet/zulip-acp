package main

import (
	"context"
	"log"
	"path/filepath"
	"strings"

	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/skills"
	"github.com/kfet/zulip-acp/internal/sysprompt"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// Test seams. Production points these at the real loaders.
var (
	loadBuiltinSkills = skills.LoadBuiltin
	loadDirSkills     = skills.LoadDir
)

// archiveProbe is the Zulip surface resolveArchive needs: whether this
// bot may move messages between channels at all.
type archiveProbe interface {
	CanMoveMessagesBetweenChannels(ctx context.Context, userID int64) (bool, error)
}

// channelLookup is the served-channel allowlist, narrowed to the one
// question resolveArchive asks it.
type channelLookup interface {
	Name(streamID int64) (string, bool)
}

// resolveArchive settles, at startup, whether the archive control can
// work — and returns (0, "") when it cannot, so the relay simply does
// not offer it.
//
// Three preconditions, each with its own log line, because "archiving
// is off" is useless to an operator who cannot see WHY:
//
//  1. a destination channel is configured and visible to the bot;
//  2. the relay does not SERVE that channel — an archived topic must
//     not be able to re-engage the relay, which is the entire reason
//     the destination is an unserved channel rather than a topic
//     prefix;
//  3. realm policy lets this bot move messages between channels
//     (can_move_messages_between_channels_group).
//
// An UNKNOWN answer to (3) — an older server, a locked-down API — is
// treated as no. The point of checking here is that nobody should
// discover the answer by tapping :wastebasket: and watching a warning
// be posted for an action that cannot happen.
func resolveArchive(ctx context.Context, cfg *config.Config, probe archiveProbe, streams []zulipproto.Stream, served channelLookup, botUserID int64) (int64, string) {
	want := cfg.GetArchiveChannel()
	if want == "" {
		log.Printf("zulip-acp: topic archiving is disabled (\"archive_channel\": \"\")")
		return 0, ""
	}
	s, ok := cfg.ResolveArchiveChannel(streams)
	if !ok {
		log.Printf("zulip-acp: topic archiving is OFF: no channel %q is visible to the bot. "+
			"Create it (the bot must not be subscribed — it must be a channel this relay does NOT serve) or set \"archive_channel\".", want)
		return 0, ""
	}
	if name, isServed := served.Name(s.StreamID); isServed {
		log.Printf("zulip-acp: topic archiving is OFF: the archive channel #%s (%d) is one this relay serves, "+
			"so an archived topic could just re-engage it. Point \"archive_channel\" at a channel outside the served set.", name, s.StreamID)
		return 0, ""
	}
	allowed, err := probe.CanMoveMessagesBetweenChannels(ctx, botUserID)
	if err != nil {
		log.Printf("zulip-acp: topic archiving is OFF: cannot tell whether this bot may move messages between channels (%v). "+
			"Zulip 12.0+ is needed to answer that for a bot user; until then, leave it off rather than fail on the tap.", err)
		return 0, ""
	}
	if !allowed {
		log.Printf("zulip-acp: topic archiving is OFF: realm policy (%s) does not let this bot move messages between channels. "+
			"Add it to that group to enable archiving.", zulipproto.RealmMoveBetweenChannels)
		return 0, ""
	}
	log.Printf("zulip-acp: topic archiving is on: :wastebasket: on my last message, or !archive, moves a topic to #%s (%d) after a confirmation", s.Name, s.StreamID)
	return s.StreamID, s.Name
}

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
			buildSkillsCatalog(builtin, hostDir), cfg.GetSilentSentinel(), cfg.GetReactions())
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
