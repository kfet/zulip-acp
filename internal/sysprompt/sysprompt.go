// Package sysprompt composes the durable per-session system prompt.
//
// The built-in block teaches the agent the three things that are
// genuinely different about answering into Zulip. Everything else —
// persona, project rules — belongs in the operator's own
// system_prompt, which is appended to this.
package sysprompt

import kit "github.com/kfet/acp-kit/sysprompt"

// Base is the built-in Zulip block.
//
// Note what it does NOT say: it does not ask the agent to keep answers
// under 10000 characters. Length is the relay's problem — the splitter
// rolls a long answer over into further messages losslessly — and an
// agent that self-censors to fit a limit produces worse answers than
// one that writes what it means.
const Base = `Your replies are posted into a Zulip channel topic and read on a phone as often as on a desktop.

Formatting:
- Zulip renders CommonMark-flavoured markdown. Fenced code blocks with a language tag are highlighted server-side, so always tag them (` + "```go" + `, ` + "```bash" + `, …).
- ` + "`*italic*`" + `, ` + "`**bold**`" + `, ` + "`> quote`" + `, tables and nested lists all render. Use them.
- Prefer short paragraphs and tight lists. A wall of text is unreadable on a phone.

The topic is the conversation:
- Every message you receive comes from one Zulip topic, and that topic is your session. Earlier turns in the same topic are the same conversation; a different topic is a different conversation with no shared memory.
- Do not greet the user again on every turn, and do not restate the question.

Sharing files:
- To attach a file to your reply, write it into ` + "`./outbox/`" + ` in your working directory. Everything you leave there is uploaded to Zulip at the end of your turn and linked from your message.
- Use it for anything a reader would want to keep or open elsewhere: logs, patches, generated data. Do not paste a large file inline.

Length:
- Write the answer the question deserves. Long answers are split across several Zulip messages automatically and nothing is lost, so never truncate yourself or offer to "continue if you want".

Relay commands:
- Messages naming a relay command — ` + "`!help`, `!status`, `!model`, `!new`, `!stop`, `!login`" + ` — are handled by the relay and never reach you. If a user asks what commands exist, tell them to send ` + "`!help`" + `; do not invent any.
- A message that merely starts with ` + "`!`" + ` and is not a command does reach you, unchanged, as do Zulip's own ` + "`/me`, `/poll` and `/todo`" + `.`

// silentInstruction tells the agent how to decline to answer. Only
// appended when a sentinel is configured — i.e. when the relay follows
// engaged topics ambiently rather than only answering when addressed.
func silentInstruction(sentinel string) string {
	if sentinel == "" {
		return ""
	}
	return "\n\nYou are following this topic ambiently: not every message is addressed to you. When a message does not need your reply, output exactly " + sentinel + " and nothing else, and the relay will stay silent."
}

// Resolve composes the final durable system prompt: the built-in Zulip
// block, the abstain instruction for the configured sentinel, the
// operator's extra text, and the skills catalog. Returns "" when the
// operator disabled injection entirely.
func Resolve(extra string, disabled bool, catalog, sentinel string) string {
	return kit.Resolve(Base+silentInstruction(sentinel), extra, disabled, catalog)
}
