package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainUpdatesAndReportsTheRecycle(t *testing.T) {
	noTokenEnv(t)
	f := &fakeRelease{tag: "v8.0.0", body: []byte("NEW")}
	path, execPath := installed(t, "OLD")

	var out, errOut strings.Builder
	code := Main(nil, "v1.0.0", Options{
		APIBase: f.start(t), ExecPath: execPath, Stdout: &out, Stderr: &errOut,
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if got, _ := os.ReadFile(path); string(got) != "NEW" {
		t.Fatalf("binary = %q", got)
	}
	// A swapped binary is inert until the relay re-execs, so the default
	// path must name the graceful verb.
	if !strings.Contains(out.String(), RestartHint) {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestMainRunsRestartCommand(t *testing.T) {
	shellEnv(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "recycled")

	f := &fakeRelease{tag: "v8.1.0"}
	_, execPath := installed(t, "OLD")
	var out, errOut strings.Builder
	code := Main([]string{"-restart-cmd", "touch " + marker}, "v1.0.0", Options{
		APIBase: f.start(t), ExecPath: execPath, Stdout: &out, Stderr: &errOut,
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("restart command did not run: %v", err)
	}
}

func TestMainRestartFailureIsAnError(t *testing.T) {
	shellEnv(t)
	f := &fakeRelease{tag: "v8.2.0"}
	_, execPath := installed(t, "OLD")
	var out, errOut strings.Builder
	code := Main([]string{"-restart-cmd", "exit 3"}, "v1.0.0", Options{
		APIBase: f.start(t), ExecPath: execPath, Stdout: &out, Stderr: &errOut,
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "recycle:") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestMainCheckAndFlags(t *testing.T) {
	noTokenEnv(t)
	f := &fakeRelease{tag: "v8.3.0"}
	base := f.start(t)
	path, execPath := installed(t, "OLD")

	var out, errOut strings.Builder
	code := Main([]string{"-check", "-repo", "kfet/zulip-acp", "-version", "8.3.0"}, "v1.0.0", Options{
		APIBase: base, ExecPath: execPath, Stdout: &out, Stderr: &errOut,
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if got, _ := os.ReadFile(path); string(got) != "OLD" {
		t.Fatal("-check must not install")
	}
	if !strings.Contains(out.String(), "update available") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestMainErrors(t *testing.T) {
	noTokenEnv(t)
	t.Run("bad flag", func(t *testing.T) {
		var errOut strings.Builder
		if code := Main([]string{"-nope"}, "v1", Options{Stderr: &errOut}); code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})
	t.Run("update fails", func(t *testing.T) {
		var out, errOut strings.Builder
		code := Main(nil, "v1", Options{
			ExecPath: func() (string, error) { return "/opt/homebrew/bin/zulip-acp", nil },
			Stdout:   &out, Stderr: &errOut,
		})
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(errOut.String(), "update:") {
			t.Fatalf("stderr = %q", errOut.String())
		}
	})
	t.Run("already current exits clean", func(t *testing.T) {
		f := &fakeRelease{tag: "v8.4.0"}
		_, execPath := installed(t, "OLD")
		var out, errOut strings.Builder
		code := Main(nil, "v8.4.0", Options{
			APIBase: f.start(t), ExecPath: execPath, Stdout: &out, Stderr: &errOut,
		})
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
	})
}

// The zero-Options production call must not panic for want of writers.
func TestMainDefaultsWriters(t *testing.T) {
	noTokenEnv(t)
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devnull.Close() })
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	t.Cleanup(func() { os.Stdout, os.Stderr = stdout, stderr })

	if code := Main([]string{"-nope"}, "v1", Options{}); code != 2 {
		t.Fatalf("exit = %d", code)
	}
}
