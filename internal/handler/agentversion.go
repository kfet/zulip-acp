package handler

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// agentVersionTimeout bounds the `<agent> --version` fallback. A
// `!status` reply must not hang on an agent that ignores the flag.
const agentVersionTimeout = 5 * time.Second

// AgentVersionFromCmd returns a Config.AgentVersion that runs
// `argv[0] --version` once, on first use, and caches the first line of
// its output. It is the fallback for an agent that sends no agentInfo
// at initialize. On any failure it returns "", and `!status` then
// shows only the agent command. It runs without the agent's env or
// cwd, which `--version` does not need.
func AgentVersionFromCmd(argv []string) func() string {
	if len(argv) == 0 {
		return func() string { return "" }
	}
	return sync.OnceValue(func() string {
		ctx, cancel := context.WithTimeout(context.Background(), agentVersionTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, argv[0], "--version").Output()
		if err != nil {
			return ""
		}
		v, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return strings.TrimSpace(v)
	})
}
