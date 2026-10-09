package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A Homebrew-style symlink must survive: the update moves the real
// file to a new Cellar directory, so the resolved path goes stale.
func TestAgentBinForKeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	cellar := filepath.Join(dir, "Cellar", "1.0", "fir")
	os.MkdirAll(filepath.Dir(cellar), 0o755)
	os.WriteFile(cellar, []byte("#!/bin/sh\n"), 0o755)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	link := filepath.Join(bin, "fir")
	if err := os.Symlink(cellar, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if got := agentBinFor([]string{"fir"}); got != link {
		t.Fatalf("agentBinFor = %q, want %q", got, link)
	}
}
