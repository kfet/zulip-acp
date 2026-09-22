package handler

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentVersionFromCmd(t *testing.T) {
	if got := AgentVersionFromCmd(nil)(); got != "" {
		t.Fatalf("empty argv = %q", got)
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := filepath.Join(dir, "agent")
	body := "#!/bin/sh\necho x >> " + count + "\n[ \"$1\" = --version ] && printf ' fir 1.2.3 \\nbuilt today\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	v := AgentVersionFromCmd([]string{script, "--mode", "acp"})
	if got := v(); got != "fir 1.2.3" {
		t.Fatalf("version = %q", got)
	}
	if got := v(); got != "fir 1.2.3" {
		t.Fatalf("cached version = %q", got)
	}
	if b, _ := os.ReadFile(count); string(b) != "x\n" {
		t.Fatalf("ran %q times, want once", b)
	}
	if got := AgentVersionFromCmd([]string{filepath.Join(dir, "missing")})(); got != "" {
		t.Fatalf("missing binary = %q", got)
	}
}
