// zulip-acp bridges a self-hosted Zulip server to an ACP-speaking
// coding agent (fir --mode acp, Claude Code, …). Each Zulip topic,
// scoped by its channel, maps to one ACP session.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/acp-kit/state"

	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/handler"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/sysprompt"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to JSON config file")
	agentCmd := flag.String("agent-cmd", "", "agent argv (default: fir --mode acp); space-separated; overrides config")
	stateDir := flag.String("state-dir", "", "root directory for per-conversation state")
	channelsFlag := flag.String("channels", "", "comma-separated Zulip channel names or ids to serve; overrides config")
	showVersion := flag.Bool("version", false, "print version and exit")
	printPaths := flag.Bool("print-paths", false, "print resolved config, state dir and agent command, then exit")
	flag.Parse()

	kitlog.Register("ZULIP_ACP_DEBUG")

	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg := &config.Config{}
	if *configPath != "" {
		c, err := config.Load(*configPath)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		cfg = c
	}
	// Environment overrides, so the API key never has to live in a
	// config file on the host.
	if v := os.Getenv("ZULIP_SITE"); v != "" {
		cfg.Site = v
	}
	if v := os.Getenv("ZULIP_EMAIL"); v != "" {
		cfg.BotEmail = v
	}
	if v := os.Getenv("ZULIP_API_KEY"); v != "" {
		cfg.BotAPIKey = v
	}
	if *agentCmd != "" {
		cfg.AgentCmd = strings.Fields(*agentCmd)
	}
	if *channelsFlag != "" {
		cfg.Channels = splitList(*channelsFlag)
	}
	if *stateDir != "" {
		cfg.StateDir = *stateDir
	}
	if cfg.StateDir == "" {
		cfg.StateDir = config.DefaultStateDir()
	}

	if *printPaths {
		cp := *configPath
		if cp == "" {
			cp = "(none; using env + defaults)"
		}
		fmt.Printf("config:     %s\n", cp)
		fmt.Printf("state-dir:  %s\n", cfg.StateDir)
		fmt.Printf("agent-cmd:  %s\n", strings.Join(cfg.GetAgentCmd(), " "))
		fmt.Printf("site:       %s\n", cfg.Site)
		fmt.Printf("channels:   %s\n", strings.Join(cfg.Channels, ", "))
		fmt.Printf("dms:        %t\n", cfg.DMs)
		return
	}

	if err := config.ValidateCredentials(cfg.Site, cfg.BotEmail, cfg.BotAPIKey); err != nil {
		log.Fatalf("zulip-acp: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		log.Fatalf("state dir: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	zc, err := zulipproto.New(zulipproto.Config{Site: cfg.Site, Email: cfg.BotEmail, APIKey: cfg.BotAPIKey})
	if err != nil {
		log.Fatalf("zulip: %v", err)
	}
	me, err := zc.Me(ctx)
	if err != nil {
		log.Fatalf("zulip: cannot authenticate as %s: %v\n  → check site, bot_email and bot_api_key.", cfg.BotEmail, err)
	}
	log.Printf("zulip-acp %s: authenticated as %s (user %d)", version, me.FullName, me.UserID)

	// Snapshot the realm's bots so the relay never answers one. The
	// cross-realm system bots are not in this list; they are caught by
	// their sender realm instead.
	users, err := zc.Users(ctx)
	if err != nil {
		log.Fatalf("zulip: list users: %v", err)
	}
	bots := map[int64]struct{}{}
	for _, u := range users {
		if u.IsBot {
			bots[u.UserID] = struct{}{}
		}
	}

	streams, err := zc.Streams(ctx)
	if err != nil {
		log.Fatalf("zulip: list channels: %v", err)
	}
	explicit, err := cfg.ResolveChannels(streams)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	follow := cfg.FollowsSubscriptions()
	served := channels.New(channels.Config{Explicit: explicit, Follow: follow, Logf: log.Printf})
	if follow {
		// The set itself is seeded by the OnRegister hook below, which
		// fires on the first registration too — one code path for the
		// boot case and the queue-died case both.
		log.Printf("zulip-acp: following the bot's subscriptions (%q in channels); subscribe it to a channel to have it served, no restart needed", config.ChannelSentinel)
	}
	labels := make([]string, 0, len(explicit))
	for id, name := range explicit {
		labels = append(labels, fmt.Sprintf("#%s (%d)", name, id))
	}
	sort.Strings(labels)
	if len(labels) > 0 {
		log.Printf("zulip-acp: serving %s", strings.Join(labels, ", "))
	}
	if cfg.DMs {
		log.Printf("zulip-acp: serving direct messages (every DM is addressed to the bot, so mention-gating is off)")
	}
	// Narrowing the event queue is an optimisation, not the allowlist:
	// a /register narrow cannot express a union of channels, so with
	// more than one served channel the queue is left unnarrowed and
	// the handler filters. A followed set can grow at any moment, so
	// it is never narrowed — and a channel narrow would drop every DM
	// on the floor, so serving DMs drops it too. See
	// zulipproto.NarrowChannels.
	var narrow [][2]string
	if !follow {
		narrow = zulipproto.NarrowChannels(served.Names(), cfg.DMs)
	}
	switch {
	case follow:
		log.Printf("zulip-acp: the event queue is not narrowed: the served set follows the bot's subscriptions and can change at any moment")
	case narrow == nil && cfg.DMs:
		log.Printf("zulip-acp: the event queue is not narrowed: a channel narrow would exclude direct messages; the allowlists filter")
	case narrow == nil:
		log.Printf("zulip-acp: the event queue is not narrowed (%d channels served); the channel allowlist filters", served.Len())
	}

	eventTypes := []string{zulipproto.EventMessage, zulipproto.EventUpdateMessage}
	var onRegister func(context.Context)
	if follow {
		// Subscription changes are what move the served set, and
		// stream events carry renames and archivals of channels
		// already in it.
		eventTypes = append(eventTypes, zulipproto.EventSubscription, zulipproto.EventStream)
		// Events are lost while a queue is dead, so resync the set on
		// every registration rather than let it drift until restart.
		onRegister = func(ctx context.Context) {
			subs, err := zc.Subscriptions(ctx)
			if err != nil {
				log.Printf("zulip: resync subscriptions: %v", err)
				return
			}
			served.Sync(subs)
		}
	}

	jr, err := journal.Open(filepath.Join(cfg.StateDir, "journal.json"))
	if err != nil {
		log.Fatalf("journal: %v", err)
	}

	agent, err := client.Start(ctx, cfg.AgentClientConfig(os.Stderr))
	if err != nil {
		log.Fatalf("agent start: %v", err)
	}
	defer agent.Close()
	log.Printf("zulip-acp: agent up (caps=%+v)", agent.Caps())

	sessions, err := state.New(state.Config{
		Agent:        agent,
		StateDir:     cfg.StateDir,
		IdleTimeout:  cfg.IdleTimeout(),
		SystemPrompt: sysprompt.Resolve(cfg.SystemPrompt, cfg.DisableSystemPrompt, "", cfg.GetSilentSentinel()),
	})
	if err != nil {
		log.Fatalf("state: %v", err)
	}
	defer func() { _ = sessions.Close() }()
	go sessions.Run(ctx)

	// The chat-command broker is shared with poe-acp (acp-kit/command).
	// handler.New wires the Handler in as its Controller, so nothing
	// here calls SetController.
	broker := command.New(agent)

	h, err := handler.New(handler.Config{
		Client:             zc,
		Agent:              agent,
		Commands:           broker,
		Version:            version,
		AgentCmd:           strings.Join(cfg.GetAgentCmd(), " "),
		StartTime:          time.Now(),
		Sessions:           sessions,
		Journal:            jr,
		BotUserID:          me.UserID,
		BotFullName:        me.FullName,
		BotSenderIDs:       bots,
		Channels:           served,
		AllowedUsers:       cfg.AllowedUsers(),
		DMs:                cfg.DMs,
		PromptTimeout:      cfg.PromptTimeout(),
		EditInterval:       cfg.EditInterval(),
		Budget:             cfg.Budget(),
		SealMarker:         cfg.SealMarker,
		ContinuationMarker: cfg.ContinuationMarker,
		SilentSentinel:     cfg.GetSilentSentinel(),
		HideThinking:       cfg.HideThinking,
		AckEmoji:           cfg.GetAckEmoji(),
		Logf:               log.Printf,
	})
	if err != nil {
		log.Fatalf("handler: %v", err)
	}
	// A restart killed the child agent, so any turn that was in flight
	// is dead. Say so rather than leaving a truncated answer that looks
	// complete.
	h.MarkInterrupted(ctx)

	runner, err := zulipproto.NewRunner(zulipproto.RunnerConfig{
		Client:     zc,
		EventTypes: eventTypes,
		Narrow:     narrow,
		OnRegister: onRegister,
		Handle: func(ctx context.Context, ev zulipproto.Event) {
			// The served set is updated from the same goroutine that
			// dispatches messages, so a message can never be judged
			// against a set older than the subscription event that
			// preceded it.
			served.Apply(ev)
			h.Handle(ctx, ev)
		},
		Logf: log.Printf,
	})
	if err != nil {
		log.Fatalf("events: %v", err)
	}

	log.Printf("zulip-acp: listening")
	_ = runner.Run(ctx)

	// Let in-flight turns finish posting before the agent is closed.
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = h.WaitIdle(shutdown)
	log.Printf("zulip-acp: stopped")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
