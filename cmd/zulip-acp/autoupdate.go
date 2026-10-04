package main

import (
	"context"
	"errors"
	"strconv"

	"github.com/kfet/acp-kit/autoupdate"
	"github.com/kfet/acp-kit/update"
	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/updater"
)

// zulipAPI is the slice of the Zulip client the update offer needs.
type zulipAPI interface {
	SendDirectMessage(ctx context.Context, userIDs []int64, content string) (int64, error)
	EditMessage(ctx context.Context, id int64, content string) error
	AddReaction(ctx context.Context, messageID int64, emoji string) error
}

// offerEmoji maps the offer's reactions to decisions. The relay
// pre-adds the first three so an owner only has to tap one.
var offerEmoji = map[string]autoupdate.Decision{
	"check":       autoupdate.ApplyNow,
	"clock":       autoupdate.Tomorrow,
	"alarm_clock": autoupdate.Tomorrow,
	"no_entry":    autoupdate.SkipVersion,
}

// dmSurface shows the update offer as one DM to the update owners,
// edited in place.
type dmSurface struct {
	zc     zulipAPI
	owners []int64
}

func (s dmSurface) Post(ctx context.Context, text string) (string, error) {
	id, err := s.zc.SendDirectMessage(ctx, s.owners, text)
	if err != nil {
		return "", err
	}
	// Best effort: a missing chip only means typing the emoji by hand.
	for _, e := range []string{"check", "clock", "no_entry"} {
		_ = s.zc.AddReaction(ctx, id, e)
	}
	return strconv.FormatInt(id, 10), nil
}

func (s dmSurface) Edit(ctx context.Context, id, text string) error {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return errors.New("autoupdate: bad message id " + id)
	}
	return s.zc.EditMessage(ctx, n, text)
}

// autoUpdateDeps are the relay pieces the auto-updater drives.
type autoUpdateDeps struct {
	zc           zulipAPI
	botID        int64
	version      string
	relayBin     string
	agentBin     string
	agentVersion func() string
	upd          *update.Updater
	idle         func() bool
	health       func(ctx context.Context) error
	reload       func() error
	logf         func(string, ...any)
}

// autoUpdateWanted reports whether the auto-updater will run.
func autoUpdateWanted(cfg *config.Config) bool {
	mode, err := autoupdate.ParseMode(cfg.AutoUpdate)
	return err == nil && mode != autoupdate.Off && len(cfg.UpdateOwnerIDs) > 0
}

// newAutoUpdater builds the background auto-updater, or nil when it is
// off or there is nobody to ask.
func newAutoUpdater(cfg *config.Config, d autoUpdateDeps) (*autoupdate.Manager, error) {
	if !autoUpdateWanted(cfg) {
		return nil, nil
	}
	mode, _ := autoupdate.ParseMode(cfg.AutoUpdate)
	c := autoupdate.Config{
		Mode:         mode,
		Dist:         updater.Config(d.version),
		RelayName:    updater.Binary,
		RelayVersion: d.version,
		RelayBin:     d.relayBin,
		AgentBin:     d.agentBin,
		AgentVersion: d.agentVersion,
		Owners:       cfg.UpdateOwners(),
		StateDir:     cfg.StateDir,
		Surface:      dmSurface{zc: d.zc, owners: cfg.UpdateOwnerIDs},
		ConvID:       journal.DM(append(append([]int64{}, cfg.UpdateOwnerIDs...), d.botID)).Token(),
		Fleet:        cfg.IsFleetManaged(),
		LockFile:     cfg.FleetLockFile,
		Updater:      d.upd,
		Idle:         d.idle,
		Reload:       d.reload,
		HealthProbe:  d.health,
		QuietHours:   cfg.AutoUpdateQuietHours,
		Logf:         d.logf,
	}
	if d.agentBin != "" {
		c.UpdateAgent = update.CommandHook(d.agentBin, "update")
	}
	return autoupdate.New(c)
}

// updateDecider adapts reactions on the offer to Manager.Decide.
func updateDecider(m *autoupdate.Manager) func(ctx context.Context, msgID, userID int64, emoji string) bool {
	return func(ctx context.Context, msgID, userID int64, emoji string) bool {
		d, ok := offerEmoji[emoji]
		return ok && m.Decide(ctx, strconv.FormatInt(msgID, 10), strconv.FormatInt(userID, 10), d)
	}
}
