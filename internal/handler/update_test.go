package handler

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/update"
)

func testUpdater(t *testing.T, h **Handler, reloadErr error, reloads *int) *update.Updater {
	t.Helper()
	return update.New(update.Config{
		RelayName: "zulip-acp", RelayVersion: "0.1.0", RelayBin: "/relay",
		Owners:     []string{strconv.FormatInt(humanID, 10)},
		StateDir:   t.TempDir(),
		UpdateSelf: func(context.Context) (string, error) { return "", nil },
		CancelAll:  func() []string { return (*h).CancelAll() },
		WaitIdle:   func(ctx context.Context) error { return (*h).WaitCancelled(ctx) },
		Reload:     func() error { *reloads++; return reloadErr },
		Version:    func(context.Context, string) string { return "v" },
	})
}

func TestUpdateCommandForceCancelsAndReloads(t *testing.T) {
	agent := newAgent("x")
	agent.block = make(chan struct{})
	var h *Handler
	reloads := 0
	hh := cmdHarness(t, agent, func(c *Config) {
		c.AckEmoji = "eyes"
		c.Updater = testUpdater(t, &h, nil, &reloads)
	})
	h = hh.h
	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", mention("go")))
	<-agent.entered
	hh.z.reset()

	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", "!update relay --force"))
	hh.awaitTurnCleanup(t)
	got := strings.Join(hh.z.stored(), "\n")
	if !strings.Contains(got, "Cancelled turns in: #") || !strings.Contains(got, "hacking") || reloads != 1 {
		t.Fatalf("reloads=%d reply=%q", reloads, got)
	}
	if len(hh.s.cancels) == 0 {
		t.Fatal("turn not cancelled")
	}
}

func TestUpdateCommandOwnerOnlyAndHelp(t *testing.T) {
	var h *Handler
	reloads := 0
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) { c.Updater = testUpdater(t, &h, errors.New("boom"), &reloads) })
	h = hh.h
	hh.deliverDM(t, humanID, "!help", humanID, botID)
	if !strings.Contains(hh.only(t), "!update") {
		t.Fatalf("help = %q", hh.only(t))
	}
	hh.z.reset()
	hh.deliverDM(t, humanID, "!update relay", humanID, botID)
	got := strings.Join(hh.z.stored(), "\n")
	if !strings.Contains(got, "Reload failed: boom") || reloads != 1 {
		t.Fatalf("reply = %q", got)
	}
	if out := h.CancelAll(); len(out) != 0 {
		t.Fatal(out)
	}
}

func TestUpdateCommandRefusesNonOwner(t *testing.T) {
	reloads := 0
	u := update.New(update.Config{Owners: []string{"1"}, StateDir: t.TempDir(), Reload: func() error { reloads++; return nil }})
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) { c.Updater = u })
	hh.deliverDM(t, humanID, "!update", humanID, botID)
	if !strings.Contains(hh.only(t), "owner-only") || reloads != 0 {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

func TestWaitCancelledHonoursContext(t *testing.T) {
	agent := newAgent("x")
	agent.block = make(chan struct{})
	hh := cmdHarness(t, agent, nil)
	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", mention("go")))
	<-agent.entered
	hh.h.inflightMu.Lock()
	hh.h.cancelled = append(hh.h.cancelled, make(chan struct{}))
	hh.h.inflightMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hh.h.WaitCancelled(ctx); err == nil {
		t.Fatal("want ctx error")
	}
	close(agent.block)
	waitIdle(t, hh)
}

// TestUpdateCommandFleetReportsThroughPost: on a fleet host `!update`
// runs the converge job, and the job's report reaches the SAME
// conversation after the command has returned.
func TestUpdateCommandFleetReportsThroughPost(t *testing.T) {
	u := update.New(update.Config{
		RelayName: "zulip-acp", RelayVersion: "0.1.0", RelayBin: "/relay",
		Owners: []string{strconv.FormatInt(humanID, 10)}, StateDir: t.TempDir(),
		Fleet: true, ConvergeCmd: "true", PollInterval: 1,
		Version: func(context.Context, string) string { return "v" },
	})
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) { c.Updater = u })
	report := make(chan string, 1)
	hh.z.mu.Lock()
	hh.z.sendHook = func(s string) error {
		if strings.Contains(s, "up to date") {
			report <- s
		}
		return nil
	}
	hh.z.mu.Unlock()
	hh.deliverDM(t, humanID, "!update", humanID, botID)
	if got := <-report; !strings.HasPrefix(got, "✅ Already up to date.") {
		t.Fatal(got)
	}
	if got := strings.Join(hh.z.stored(), "\n"); !strings.Contains(got, "running converge") {
		t.Fatal(got)
	}
}
