package live

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// TestQueueSwapOverlapIsLossless is the empirical basis for
// Runner.swapQueue (internal/zulipproto/events.go): when an inherited
// queue's registration is not the one this image wants, the
// replacement is registered FIRST, the old queue is then drained, and
// only then deleted.
//
// That is sound only if two things hold on a real server, and both are
// measured here rather than assumed:
//
//   - a queue registered EARLIER still delivers a message posted after
//     a second queue was registered — i.e. the old queue keeps
//     buffering while the replacement exists, so the overlap covers
//     the whole window;
//   - a message posted inside the overlap is delivered by BOTH queues,
//     which is precisely why the swap must de-duplicate on message id.
//
// If this ever fails, the swap is not lossless and the design must be
// revisited — not papered over.
func TestQueueSwapOverlapIsLossless(t *testing.T) {
	c, _ := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	me, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("me: %v", err)
	}

	// The outgoing queue: the one an image inherits and cannot resume.
	old, err := c.Register(ctx, []string{zulipproto.EventMessage}, nil, 0)
	if err != nil {
		t.Fatalf("register old queue: %v", err)
	}
	defer func() { _ = c.DeleteQueue(context.Background(), old.QueueID) }()

	// Posted while only the old queue exists: this is what a
	// delete-then-register swap loses.
	before := fmt.Sprintf("swap-probe-before %d", time.Now().UnixNano())
	if _, err := c.SendDirectMessage(ctx, []int64{me.UserID}, before); err != nil {
		t.Fatalf("send before: %v", err)
	}

	// Step 1 of the swap: the replacement is registered while the old
	// queue is still alive.
	fresh, err := c.Register(ctx, []string{zulipproto.EventMessage, zulipproto.EventReaction}, nil, 0)
	if err != nil {
		t.Fatalf("register replacement: %v", err)
	}
	defer func() { _ = c.DeleteQueue(context.Background(), fresh.QueueID) }()

	// Posted inside the overlap: both queues must see it.
	during := fmt.Sprintf("swap-probe-during %d", time.Now().UnixNano())
	if _, err := c.SendDirectMessage(ctx, []int64{me.UserID}, during); err != nil {
		t.Fatalf("send during: %v", err)
	}

	oldSeen := messageIDs(ctx, t, c, old.QueueID, old.LastEventID, before, during)
	if len(oldSeen) != 2 {
		t.Fatalf("the outgoing queue delivered %d of the 2 probes — it stopped buffering once a second queue existed, and the swap is NOT lossless", len(oldSeen))
	}
	newSeen := messageIDs(ctx, t, c, fresh.QueueID, fresh.LastEventID, before, during)
	if _, ok := newSeen[during]; !ok {
		t.Fatal("the replacement queue did not deliver the overlap message")
	}
	if _, ok := newSeen[before]; ok {
		t.Fatal("the replacement queue delivered a message posted before it existed — the premise that /register skips history is wrong")
	}
	// The overlap message arrived twice, under two unrelated per-queue
	// event ids, with ONE message id. That message id is what the
	// runner's dedup keys on.
	if oldSeen[during] != newSeen[during] {
		t.Fatalf("message ids differ across queues (%d vs %d) — dedup on message id would not work",
			oldSeen[during], newSeen[during])
	}
}

// messageIDs polls one queue once and returns marker → message id for
// whichever probe markers it delivered.
func messageIDs(ctx context.Context, t *testing.T, c *zulipproto.Client, queueID string, last int64, markers ...string) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	evs, err := c.GetEvents(ctx, queueID, last)
	if err != nil {
		t.Fatalf("GetEvents on %s: %v", queueID, err)
	}
	for _, ev := range evs {
		if ev.Message == nil {
			continue
		}
		for _, m := range markers {
			if strings.Contains(ev.Message.Content, m) {
				got[m] = ev.Message.ID
			}
		}
	}
	return got
}
