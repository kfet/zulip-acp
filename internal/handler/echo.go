package handler

import (
	"context"
	"sync"
	"time"
)

// reactEchoTimeout is how long reactWhenVisible waits for the echo of
// the relay's own message before it adds the reactions anyway.
const reactEchoTimeout = 2 * time.Second

// ownEchoRecent is how many echoed message ids ownEcho remembers, so
// an echo that arrives before its waiter registers still resolves.
const ownEchoRecent = 256

// ownEcho tracks the echo of the relay's own messages on the event
// stream.
//
// A live client (the mobile app, for example) drops a reaction event
// for a message it has not received yet. The relay posts a message and
// adds reactions a few milliseconds later, so without this wait the
// reaction events can reach the client first and the emoji show only
// after a reload. The echo of the message on our own event queue is the
// proof that the server has fanned the message out.
type ownEcho struct {
	mu      sync.Mutex
	waiters map[int64][]chan struct{}
	recent  map[int64]struct{}
	order   []int64
}

func newOwnEcho() *ownEcho {
	return &ownEcho{waiters: map[int64][]chan struct{}{}, recent: map[int64]struct{}{}}
}

// seen records the echo of message id and releases its waiters.
func (e *ownEcho) seen(id int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range e.waiters[id] {
		close(ch)
	}
	delete(e.waiters, id)
	if _, ok := e.recent[id]; ok {
		return
	}
	e.recent[id] = struct{}{}
	e.order = append(e.order, id)
	if len(e.order) > ownEchoRecent {
		delete(e.recent, e.order[0])
		e.order = e.order[1:]
	}
}

// wait returns a channel that is closed when the echo of id is seen.
// If the echo was already seen, the channel is closed at once. cancel
// removes the waiter.
func (e *ownEcho) wait(id int64) (ch <-chan struct{}, cancel func()) {
	c := make(chan struct{})
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.recent[id]; ok {
		close(c)
		return c, func() {}
	}
	e.waiters[id] = append(e.waiters[id], c)
	return c, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		ws := e.waiters[id]
		for i, w := range ws {
			if w == c {
				ws = append(ws[:i], ws[i+1:]...)
				break
			}
		}
		if len(ws) == 0 {
			delete(e.waiters, id)
		} else {
			e.waiters[id] = ws
		}
	}
}

// awaitEcho blocks until the echo of msgID is seen or reactEchoTimeout
// passes. It must never run on the event loop: the echo arrives there.
func (h *Handler) awaitEcho(msgID int64) {
	ch, cancel := h.ownEcho.wait(msgID)
	defer cancel()
	select {
	case <-ch:
	case <-h.echoAfter(reactEchoTimeout):
	}
}

func (h *Handler) echoAfter(d time.Duration) <-chan time.Time {
	if h.cfg.EchoAfter != nil {
		return h.cfg.EchoAfter(d)
	}
	return time.After(d)
}

// reactWhenVisible adds emojis to the relay's own message msgID, in
// order, once the message has echoed back (or after reactEchoTimeout).
// It returns at once; the work runs on its own goroutine. ok, if not
// nil, is asked just before the reactions are added; false skips them.
// what names the message in the log.
func (h *Handler) reactWhenVisible(ctx context.Context, msgID int64, what string, ok func() bool, emojis ...string) {
	ctx = context.WithoutCancel(ctx)
	h.echoWG.Add(1)
	h.reacting.add()
	go func() {
		defer h.echoWG.Done()
		defer h.reacting.done()
		h.awaitEcho(msgID)
		if ok != nil && !ok() {
			return
		}
		for _, e := range emojis {
			if err := h.cfg.Client.AddReaction(ctx, msgID, e); err != nil {
				h.cfg.Logf("handler: could not pre-add :%s: to %s %d: %v", e, what, msgID, err)
			}
		}
	}()
}

// reactWait bounds how long settleStatus waits for pre-added reactions.
const reactWait = reactEchoTimeout + 5*time.Second

// inflightReacts counts reactWhenVisible goroutines that have not yet
// added their reactions.
//
// The topic status mark moves the whole topic. The macOS client applies
// that move from its cached copy of a message, and a reaction that
// arrives around the move is lost there until a reload. settleStatus
// waits on this so the reactions land before the topic moves.
type inflightReacts struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n is 0; nil means idle
}

func (r *inflightReacts) add() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		r.idle = make(chan struct{})
	}
	r.n++
}

func (r *inflightReacts) done() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n--
	if r.n == 0 {
		close(r.idle)
		r.idle = nil
	}
}

// wait blocks until no reactions are in flight, or until d elapses.
// It reports whether it ended idle.
func (r *inflightReacts) wait(d time.Duration) bool {
	r.mu.Lock()
	ch := r.idle
	r.mu.Unlock()
	if ch == nil {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}
