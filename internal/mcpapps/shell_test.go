package mcpapps

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realNode finds a Node.js binary. Some hosts put a shim (bun) first on
// PATH that does not behave like Node, so each candidate must report a
// Node version ("v18.19.1").
func realNode() string {
	for _, name := range []string{"node", "nodejs", "/usr/bin/node", "/usr/local/bin/node"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if v, _ := exec.Command(p, "--version").Output(); strings.HasPrefix(string(v), "v") {
			return p
		}
	}
	return ""
}

// TestShellJSBehaviour runs shell/shell_test.js: the host shell under
// fake browser APIs. No browser runs in the test suite, so this is the
// guard on the postMessage side of the bridge.
func TestShellJSBehaviour(t *testing.T) {
	node := realNode()
	if node == "" {
		t.Skip("Node.js is not installed")
	}
	if out, err := exec.Command(node, filepath.Join("shell", "shell_test.js")).CombinedOutput(); err != nil {
		t.Fatalf("shell_test.js: %v\n%s", err, out)
	}
}
