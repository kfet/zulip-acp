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
	"strings"
	"syscall"
	"time"

	"github.com/kfet/acp-kit/client"
	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/acp-kit/state"

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
	channels := flag.String("channels", "", "comma-separated Zulip channel names or ids to serve; overrides config")
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
	if *channels != "" {
		cfg.Channels = splitList(*channels)
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

	streams, err := zc.Streams(ctx)
	if err != nil {
		log.Fatalf("zulip: list channels: %v", err)
	}
	served, err := cfg.ResolveChannels(streams)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	names := make([]string, 0, len(served))
	narrow := make([][2]string, 0, len(served))
	for id, name := range served {
		names = append(names, fmt.Sprintf("#%s (%d)", name, id))
		narrow = append(narrow, zulipproto.NarrowChannel(name))
	}
	log.Printf("zulip-acp: serving %s", strings.Join(names, ", "))

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

	h, err := handler.New(handler.Config{
		Client:             zc,
		Agent:              agent,
		Sessions:           sessions,
		Journal:            jr,
		BotUserID:          me.UserID,
		BotFullName:        me.FullName,
		Channels:           served,
		AllowedUsers:       cfg.AllowedUsers(),
		PromptTimeout:      cfg.PromptTimeout(),
		EditInterval:       cfg.EditInterval(),
		Budget:             cfg.Budget(),
		SealMarker:         cfg.SealMarker,
		ContinuationMarker: cfg.ContinuationMarker,
		SilentSentinel:     cfg.GetSilentSentinel(),
		HideThinking:       cfg.HideThinking,
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
		EventTypes: []string{zulipproto.EventMessage, zulipproto.EventUpdateMessage},
		Narrow:     narrow,
		Handle:     h.Handle,
		Logf:       log.Printf,
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
