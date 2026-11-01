package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TopicStatusMode is how the relay shows in the topic list whether
// agent work is live in a topic. See internal/handler/topicstatus.go.
type TopicStatusMode string

const (
	// TopicStatusOff leaves topic names alone.
	TopicStatusOff TopicStatusMode = "off"
	// TopicStatusResolve uses Zulip's native resolve mark (✔): resolved
	// while idle, unresolved while a turn runs or a schedule is
	// pending. Every resolve makes Notification Bot post a notice.
	TopicStatusResolve TopicStatusMode = "resolve"
	// TopicStatusEmoji puts a glyph in front of the topic name: one
	// while a turn runs, one while the agent waits for the user, and
	// none while idle. A plain topic move posts no notice.
	TopicStatusEmoji TopicStatusMode = "emoji"
)

// UnmarshalJSON accepts "resolve", "emoji", "off", and the old boolean
// form: true is the default mode, false is off.
func (m *TopicStatusMode) UnmarshalJSON(b []byte) error {
	var on bool
	if err := json.Unmarshal(b, &on); err == nil {
		*m = TopicStatusOff
		if on {
			*m = DefaultTopicStatus
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("topic_status must be \"resolve\", \"emoji\", \"off\" or a boolean")
	}
	switch v := TopicStatusMode(s); v {
	case TopicStatusOff, TopicStatusResolve, TopicStatusEmoji:
		*m = v
		return nil
	}
	return fmt.Errorf("topic_status %q: want \"resolve\", \"emoji\" or \"off\"", s)
}

// DefaultTopicStatus is the mode when topic_status is unset.
const DefaultTopicStatus = TopicStatusEmoji

// The default status glyphs. Both are monochrome symbols with no emoji
// presentation in Unicode (Emoji_Presentation=No, and not in the emoji
// set at all), so web, desktop and the Flutter mobile app draw them in
// the text colour, like a letter, rather than as a colour picture.
// The colour candidates ⌛ U+231B and ❔ U+2754 are refused for that
// reason: both default to emoji presentation.
const (
	// DefaultStatusBusy is U+29D7 BLACK HOURGLASS.
	DefaultStatusBusy = "⧗"
	// DefaultStatusQuestion is U+2047 DOUBLE QUESTION MARK.
	DefaultStatusQuestion = "⁇"
)

// maxStatusGlyph bounds a status glyph, in code points. A prefix is a
// marker, not a label: it costs topic-name length on every topic.
const maxStatusGlyph = 4

// GetTopicStatus returns the topic status mode. Unset means
// DefaultTopicStatus.
func (c *Config) GetTopicStatus() TopicStatusMode {
	if c.TopicStatus == nil {
		return DefaultTopicStatus
	}
	return *c.TopicStatus
}

// GetStatusBusy returns the glyph of a topic with a turn running.
func (c *Config) GetStatusBusy() string {
	if c.StatusBusy == nil {
		return DefaultStatusBusy
	}
	return *c.StatusBusy
}

// GetStatusQuestion returns the glyph of a topic that waits for the
// user.
func (c *Config) GetStatusQuestion() string {
	if c.StatusQuestion == nil {
		return DefaultStatusQuestion
	}
	return *c.StatusQuestion
}

// validateTopicStatus refuses glyphs that cannot work as a prefix. A
// glyph with a space would not round-trip through the prefix strip; two
// equal glyphs, or one equal to Zulip's ✔, would make two states one.
func (c *Config) validateTopicStatus() error {
	busy, question := c.GetStatusBusy(), c.GetStatusQuestion()
	for name, g := range map[string]string{"status_busy": busy, "status_question": question} {
		if g == "" || utf8.RuneCountInString(g) > maxStatusGlyph || strings.ContainsFunc(g, unicode.IsSpace) {
			return fmt.Errorf("%s %q: want 1 to %d characters with no spaces", name, g, maxStatusGlyph)
		}
		if g == "✔" {
			return fmt.Errorf("%s must not be ✔: that is Zulip's resolved mark", name)
		}
	}
	if busy == question {
		return fmt.Errorf("status_busy and status_question must differ")
	}
	return nil
}

// OffersTopicStatus reports whether the relay offers the agent the
// `topic_status` loopback tool: only in emoji mode, and only with the
// loopback on.
func (c *Config) OffersTopicStatus() bool {
	return c.RelayMCP && c.GetTopicStatus() == TopicStatusEmoji
}
