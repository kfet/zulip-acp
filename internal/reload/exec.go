//go:build unix

package reload

// Process-image replacement: the last step of a graceful reload, and the
// only part of this package that a non-unix build could not compile.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// execFn is the image-replacement seam (default syscall.Exec). Tests
// stub it: a real Exec would replace the test binary.
var execFn = syscall.Exec

// SelfPath resolves the on-disk path of the binary to exec.
//
// os.Executable is preferred, but on Linux it reads /proc/self/exe,
// which names the running INODE — and the standard update flow replaces
// the binary with an atomic rename, which unlinks that inode. The link
// then reads "/path/to/zulip-acp (deleted)", and exec'ing that path
// fails. So a "(deleted)" marker is stripped, and the result is
// stat'ed; if it is still not there, os.Args[0] is resolved through
// PATH as a last resort.
//
// One case it deliberately does not chase: a binary that was MOVED
// ASIDE rather than replaced. /proc/self/exe then names the new
// location, Stat succeeds, and the reload re-execs the OLD image. The
// documented update flow renames the NEW binary onto the path
// (internal/skills/bundle/update), which is the "(deleted)" case above;
// second-guessing /proc/self/exe further would mean preferring argv[0],
// and argv[0] is attacker-adjacent input in a way /proc/self/exe is not.
//
// Getting this wrong is not a theoretical risk: it is the difference
// between a reload that picks up the new binary and one that fails
// outright after the agent has already been shut down.
func SelfPath() (string, error) {
	if p, err := osExecutable(); err == nil {
		p = strings.TrimSuffix(p, " (deleted)")
		if _, serr := os.Stat(p); serr == nil {
			return p, nil
		}
	}
	argv0 := os.Args[0]
	if strings.ContainsRune(argv0, filepath.Separator) {
		if _, err := os.Stat(argv0); err == nil {
			return filepath.Abs(argv0)
		}
	}
	p, err := lookPath(argv0)
	if err != nil {
		return "", fmt.Errorf("reload: cannot locate own binary: %w", err)
	}
	return p, nil
}

// osExecutable and lookPath are seams for SelfPath's fallback ladder.
var (
	osExecutable = os.Executable
	lookPath     = exec.LookPath
)

// Exec replaces this process image with the on-disk binary, preserving
// argv and passing c — plus the loopback MCP token registry blob, when
// the relay has one — forward in the environment. The PID is unchanged,
// so the init system never observes the service stop.
//
// mcpTokens comes from mcphost.Host.ExportTokens and is empty when the
// loopback is disabled. It is a bearer credential for every live
// session: it is placed in the environment of the process we are
// becoming and NOWHERE else — never logged, never written to disk.
//
// On success it does not return. Every caller MUST have drained its
// turns and shut its ACP agent child down first: exec runs no deferred
// functions and leaves no opportunity for cleanup. The MCP host must
// have been closed with CloseForExec, not Close, so the socket file the
// agent's redirector is reconnecting to is still there for the
// successor to bind.
func Exec(c Cursor, mcpTokens string) error {
	path, err := SelfPath()
	if err != nil {
		return err
	}
	argv := append([]string{path}, os.Args[1:]...)
	if err := execFn(path, argv, Environ(os.Environ(), c, mcpTokens)); err != nil {
		return fmt.Errorf("reload: exec %s: %w", path, err)
	}
	return nil // unreachable on success: execve replaced the image
}
