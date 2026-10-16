// Package discover finds the zulip-acp installs on THIS host — running
// relays, the service manager units that start them, and binaries on
// PATH — and drives `zulip-acp status` and the plan/confirm/swap/reload
// flow of `zulip-acp update`.
//
// It never reaches another host: a fleet host is converged from
// dist.lock by scripts/converge.sh, not from here. The swap itself is
// distkit's checksum-verified, ETXTBSY-safe atomic rename; this package
// only decides WHICH binaries to swap and HOW to reload the relays that
// run them.
//
// Every operating-system fact goes through Sys, so the parsing for both
// Linux (/proc, systemd) and macOS (ps, launchd) is tested on any host.
package discover

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

// Sys is the operating-system surface discovery and reload need.
type Sys interface {
	GOOS() string
	Getpid() int
	Home() string
	Getenv(key string) string
	Executable() (string, error)
	ReadFile(path string) ([]byte, error)
	Readlink(path string) (string, error)
	Glob(pattern string) ([]string, error)
	EvalSymlinks(path string) (string, error)
	// IsExec reports whether path is a regular file with an exec bit.
	IsExec(path string) bool
	// Run runs a command and returns its stdout.
	Run(name string, args ...string) (string, error)
	Signal(pid int, sig syscall.Signal) error
}

// OS returns the Sys of the running host.
func OS() Sys { return osSys{} }

type osSys struct{}

func (osSys) GOOS() string                          { return runtime.GOOS }
func (osSys) Getpid() int                           { return os.Getpid() }
func (osSys) Getenv(k string) string                { return os.Getenv(k) }
func (osSys) Executable() (string, error)           { return os.Executable() }
func (osSys) ReadFile(p string) ([]byte, error)     { return os.ReadFile(p) }
func (osSys) Readlink(p string) (string, error)     { return os.Readlink(p) }
func (osSys) Glob(p string) ([]string, error)       { return filepath.Glob(p) }
func (osSys) EvalSymlinks(p string) (string, error) { return filepath.EvalSymlinks(p) }
func (osSys) Signal(pid int, s syscall.Signal) error {
	return syscall.Kill(pid, s)
}

func (osSys) Home() string {
	h, _ := os.UserHomeDir()
	return h
}

func (osSys) IsExec(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

func (osSys) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
