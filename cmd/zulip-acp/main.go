// zulip-acp bridges a self-hosted Zulip server to an ACP-speaking
// coding agent (fir --mode acp, Claude Code, …). Each Zulip topic,
// scoped by its channel, maps to one ACP session.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/acp-kit/relaytool"
	"github.com/kfet/acp-kit/schedule"
	"github.com/kfet/acp-kit/state"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/handler"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/reload"
	"github.com/kfet/zulip-acp/internal/zulipmcp"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

var version = "dev"

func main() {
	// The redirector subcommand must be intercepted before anything
	// else: the agent spawns THIS binary as the MCP stdio server, and
	// that process must not parse flags, read config or start a relay.
	if handled, err := mcphost.MaybeRunRedir(zulipmcp.RedirConfig()); handled {
		if err != nil {
			log.Fatalf("mcp-serve: %v", err)
		}
		return
	}

	configPath := flag.String("config", "", "path to JSON config file")
	agentCmd := flag.String("agent-cmd", "", "agent argv (default: fir --mode acp); space-separated; overrides config")
	stateDir := flag.String("state-dir", "", "root directory for per-conversation state")
	channelsFlag := flag.String("channels", "", "comma-separated Zulip channel names or ids to serve; overrides config")
	showVersion := flag.Bool("version", false, "print version and exit")
	printPaths := flag.Bool("print-paths", false, "print resolved config, state dir and agent command, then exit")
	reloadDrain := flag.Duration("reload-drain-deadline", reload.DefaultReloadDrain,
		"how long a SIGHUP graceful reload waits for in-flight turns to finish before re-execing anyway. "+
			"Nothing external is waiting on this — the Zulip event queue buffers server-side meanwhile — and agent turns "+
			"legitimately run tens of minutes, so this is a leak backstop, not a working bound (prompt_timeout bounds a turn as work)")
	stopDrain := flag.Duration("drain-deadline", reload.DefaultStopDrain,
		"how long a SIGINT/SIGTERM shutdown waits for in-flight turns to finish posting. Something external IS waiting "+
			"(systemd SIGKILLs the cgroup at TimeoutStopSec), so keep it comfortably underneath that")
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
		fmt.Printf("relay-mcp:  %t\n", cfg.RelayMCP)
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

	// SIGHUP is the graceful reload: stop all INTAKE — the event poll and
	// the schedule timers — let the in-flight turns finish, then exec the
	// on-disk binary in place. Nothing accumulates locally meanwhile: the
	// Zulip event queue buffers server-side, and an overdue schedule is
	// claimed by the successor image on its next tick.
	//
	// Only intake stops. Turns already running are on contexts detached
	// from it (handler.handleMessage uses context.WithoutCancel), which
	// is what lets an agent reload the relay hosting it from inside its
	// own turn and still finish its reply.
	//
	// Closing the channel is idempotent via sync.Once, so a burst of
	// reloads is one reload.
	hupSig := make(chan os.Signal, 1)
	signal.Notify(hupSig, syscall.SIGHUP)
	defer signal.Stop(hupSig)
	handoff := make(chan struct{})
	intakeCtx, stopIntake := context.WithCancel(ctx)
	defer stopIntake()
	var hupOnce sync.Once
	go func() {
		for range hupSig {
			hupOnce.Do(func() {
				log.Printf("zulip-acp: SIGHUP — graceful reload: no longer polling, draining in-flight turns (up to %s), then re-exec", *reloadDrain)
				close(handoff)
				stopIntake()
				// Ignore every SIGHUP from here on, and do it via
				// signal.Ignore so the SIG_IGN disposition is INHERITED
				// ACROSS THE EXEC. The drain can run for minutes and it
				// is invisible from outside, so an impatient operator
				// reloading a second time is expected — and a SIGHUP
				// that arrives while execve(2) is running would
				// otherwise be delivered to the new image before it has
				// installed a handler, whose default action is to kill
				// it. The new image's own signal.Notify resets the
				// disposition, so at worst a redundant reload is
				// dropped.
				signal.Ignore(syscall.SIGHUP)
			})
		}
	}()

	// A graceful reload exec's this binary in place and hands the live
	// Zulip event queue forward in the environment. Read it before
	// anything else touches the network: registering a fresh queue when
	// one was inherited would skip every message posted during the
	// reload window.
	cursor, err := reload.Inherited()
	if err != nil {
		log.Printf("zulip-acp: WARN %v — registering a fresh event queue; messages posted during the reload are lost", err)
	}

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
	ambient, err := cfg.ResolveAmbient(streams)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	follow := cfg.FollowsSubscriptions()
	served := channels.New(channels.Config{Explicit: explicit, Ambient: ambient, Follow: follow, Logf: log.Printf})
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
	if len(ambient) > 0 {
		albls := make([]string, 0, len(ambient))
		for id, name := range ambient {
			albls = append(albls, fmt.Sprintf("#%s (%d)", name, id))
		}
		sort.Strings(albls)
		log.Printf("zulip-acp: ambient channels (engage with no @-mention, like a DM): %s", strings.Join(albls, ", "))
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

	// Teardown is explicit rather than purely deferred: a graceful
	// reload ends in syscall.Exec, which replaces the process image and
	// runs no deferred functions. Everything that owns a child process
	// or a socket must be closed BEFORE that, or the exec'd image
	// inherits an orphaned ACP agent. sync.Once keeps the deferred call
	// harmless once the reload path has already run it.
	var closeOnce sync.Once
	var closers []func()
	cleanup := func() {
		closeOnce.Do(func() {
			// Reverse order: the agent must go before the MCP host it
			// dials, and after the session manager that drives it.
			for i := len(closers) - 1; i >= 0; i-- {
				closers[i]()
			}
		})
	}
	defer cleanup()

	// The self-hosted MCP server must exist before the agent starts:
	// its per-session config is minted at session/new, so the client
	// needs the hook wired at construction.
	var mcpHost *mcphost.Host
	if cfg.RelayMCP {
		mcpHost, err = mcphost.New(zulipmcp.HostConfig())
		if err != nil {
			log.Fatalf("relay-mcp: %v", err)
		}
		closers = append(closers, func() { _ = mcpHost.Close() })
	}

	clientCfg := cfg.AgentClientConfig(os.Stderr)
	if mcpHost != nil {
		clientCfg.MCPServersForSession = func(cwd string) []acp.McpServer {
			// A fresh token per session, bound server-side to the
			// conv-id. The conversation is never sent by the client,
			// so a tool call cannot claim to be from another topic.
			return mcpHost.ServerConfigForSession(filepath.Base(cwd))
		}
	}
	agent, err := client.Start(ctx, clientCfg)
	if err != nil {
		log.Fatalf("agent start: %v", err)
	}
	closers = append(closers, func() { agent.Close() })
	log.Printf("zulip-acp: agent up (caps=%+v)", agent.Caps())

	sessions, err := state.New(state.Config{
		Agent:                agent,
		StateDir:             cfg.StateDir,
		IdleTimeout:          cfg.IdleTimeout(),
		SystemPromptProvider: systemPromptProvider(*configPath, cfg),
	})
	if err != nil {
		log.Fatalf("state: %v", err)
	}
	closers = append(closers, func() { _ = sessions.Close() })
	go sessions.Run(ctx)

	// The chat-command broker is shared with poe-acp (acp-kit/command).
	// handler.New wires the Handler in as its Controller, so nothing
	// here calls SetController.
	broker := command.New(agent)

	// The loopback: the schedule store fires back into the Handler and
	// the tools resolve conversations through it, while the Handler
	// holds both. The cycle is broken by capturing h, which is assigned
	// below before any event, tool call or tick can reach it.
	var h *handler.Handler
	var schedules *schedule.Store
	var tools *relaytool.Tools
	if cfg.RelayMCP {
		schedules, err = schedule.Open(schedule.Config{
			Path:        filepath.Join(cfg.StateDir, "schedules.json"),
			Fire:        func(ctx context.Context, it schedule.Item) error { return h.FireSchedule(ctx, it) },
			MaxDepth:    cfg.MaxScheduleDepth,
			MaxPerConv:  cfg.MaxSchedulesPerConv,
			MaxTotal:    cfg.MaxSchedulesTotal,
			MinInterval: cfg.MinScheduleInterval(),
			Logf:        log.Printf,
		})
		if err != nil {
			log.Fatalf("schedules: %v", err)
		}
		tools, err = relaytool.New(relaytool.Config{
			Broker:    broker,
			ConvToken: func(k string) (string, bool) { return h.ConvToken(k) },
			Logf:      log.Printf,
		})
		if err != nil {
			log.Fatalf("relay-mcp: %v", err)
		}
	}

	h, err = handler.New(handler.Config{
		Client:             zc,
		Agent:              agent,
		Commands:           broker,
		Schedules:          schedules,
		Loopback:           tools,
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
		RepostOnClose:      cfg.GetRepostOnClose(),
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

	// Tools are registered only once the Handler exists: handler.New is
	// what wires it in as the broker's Controller, and relaytool asks
	// the broker which optional capabilities that Controller has.
	if cfg.RelayMCP {
		tools.Register(mcpHost)
		// The Zulip-specific half: `history` needs a journal.Key, not
		// a broker token, so it resolves identity through ConvKey —
		// the same server-side binding, one layer earlier.
		zulipTools, err := zulipmcp.NewTools(zulipmcp.Config{
			Client:  zc,
			ConvKey: func(k string) (journal.Key, bool) { return h.ConvKey(k) },
			Timeout: cfg.PromptTimeout(),
			Logf:    log.Printf,
		})
		if err != nil {
			log.Fatalf("relay-mcp history: %v", err)
		}
		zulipTools.Register(mcpHost)
		if err := mcpHost.Listen(); err != nil {
			log.Fatalf("relay-mcp listener: %v", err)
		}
		go schedules.Run(intakeCtx)
		log.Printf("zulip-acp: relay MCP loopback on %s — the agent can post, schedule and read history in its own conversation", mcpHost.SocketPath())
	}

	runner, err := zulipproto.NewRunner(zulipproto.RunnerConfig{
		Client:            zc,
		EventTypes:        eventTypes,
		Narrow:            narrow,
		OnRegister:        onRegister,
		Handoff:           handoff,
		ResumeQueueID:     cursor.QueueID,
		ResumeLastEventID: cursor.LastEventID,
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
	runErr := runner.Run(ctx)
	reloading := errors.Is(runErr, zulipproto.ErrHandoff)

	// Let in-flight turns finish posting before the agent is closed. On
	// a reload this is the step that makes an agent's own reply survive
	// its own `systemctl --user reload`: the signal has already been
	// delivered and the call has already returned, and we are waiting
	// here for exactly that turn.
	reloading = reload.Finish(ctx, reload.FinishConfig{
		Reloading:      reloading,
		Idle:           h,
		ReloadDeadline: *reloadDrain,
		StopDeadline:   *stopDrain,
		DiscardQueue:   runner.Discard,
		Logf:           log.Printf,
	})

	if !reloading {
		log.Printf("zulip-acp: stopped")
		return
	}

	// Everything holding a child process or socket must go before the
	// exec: it runs no deferred functions.
	cleanup()
	qid, last := runner.Cursor()
	c := reload.Cursor{QueueID: qid, LastEventID: last}
	if !c.Valid() {
		// The queue had already died (expired, or a silence
		// re-registration was in flight) when the reload landed. The
		// successor registers fresh, which skips everything posted
		// since — the one case where a reload is as lossy as a restart.
		log.Printf("zulip-acp: WARN no live event queue to hand on; the new image will register a fresh one and miss anything posted in the meantime")
	}
	log.Printf("zulip-acp: re-exec with %s", c)
	// Only returns on failure. The queue is still alive server-side, so
	// a failed exec strands it — say so, and exit non-zero so
	// Restart=on-failure brings the relay back.
	log.Fatalf("zulip-acp: %v (the event queue is orphaned; a fresh registration will miss messages posted since %d)", reload.Exec(c), last)
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
