package live

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/reload"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// envExecPhase drives the three process images this test needs. A
// graceful reload is a syscall.Exec, which REPLACES the process image —
// so it cannot be performed by the process running `go test` without
// destroying the test binary. Instead the test re-runs itself:
//
//	phase ""  — the ordinary `go test` process: spawns phase 1 and
//	            asserts on its output.
//	phase 1   — registers a queue, posts a message into the topic, and
//	            then exec's ITSELF with reload's cursor env set. This is
//	            the reload window: the message is posted while NOBODY is
//	            polling.
//	phase 2   — the exec'd image. Resumes GetEvents on the INHERITED
//	            queue and must still receive that message.
//
// A control queue registered in phase 2 proves the point negatively:
// registering fresh — which is what every zulip-acp before this change
// did on restart — never sees the message at all.
const envExecPhase = "ZULIP_ACP_LIVE_EXEC_PHASE"

// TestEventQueueSurvivesReExec is the empirical basis for the whole
// graceful-reload design (internal/reload): a Zulip event queue is
// server-side state keyed by queue_id, not connection state, so it
// outlives the process that registered it and can be resumed — exactly
// — by a different process image with the same credentials.
//
// If this ever fails, the reload path is unsound and must go back to a
// hard restart: resuming a queue that did not survive would silently
// drop every message posted during the window.
func TestEventQueueSurvivesReExec(t *testing.T) {
	switch os.Getenv(envExecPhase) {
	case "1":
		execPhase1(t)
	case "2":
		execPhase2(t)
	default:
		liveClient(t) // gate + credential check in the parent too
		out := runExecChild(t)
		for _, want := range []string{"PHASE2 resumed queue", "PHASE2 RESUMED_OK", "PHASE2 FRESH_QUEUE_MISSED_IT"} {
			if !strings.Contains(out, want) {
				t.Fatalf("child output missing %q:\n%s", want, out)
			}
		}
	}
}

// runExecChild re-runs this one test in a child process with phase 1
// selected, and returns its combined output.
func runExecChild(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestEventQueueSurvivesReExec", "-test.v")
	cmd.Env = append(os.Environ(), envExecPhase+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exec child failed: %v\n%s", err, out)
	}
	return string(out)
}

// execPhase1 registers a queue, posts a message nobody is polling for,
// and hands the queue to a fresh process image.
func execPhase1(t *testing.T) {
	c, _ := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	me, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("me: %v", err)
	}

	res, rerr := c.Register(ctx, []string{zulipproto.EventMessage}, nil)
	if err := rerr; err != nil {
		t.Fatalf("register: %v", err)
	}
	fmt.Printf("PHASE1 registered %s at %d\n", res.QueueID, res.LastEventID)

	// The probe is a DM the bot sends to ITSELF. A channel message
	// would only reach the queue if the bot were subscribed to that
	// channel, and a self-DM is also invisible to the running relay,
	// which drops its own messages by sender id before any allowlist.
	marker := fmt.Sprintf("reload-probe %d", time.Now().UnixNano())
	if _, err := c.SendDirectMessage(ctx, []int64{me.UserID}, marker); err != nil {
		t.Fatalf("send: %v", err)
	}
	fmt.Printf("PHASE1 posted %q with nobody polling\n", marker)

	// Replace this image, carrying the cursor exactly as a reload does.
	cur := reload.Cursor{QueueID: res.QueueID, LastEventID: res.LastEventID}
	env := reload.Environ(os.Environ(), cur)
	env = append(env, "ZULIP_ACP_LIVE_EXEC_MARKER="+marker)
	for i, kv := range env {
		if strings.HasPrefix(kv, envExecPhase+"=") {
			env[i] = envExecPhase + "=2"
		}
	}
	if err := syscall.Exec(os.Args[0], os.Args, env); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// execPhase2 is the post-exec image: it resumes the inherited queue and
// must still be handed the message posted during the window, while a
// queue registered fresh right now must not be.
func execPhase2(t *testing.T) {
	c, _ := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	me, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("me: %v", err)
	}

	cur, ierr := reload.Inherited()
	if err := ierr; err != nil {
		t.Fatalf("inherited cursor: %v", err)
	}
	if !cur.Valid() {
		t.Fatal("no cursor inherited across exec")
	}
	fmt.Printf("PHASE2 resumed queue %s at %d\n", cur.QueueID, cur.LastEventID)

	// The control: what a hard restart does. Registering now puts the
	// cursor AFTER the probe message, so this queue can never see it.
	fresh, err := c.Register(ctx, []string{zulipproto.EventMessage}, nil)
	if err != nil {
		t.Fatalf("control register: %v", err)
	}

	marker := os.Getenv("ZULIP_ACP_LIVE_EXEC_MARKER")
	evs, err := c.GetEvents(ctx, cur.QueueID, cur.LastEventID)
	if err != nil {
		t.Fatalf("resume GetEvents on %s: %v", cur.QueueID, err)
	}
	found := false
	for _, ev := range evs {
		if ev.Message != nil && strings.Contains(ev.Message.Content, marker) {
			found = true
		}
	}
	if !found {
		t.Fatalf("resumed queue %s did not deliver %q (got %d events)", cur.QueueID, marker, len(evs))
	}
	fmt.Println("PHASE2 RESUMED_OK")

	// Post one more message so the control queue's long poll returns
	// promptly rather than burning its full ~90s window.
	if _, err := c.SendDirectMessage(ctx, []int64{me.UserID}, "control-wake"); err != nil {
		t.Fatalf("control wake send: %v", err)
	}
	cevs, err := c.GetEvents(ctx, fresh.QueueID, fresh.LastEventID)
	if err != nil {
		t.Fatalf("control GetEvents: %v", err)
	}
	for _, ev := range cevs {
		if ev.Message != nil && strings.Contains(ev.Message.Content, marker) {
			t.Fatalf("a freshly registered queue delivered %q — the premise of the control is wrong", marker)
		}
	}
	fmt.Println("PHASE2 FRESH_QUEUE_MISSED_IT")
	_ = c.DeleteQueue(ctx, fresh.QueueID)
	_ = c.DeleteQueue(ctx, cur.QueueID)
}
