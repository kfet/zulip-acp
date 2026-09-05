// Package autotopic names a Zulip topic after the message that opens
// it.
//
// Zulip 11's "general chat" is the empty topic — but see IsGeneralChat
// for what a client actually sees on the wire. A relay
// that answers there buries every conversation in one undifferentiated
// stream, so in configured channels the opening message is MOVED to a
// topic of its own — and this package decides what that topic is
// called.
//
// The naming is a heuristic over the raw markdown, deliberately kept
// as a pure function of (text, now): no Zulip types, no network, no
// agent. That is what makes it exhaustively table-testable today and
// swappable for an agent-generated title later, with the call site
// unchanged.
package autotopic

import (
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxLen caps a generated topic, in code points. It is exactly Zulip's
// MAX_TOPIC_LENGTH, which the server enforces by SILENT truncation —
// the same trap as MAX_MESSAGE_LENGTH. Kept as a literal so this
// package stays free of Zulip imports; zulipproto.MaxTopicLength is
// the wire-side guard.
const MaxLen = 60

// generalChat is the display name Zulip substitutes for the empty
// topic when a client has not declared that it understands "".
const generalChat = "general chat"

// IsGeneralChat reports whether topic is Zulip's general chat.
//
// It matches BOTH forms, and that is the whole point. Zulip only sends
// the empty string for the empty topic to clients that declare the
// `empty_topic_name` client capability in POST /register; to everyone
// else the server substitutes the translated display name. Measured
// against Zulip 12.2 (feature level 500): GET /api/v1/messages returns
// "subject": "general chat" for a message in general chat, so a bare
// `topic == ""` test never fires in production.
//
// We deliberately DO NOT declare `empty_topic_name`. Declaring it
// would flip the topic string the relay sees for every empty-topic
// conversation at once, remapping their journal keys from "general
// chat" to "" and orphaning live agent sessions. That migration is not
// worth a cosmetic gain — so the topic string stays exactly what the
// server sent everywhere, and only this predicate knows both spellings.
//
// The compare is whitespace-trimmed and case-insensitive, but exact:
// an ordinary topic such as "general" or "general chat notes" is a
// human's topic and must not be moved.
//
// The display name is TRANSLATED, and only the English one is matched.
// In a realm whose default language is not English the server sends
// e.g. "Allgemeiner Chat" and autotopic is inert there — the relay
// answers in general chat, exactly as it did before the feature.
func IsGeneralChat(topic string) bool {
	t := strings.TrimSpace(topic)
	return t == "" || strings.EqualFold(t, generalChat)
}

// Name returns the topic to move a general-chat message to. It never
// returns a name that IS general chat — neither "" nor the display
// name — because such a result would silently mean "did not move".
func Name(text string) string { return NameAt(text, time.Now()) }

// NameAt is Name with the clock injected, so the fallback is testable.
func NameAt(text string, now time.Time) string {
	if s := fromText(text); !IsGeneralChat(s) {
		return s
	}
	// Nothing usable in the message: fall back to something stable,
	// human-readable and unlikely to collide with another turn.
	return "chat " + now.Format("2006-01-02 15:04:05")
}

// Disambiguate makes a name unique by appending a message id, which is
// unique per realm and stable forever. It exists because two people
// typing "hi" in general chat must not be dropped into ONE
// conversation — the name is a heuristic, the id is not.
func Disambiguate(name string, id int64) string {
	// An int64 is at most 19 digits, so the suffix is at most 23 code
	// points and always leaves room for a name.
	suffix := " (#" + strconv.FormatInt(id, 10) + ")"
	return truncate(name, MaxLen-utf8.RuneCountInString(suffix)) + suffix
}

// fromText renders the first usable line of a message as a topic, or
// "" when the message carries no usable words at all.
func fromText(text string) string {
	line := firstLine(text)
	line = stripMentions(line)
	line = stripDecoration(line)
	line = strings.Join(strings.Fields(line), " ")
	line = strings.TrimFunc(line, isTrimmable)
	if line == "" {
		return ""
	}
	return truncate(line, MaxLen)
}

// firstLine is the first non-empty line, with markdown quoting,
// heading and bullet markers stripped — they carry no meaning in a
// title.
func firstLine(text string) string {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		// Fenced code and horizontal rules open a block that says
		// nothing about the subject; skip the marker, keep reading.
		if line == "" || strings.HasPrefix(line, "```") || line == "---" {
			continue
		}
		if line = stripMarkers(line); line != "" {
			return line
		}
	}
	return ""
}

// stripMarkers removes leading block markers. A marker only counts
// when it is followed by a space, so "-rf ./build" keeps its flag and
// "#42 is broken" keeps its issue number.
func stripMarkers(line string) string {
	for {
		trimmed := strings.TrimLeft(line, "> \t")
		if trimmed != line {
			line = trimmed
			continue
		}
		if i := strings.IndexByte(line, ' '); i > 0 && isMarkerRun(line[:i]) {
			line = strings.TrimSpace(line[i+1:])
			continue
		}
		return strings.TrimSpace(line)
	}
}

// isMarkerRun reports whether tok is a run of one markdown block
// marker: "#", "###", "-", "*", "+".
func isMarkerRun(tok string) bool {
	if !strings.ContainsAny(tok[:1], "#-*+") {
		return false
	}
	return strings.Trim(tok, tok[:1]) == ""
}

// stripMentions removes Zulip mention syntax — @**Name**, @**Name|42**
// and the silent form @_**Name**_ — so a message addressed to the bot
// is not titled after the bot. A bare '@' that opens no mention is
// left alone: it is far more likely part of an address than of a
// handle Zulip would have rendered.
func stripMentions(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '@' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i + 1
		if j < len(s) && s[j] == '_' {
			j++
		}
		end := -1
		if strings.HasPrefix(s[j:], "**") {
			end = strings.Index(s[j+2:], "**")
		}
		if end < 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		i = j + 2 + end + 2
		// A silent mention closes with a trailing underscore.
		if i < len(s) && s[i] == '_' {
			i++
		}
	}
	return b.String()
}

// stripDecoration removes inline markdown that is noise in a title:
// emphasis, code ticks and link targets. The link TEXT is kept, the
// target dropped. A lone '_' or '#' is left alone — inside a word it
// is an identifier, not decoration.
func stripDecoration(s string) string {
	s = stripLinks(s)
	for _, tok := range []string{"**", "__", "~~", "`", "*", "[", "]"} {
		s = strings.ReplaceAll(s, tok, " ")
	}
	return s
}

// stripLinks rewrites [text](url) as text.
func stripLinks(s string) string {
	for {
		open := strings.Index(s, "](")
		if open < 0 {
			return s
		}
		end := strings.Index(s[open+2:], ")")
		if end < 0 {
			return s
		}
		s = s[:open] + s[open+2+end+1:]
	}
}

// isTrimmable reports whether r is punctuation or space that adds
// nothing at either end of a title. An interior '?' survives; a
// trailing one does not.
func isTrimmable(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
}

// truncate cuts to at most n CODE POINTS, on a word boundary when one
// is available in the last third, and never mid-word otherwise.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	cut := string(runes[:n])
	if i := strings.LastIndexByte(cut, ' '); i > 0 && len([]rune(cut[:i])) >= n/3 {
		cut = cut[:i]
	}
	return strings.TrimRightFunc(cut, isTrimmable)
}
