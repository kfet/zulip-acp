// Package command is the relay's `!command` parse core: it decides
// whether a chat message is a relay command or ordinary prose, and
// resolves the command name against an ordered registry.
//
// # No Zulip in here
//
// This package must never import anything Zulip-specific — not
// `zulipproto`, not `journal`, not `handler`. A command surface is
// generic relay machinery: `slack-acp` and `poe-acp` would want the
// same parser, and the import graph is what keeps promoting it to
// `acp-kit/command` a `git mv` rather than an untangling. It is
// listed in BACKLOG.md as a promotion candidate, deliberately not
// promoted yet — see the same reasoning that keeps `internal/rollover`
// local.
//
// Everything platform-shaped therefore lives OUTSIDE: the concrete
// command list, the Zulip-markdown help rendering, and the handlers
// themselves are in `internal/handler`. This package holds only the
// grammar.
//
// # The grammar
//
// Given text that has already been mention-stripped and trimmed:
//
//   - A leading "!!" is the ESCAPE. One "!" is removed and the rest is
//     prose. That is how a human writes a message that genuinely
//     starts with a bang.
//   - Otherwise a leading "!" followed by a COMMAND-SHAPED token — a
//     letter, then letters, digits, "_" or "-" — is a command. Known
//     names (and aliases, case-insensitively) resolve; anything else
//     is reported as unknown so the caller can name the offender.
//   - Everything else is prose and is forwarded to the agent
//     byte-for-byte. "!important: ship it", "!5 minutes" and a lone
//     "!" are prose, because eating a message that merely happens to
//     start with a bang is far worse than missing a typo'd command.
package command

import (
	"fmt"
	"strings"
)

// Sigil is the character that introduces a relay command.
const Sigil = "!"

// Escape is what a human types to send prose that starts with Sigil.
// Parse turns a leading Escape back into a single literal Sigil.
const Escape = Sigil + Sigil

// Spec describes one command: its canonical name, any aliases, a usage
// hint for its argument, and a one-line summary for help output.
type Spec struct {
	// Name is the canonical, lower-case name, without the sigil.
	Name string
	// Aliases are additional lower-case names resolving to Name.
	Aliases []string
	// Args is the usage hint for the argument, e.g. "[id]". Empty when
	// the command takes none.
	Args string
	// Summary is the one-line description shown in help.
	Summary string
}

// Usage renders the invocation form, e.g. "!model [id]".
func (s Spec) Usage() string {
	if s.Args == "" {
		return Sigil + s.Name
	}
	return Sigil + s.Name + " " + s.Args
}

// Set is an ordered, immutable command registry. Order is registration
// order, which is the order help output uses — alphabetical would bury
// the command people actually need.
type Set struct {
	specs  []Spec
	byName map[string]Spec
}

// NewSet builds a registry. It panics on a duplicate name or alias:
// that is a programming error in a compile-time-constant list, and a
// silently shadowed command is far worse than a startup crash.
func NewSet(specs ...Spec) *Set {
	s := &Set{specs: specs, byName: make(map[string]Spec, len(specs)*2)}
	for _, sp := range specs {
		for _, n := range append([]string{sp.Name}, sp.Aliases...) {
			if _, dup := s.byName[n]; dup {
				panic(fmt.Sprintf("command: duplicate command name %q", n))
			}
			s.byName[n] = sp
		}
	}
	return s
}

// Specs returns the registered commands in registration order. The
// slice is a copy, so a caller rendering help cannot disturb the set.
func (s *Set) Specs() []Spec {
	out := make([]Spec, len(s.specs))
	copy(out, s.specs)
	return out
}

// Lookup resolves a name or alias, case-insensitively.
func (s *Set) Lookup(name string) (Spec, bool) {
	sp, ok := s.byName[strings.ToLower(name)]
	return sp, ok
}

// Kind classifies a parsed message.
type Kind int

const (
	// KindProse means the message is not a command and must reach the
	// agent unchanged (modulo an unescaped leading sigil).
	KindProse Kind = iota
	// KindCommand means a registered command was invoked.
	KindCommand
	// KindUnknown means the message is command-shaped but names no
	// registered command. It must NOT be forwarded to the agent.
	KindUnknown
)

// Result is what Parse decided.
type Result struct {
	// Kind is the classification.
	Kind Kind
	// Name is the resolved canonical command name (KindCommand) or the
	// lower-cased offending token (KindUnknown). Empty for prose.
	Name string
	// Spec is the resolved command (KindCommand only).
	Spec Spec
	// Args is everything after the command token, trimmed (KindCommand
	// only).
	Args string
	// Text is the prose to forward to the agent (KindProse only).
	Text string
}

// Parse classifies text. See the package doc for the grammar.
func (s *Set) Parse(text string) Result {
	t := strings.TrimSpace(text)
	if rest, ok := strings.CutPrefix(t, Escape); ok {
		return Result{Kind: KindProse, Text: Sigil + rest}
	}
	body, ok := strings.CutPrefix(t, Sigil)
	if !ok {
		return Result{Kind: KindProse, Text: t}
	}
	tok, args := splitToken(body)
	if !commandShaped(tok) {
		return Result{Kind: KindProse, Text: t}
	}
	name := strings.ToLower(tok)
	sp, known := s.Lookup(name)
	if !known {
		return Result{Kind: KindUnknown, Name: name}
	}
	return Result{Kind: KindCommand, Name: sp.Name, Spec: sp, Args: args}
}

// splitToken cuts body at the first whitespace run, returning the
// command token and the trimmed remainder.
func splitToken(body string) (tok, args string) {
	i := strings.IndexFunc(body, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if i < 0 {
		return body, ""
	}
	return body[:i], strings.TrimSpace(body[i:])
}

// commandShaped reports whether tok looks like a command name: a
// leading ASCII letter followed by ASCII letters, digits, "_" or "-".
// The check is deliberately strict — it is the only thing standing
// between "!important: fix this" and a swallowed message.
func commandShaped(tok string) bool {
	if tok == "" {
		return false
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}
