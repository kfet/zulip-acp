package config

import (
	"strings"
	"testing"
)

func TestValidateScope(t *testing.T) {
	for _, c := range []Config{{}, {SessionScope: "thread", CwdMode: "local"}, {SessionScope: "thread_user", CwdMode: "agent"}} {
		if err := c.Validate(); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	if err := (&Config{SessionScope: "user"}).Validate(); err == nil || !strings.Contains(err.Error(), "session_scope") {
		t.Fatalf("err = %v", err)
	}
	if err := (&Config{CwdMode: "remote"}).Validate(); err == nil || !strings.Contains(err.Error(), "cwd_mode") {
		t.Fatalf("err = %v", err)
	}
}

func TestScopeAccessors(t *testing.T) {
	if (&Config{}).PerUserSessions() || (&Config{}).AgentCwd() {
		t.Fatal("defaults are not today's behaviour")
	}
	c := &Config{SessionScope: SessionScopeThreadUser, CwdMode: CwdModeAgent}
	if !c.PerUserSessions() || !c.AgentCwd() {
		t.Fatal("accessors ignore the config")
	}
}
