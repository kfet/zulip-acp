package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// Degraded is one config entry that the relay turned off at start,
// because it names a channel that Zulip does not have (or that the bot
// cannot see). A missing channel turns off only the entry that names
// it: the relay keeps the rest of its config and keeps running. A
// renamed channel must never put the relay into a crash loop.
type Degraded struct {
	// Field is the config key that holds the bad name, e.g.
	// "routes[0].from".
	Field string
	// Want is the channel name or id as the config gives it.
	Want string
	// Why tells what is wrong with the entry.
	Why string
	// Effect tells what is off now, in plain words.
	Effect string
	// Fix tells the operator what to change.
	Fix string
}

// String is the one-line report of d, used in the log, the owner DM
// and `!status`.
func (d Degraded) String() string {
	return fmt.Sprintf("%s: %s → %s. Fix: %s, then reload.", d.Field, d.Why, d.Effect, d.Fix)
}

// Degraded returns every entry the Resolve* calls turned off, in the
// order they were found.
func (c *Config) Degraded() []Degraded { return append([]Degraded(nil), c.degraded...) }

// degrade records that the entry at field is off, because it names a
// channel the bot cannot see.
func (c *Config) degrade(field, want, effect string) {
	want = strings.TrimSpace(want)
	c.degraded = append(c.degraded, Degraded{
		Field: field, Want: want, Effect: effect,
		Why: fmt.Sprintf("channel %q not visible to the bot", want),
		Fix: fmt.Sprintf("put the channel's current name in %s (names are case-sensitive; a renamed channel needs its new name), or subscribe the bot", field),
	})
}

// lookup resolves one channel: by id when want is a number, else by
// name (case-sensitive, as Zulip's are). On failure it records the problem with
// effect and returns ok=false.
func (c *Config) lookup(field, want, effect string, available []zulipproto.Stream) (int64, bool) {
	want = strings.TrimSpace(want)
	id, numeric := strconv.ParseInt(want, 10, 64)
	for _, s := range available {
		if (numeric == nil && s.StreamID == id) || (numeric != nil && s.Name == want) {
			return s.StreamID, true
		}
	}
	c.degrade(field, want, effect)
	return 0, false
}

// lookupList resolves a list of channels; nil for an empty list. Every
// bad entry is recorded, not only the first. ok=false when any entry
// is bad: the caller then turns off the WHOLE entry, because a source
// list that silently lost a channel would change what the entry does,
// and one that lost every channel would mean "all channels".
func (c *Config) lookupList(field string, wants []string, effect string, available []zulipproto.Stream) ([]int64, bool) {
	var out []int64
	ok := true
	for _, w := range wants {
		id, good := c.lookup(field, w, effect, available)
		ok = ok && good
		out = append(out, id)
	}
	if !ok {
		return nil, false
	}
	return out, true
}

// lookupAt resolves the channel of a "channel/topic" address.
func (c *Config) lookupAt(field, at, effect string, available []zulipproto.Stream) (int64, string, bool) {
	ch, topic, _ := splitAt(at)
	id, ok := c.lookup(field, ch, effect, available)
	return id, topic, ok
}
