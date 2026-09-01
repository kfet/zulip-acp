// Package channels owns the set of Zulip channels the relay serves.
//
// Two shapes, one type:
//
//   - Explicit: the ids resolved from the operator's `channels` list.
//     Static for the process lifetime; config is authoritative, so an
//     explicit channel is served whether or not a subscription event
//     ever mentions it.
//   - Followed: the channels the bot is SUBSCRIBED to, requested with
//     the "*" sentinel. This half is live — it is seeded from
//     GET /users/me/subscriptions at boot and then maintained from
//     subscription/stream events, so adding the bot to a channel
//     starts serving it with no config edit and no restart.
//
// The set is written by the event loop and read by the handler on its
// turn goroutines, so every accessor is guarded.
package channels

import (
	"sort"
	"sync"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// Config configures a Set.
type Config struct {
	// Explicit is the resolved static allowlist: stream id → name.
	Explicit map[int64]string
	// Follow enables the subscription-following half of the set.
	Follow bool
	// Logf receives join/leave messages. Optional.
	Logf func(format string, args ...any)
}

// Set is the served-channel set.
type Set struct {
	follow bool
	logf   func(format string, args ...any)

	mu       sync.RWMutex
	explicit map[int64]string
	followed map[int64]string
}

// New builds a Set. The explicit map is copied, so the caller may keep
// using its own.
func New(cfg Config) *Set {
	s := &Set{
		follow:   cfg.Follow,
		logf:     cfg.Logf,
		explicit: make(map[int64]string, len(cfg.Explicit)),
		followed: map[int64]string{},
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	for id, name := range cfg.Explicit {
		s.explicit[id] = name
	}
	return s
}

// Name returns the channel's name and whether it is served. It is the
// handler's allowlist check.
func (s *Set) Name(id int64) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if name, ok := s.explicit[id]; ok {
		return name, true
	}
	name, ok := s.followed[id]
	return name, ok
}

// Names returns the served channel names, sorted. Used to decide
// whether the event queue can be narrowed, and for logging.
func (s *Set) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.explicit)+len(s.followed))
	for _, name := range s.explicit {
		out = append(out, name)
	}
	for id, name := range s.followed {
		if _, dup := s.explicit[id]; !dup {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Len returns how many distinct channels are served.
func (s *Set) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.explicit)
	for id := range s.followed {
		if _, dup := s.explicit[id]; !dup {
			n++
		}
	}
	return n
}

// Sync replaces the followed half with the bot's current
// subscriptions, logging the difference. It is a no-op unless the set
// follows subscriptions.
//
// Events are lost while an event queue is dead, so this runs on every
// (re-)registration: without it the followed set drifts silently away
// from the realm and stays wrong until a restart.
func (s *Set) Sync(subs []zulipproto.Stream) {
	if !s.follow {
		return
	}
	next := make(map[int64]string, len(subs))
	for _, sub := range subs {
		next[sub.StreamID] = sub.Name
	}
	// The diff is computed under the lock and logged after it: `next`
	// becomes shared state the moment it is published, so iterating it
	// unlocked would race an event arriving on another goroutine.
	type change struct {
		id   int64
		name string
	}
	var joined, left []change
	s.mu.Lock()
	prev := s.followed
	for id, name := range next {
		if _, had := prev[id]; had {
			continue
		}
		if _, dup := s.explicit[id]; !dup {
			joined = append(joined, change{id, name})
		}
	}
	for id, name := range prev {
		if _, still := next[id]; still {
			continue
		}
		if _, dup := s.explicit[id]; !dup {
			left = append(left, change{id, name})
		}
	}
	s.followed = next
	s.mu.Unlock()

	for _, c := range joined {
		s.logf("channels: now serving #%s (%d) — the bot is subscribed to it", c.name, c.id)
	}
	for _, c := range left {
		s.logf("channels: no longer serving #%s (%d) — the bot is not subscribed to it", c.name, c.id)
	}
}

// Apply folds one event into the set. Everything it does not
// understand is ignored, so it is safe to feed it the whole stream.
func (s *Set) Apply(ev zulipproto.Event) {
	if !s.follow {
		return
	}
	switch ev.Type {
	case zulipproto.EventSubscription:
		switch ev.Op {
		case "add":
			for _, sub := range ev.Subscriptions {
				s.add(sub)
			}
		case "remove":
			for _, sub := range ev.Subscriptions {
				s.remove(sub.StreamID, sub.Name)
			}
		}
	case zulipproto.EventStream:
		switch ev.Op {
		case "delete":
			// An archived channel takes its subscription with it, and
			// Zulip does not always follow up with subscription
			// op=remove.
			for _, st := range ev.Streams {
				s.remove(st.StreamID, st.Name)
			}
		case "update":
			if name, ok := ev.RenamedTo(); ok {
				s.rename(ev.StreamID, name)
			}
		}
	}
}

func (s *Set) add(sub zulipproto.Stream) {
	s.mu.Lock()
	_, had := s.followed[sub.StreamID]
	_, dup := s.explicit[sub.StreamID]
	s.followed[sub.StreamID] = sub.Name
	s.mu.Unlock()
	if !had && !dup {
		s.logf("channels: now serving #%s (%d) — the bot was subscribed to it", sub.Name, sub.StreamID)
	}
}

func (s *Set) remove(id int64, name string) {
	s.mu.Lock()
	known, had := s.followed[id]
	delete(s.followed, id)
	_, dup := s.explicit[id]
	s.mu.Unlock()
	if !had || dup {
		// Never subscribed, or pinned by config: config wins.
		return
	}
	if name == "" {
		name = known
	}
	s.logf("channels: no longer serving #%s (%d) — the bot was unsubscribed from it", name, id)
}

func (s *Set) rename(id int64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.followed[id]; ok {
		s.followed[id] = name
	}
	if _, ok := s.explicit[id]; ok {
		s.explicit[id] = name
	}
}
