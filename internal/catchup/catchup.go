// Package catchup reads the messages that arrived while the relay was
// down.
//
// A fresh /register hands back the server's CURRENT last_event_id, so
// every message posted before that instant is behind the cursor and is
// never delivered as an event. A graceful reload avoids this by
// inheriting the live queue (see internal/reload), but a cold start —
// a crash, a host reboot, a `systemctl stop` — has no queue to
// inherit. The catch-up closes that gap: the relay persists the id of
// the last message it processed, and after a fresh registration it
// reads forward from that id with GET /messages.
//
// This package owns only the mark and the pager. What a message MEANS
// — which conversation it belongs to, whether it is a command, how the
// missed messages of one topic become one turn — is the handler's
// business, behind Config.Deliver.
package catchup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// DefaultPageSize is how many messages one GET /messages reads.
const DefaultPageSize = 200

// DefaultMaxMessages bounds one catch-up read. A relay that was down
// for days in a busy realm must not spend its startup reading the
// whole history; the age limit applied by the handler would discard
// most of it anyway.
const DefaultMaxMessages = 5000

// Store is the persisted mark: the id of the last message the relay
// processed. Message ids are realm-global and increase monotonically,
// so one number covers every conversation.
//
// It is read on every message event, so the value is cached in memory
// and the file is written only when the mark moves forward.
type Store struct {
	path string
	id   atomic.Int64
}

// Open loads the mark at path. A missing file is an empty mark (0):
// the first run of a relay that has never recorded one.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("catchup: read mark: %w", err)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || id < 0 {
		return nil, fmt.Errorf("catchup: mark %s is malformed: %q", path, b)
	}
	s.id.Store(id)
	return s, nil
}

// Get returns the mark. 0 means none has been recorded.
func (s *Store) Get() int64 { return s.id.Load() }

// Set moves the mark forward to id and persists it. A value at or
// below the current mark is a no-op: the mark never moves back.
func (s *Store) Set(id int64) error {
	if id <= s.id.Load() {
		return nil
	}
	s.id.Store(id)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("catchup: mkdir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(id, 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("catchup: write mark: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("catchup: commit mark: %w", err)
	}
	return nil
}

// Source is the Zulip read surface the catch-up needs.
type Source interface {
	MessagesAfter(ctx context.Context, narrow []zulipproto.NarrowTerm, afterID int64, limit int) ([]zulipproto.Message, bool, error)
}

// Config configures Run.
type Config struct {
	Source Source
	Store  *Store
	// Deliver receives every message above the mark, oldest first,
	// in ONE call. The mark moves past them only after it returns.
	Deliver func(ctx context.Context, msgs []zulipproto.Message)
	// Newest is the server's max_message_id at the registration this
	// catch-up follows. With no mark recorded it becomes the mark.
	Newest int64
	// PageSize and MaxMessages: 0 uses the defaults.
	PageSize    int
	MaxMessages int
	// Attempts is how many times a failed read is tried; 0 uses
	// DefaultAttempts. Sleep waits between attempts; nil uses the real
	// clock. The event queue buffers meanwhile, so a short wait costs
	// nothing, and a read given up on leaves the gap unanswered for
	// good: live messages advance the mark past it.
	Attempts int
	Sleep    func(ctx context.Context, d time.Duration) error
	Logf     func(format string, args ...any)
}

// DefaultAttempts is how many times a failed catch-up read is tried.
const DefaultAttempts = 4

// retryDelay is the wait before the first retry; it doubles after each.
const retryDelay = 2 * time.Second

// Run performs one catch-up read, retrying a failed read with
// backoff. The error is the last attempt's.
func Run(ctx context.Context, cfg Config) error {
	attempts := cfg.Attempts
	if attempts <= 0 {
		attempts = DefaultAttempts
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cfg.Logf = logf
	delay := retryDelay
	var err error
	for i := 1; ; i++ {
		if err = runOnce(ctx, cfg); err == nil || i >= attempts {
			return err
		}
		logf("catchup: attempt %d failed: %v; retrying in %s", i, err, delay)
		if serr := sleep(ctx, delay); serr != nil {
			return err
		}
		delay *= 2
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runOnce performs one catch-up read.
//
// With no mark recorded it stores the newest message id and replays
// nothing: a relay installed today must not answer last month's
// traffic. Otherwise it pages forward from the mark until the server
// reports the newest message, hands everything to Deliver, and only
// then advances the mark.
//
// The read is not narrowed. A /messages narrow cannot express a union
// of channels plus direct messages, so it reads what the bot can see —
// its subscribed channels and its DMs — and the handler's gates decide,
// exactly as they do for the unnarrowed event queue.
func runOnce(ctx context.Context, cfg Config) error {
	logf := cfg.Logf
	page := cfg.PageSize
	if page <= 0 {
		page = DefaultPageSize
	}
	limit := cfg.MaxMessages
	if limit <= 0 {
		limit = DefaultMaxMessages
	}
	mark := cfg.Store.Get()
	if mark == 0 {
		// max_message_id is taken AT registration, so a message posted
		// after it is above the mark and still arrives as an event.
		logf("catchup: no mark recorded; starting from message %d (nothing is replayed)", cfg.Newest)
		return cfg.Store.Set(cfg.Newest)
	}
	var all []zulipproto.Message
	after := mark
	for {
		msgs, newest, err := cfg.Source.MessagesAfter(ctx, nil, after, page)
		if err != nil {
			return fmt.Errorf("catchup: read messages after %d: %w", after, err)
		}
		for _, m := range msgs {
			// The anchor is exclusive, but a message id at or below the
			// mark has been processed whatever the server says.
			if m.ID > after {
				all = append(all, m)
				after = m.ID
			}
		}
		if newest || len(msgs) == 0 {
			break
		}
		if len(all) >= limit {
			logf("catchup: WARN stopped after %d messages; anything newer than message %d that arrived while the relay was down is not replayed", len(all), after)
			break
		}
	}
	if len(all) == 0 {
		logf("catchup: nothing arrived since message %d", mark)
		return nil
	}
	logf("catchup: %d message(s) arrived while the relay was down (after message %d)", len(all), mark)
	cfg.Deliver(ctx, all)
	return cfg.Store.Set(after)
}
