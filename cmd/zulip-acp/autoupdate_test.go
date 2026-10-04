package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/update"
	"github.com/kfet/zulip-acp/internal/config"
)

type fakeZulip struct {
	sent      []string
	edits     map[int64]string
	reactions []string
	sendErr   error
}

func (f *fakeZulip) SendDirectMessage(_ context.Context, _ []int64, content string) (int64, error) {
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.sent = append(f.sent, content)
	return int64(len(f.sent)), nil
}

func (f *fakeZulip) EditMessage(_ context.Context, id int64, content string) error {
	if f.edits == nil {
		f.edits = map[int64]string{}
	}
	f.edits[id] = content
	return nil
}

func (f *fakeZulip) AddReaction(_ context.Context, _ int64, emoji string) error {
	f.reactions = append(f.reactions, emoji)
	return nil
}

func TestDMSurface(t *testing.T) {
	z := &fakeZulip{}
	s := dmSurface{zc: z, owners: []int64{7}}
	ctx := context.Background()
	id, err := s.Post(ctx, "offer")
	if err != nil || id != "1" || strings.Join(z.reactions, ",") != "check,clock,no_entry" {
		t.Fatal(id, err, z.reactions)
	}
	if err := s.Edit(ctx, "1", "done"); err != nil || z.edits[1] != "done" {
		t.Fatal(err)
	}
	if s.Edit(ctx, "x", "y") == nil {
		t.Fatal("bad id")
	}
	z.sendErr = errors.New("down")
	if _, err := s.Post(ctx, "x"); err == nil {
		t.Fatal("want error")
	}
}

func TestNewAutoUpdater(t *testing.T) {
	dir := t.TempDir()
	deps := autoUpdateDeps{zc: &fakeZulip{}, botID: 1, version: "0.1.0", relayBin: dir + "/relay",
		reload: func() error { return nil }}
	cfg := &config.Config{StateDir: dir}
	if m, err := newAutoUpdater(cfg, deps); m != nil || err != nil {
		t.Fatal("no owners: off")
	}
	cfg.UpdateOwnerIDs = []int64{7}
	cfg.AutoUpdate = "off"
	if m, _ := newAutoUpdater(cfg, deps); m != nil {
		t.Fatal("off")
	}
	cfg.AutoUpdate = ""
	cfg.AutoUpdateQuietHours = "bad"
	if _, err := newAutoUpdater(cfg, deps); err == nil {
		t.Fatal("bad quiet hours")
	}
	cfg.AutoUpdateQuietHours = ""
	deps.agentBin = dir + "/fir"
	m, err := newAutoUpdater(cfg, deps)
	if err != nil || m == nil || !strings.Contains(m.Status(), "stage") {
		t.Fatal(m, err)
	}
	decide := updateDecider(m)
	ctx := context.Background()
	if decide(ctx, 1, 7, "tada") || decide(ctx, 1, 7, "check") {
		t.Fatal("no offer pending: nothing decided")
	}
	cfg.FleetLockFile = dir + "/dist.lock"
	deps.upd = update.New(update.Config{StateDir: dir})
	if m, err := newAutoUpdater(cfg, deps); err != nil || !strings.Contains(m.Status(), "fleet") {
		t.Fatal(err)
	}
}
