package config

import (
	"os"
	"slices"
	"testing"

	"github.com/kfet/zulip-acp/internal/reload"
)

// TestAgentClientConfigScrubsTheReloadCursor: a re-exec'd relay carries
// the live event-queue cursor in its own environment, and the ACP agent
// must never see it. A queue id plus any credential is enough to poll
// the relay's own queue and take delivery of its messages, and the
// agent is driven by text from people who are not the operator.
func TestAgentClientConfigScrubsTheReloadCursor(t *testing.T) {
	c := &Config{BotAPIKey: "k"}
	got := c.AgentClientConfig(os.Stderr).SecretEnvNames
	for _, name := range append([]string{"ZULIP_API_KEY", "ZULIP_EMAIL"}, reload.AgentEnvNames()...) {
		if !slices.Contains(got, name) {
			t.Errorf("SecretEnvNames %q is missing %q", got, name)
		}
	}
}
