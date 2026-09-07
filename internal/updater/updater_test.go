package updater

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kfet/distkit"
	"github.com/kfet/distkit/installsh"
)

// repoRoot is where install.sh and its spec live, from this package.
const repoRoot = "../.."

// The four strings are the whole point of this package: a wrong repo or a
// wrong asset stem is a self-update that resolves nothing, and a wrong
// restart hint tells an operator to do the one thing (restart) that drops
// every message posted before the fresh event queue registers.
func TestConfigNamesThisRelay(t *testing.T) {
	cfg := Config("v1.2.3")
	if cfg.Repo != "kfet/zulip-acp" || cfg.Binary != "zulip-acp" || cfg.AssetStem != "zulip-acp" {
		t.Fatalf("config names the wrong artefact: %+v", cfg)
	}
	if cfg.Version != "v1.2.3" {
		t.Fatalf("version not carried: %q", cfg.Version)
	}
	if cfg.RestartHint != "systemctl --user reload zulip-acp" {
		t.Fatalf("restart hint is not the graceful reload: %q", cfg.RestartHint)
	}
	// goreleaser publishes raw binaries named zulip-acp-<os>-<arch>
	// (.goreleaser.yaml archives.name_template), with GOARCH=arm at
	// GOARM=6 published as armv6. A mismatch here is an update that
	// reports "release … has no asset".
	named := cfg
	named.AssetTemplate = distkit.DefaultAssetTemplate // applied by distkit's own normalise
	if got, want := named.AssetName("v1.2.3"), "zulip-acp-"+runtime.GOOS+"-"+assetArch(); got != want {
		t.Fatalf("asset name = %q, want %q", got, want)
	}
}

func assetArch() string {
	if runtime.GOARCH == "arm" {
		return "armv6"
	}
	return runtime.GOARCH
}

// The binary and the installer must agree about where releases come from
// and what the assets are called, or `install.sh` fetches one thing and
// `zulip-acp update` another.
func TestInstallShSpecAgreesWithTheBinary(t *testing.T) {
	spec, err := installsh.LoadSpec(filepath.Join(repoRoot, "install.sh.json"))
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	cfg := Config("v1.2.3")
	want := installsh.FromConfig(cfg)
	// An omitted spec field means "the distkit default", which is what
	// FromConfig leaves empty too when the Go config takes the default.
	if spec.Repo != want.Repo {
		t.Errorf("install.sh.json repo = %q, binary uses %q", spec.Repo, want.Repo)
	}
	if spec.Binary != want.Binary {
		t.Errorf("install.sh.json binary = %q, binary uses %q", spec.Binary, want.Binary)
	}
	if got := orDefault(spec.AssetStem, want.Binary); got != want.AssetStem {
		t.Errorf("install.sh.json asset_stem = %q, binary uses %q", got, want.AssetStem)
	}
	if got := orDefault(spec.AssetTemplate, distkit.DefaultAssetTemplate); got != orDefault(want.AssetTemplate, distkit.DefaultAssetTemplate) {
		t.Errorf("install.sh.json asset_template = %q, binary uses %q", got, want.AssetTemplate)
	}
	if got := orDefault(spec.ArmSuffix, "armv6"); got != orDefault(want.ArmSuffix, "armv6") {
		t.Errorf("install.sh.json arm_suffix = %q, binary uses %q", got, want.ArmSuffix)
	}
	if got := orDefault(spec.ChecksumsAsset, distkit.DefaultChecksums); got != orDefault(want.ChecksumsAsset, distkit.DefaultChecksums) {
		t.Errorf("install.sh.json checksums_asset = %q, binary uses %q", got, want.ChecksumsAsset)
	}
	if spec.NoChecksums || want.NoChecksums {
		t.Error("checksum verification must never be disabled: this runs on fleet hosts")
	}
}

// The checked-in install.sh is generated, and a stale copy is the one
// failure a user hits before they have a binary to complain with. `make
// check-installsh` guards it too; this guards it for anyone who only runs
// `go test`.
func TestInstallShIsNotDrifted(t *testing.T) {
	spec, err := installsh.LoadSpec(filepath.Join(repoRoot, "install.sh.json"))
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if err := installsh.CheckDrift(filepath.Join(repoRoot, "install.sh"), spec); err != nil {
		t.Fatalf("install.sh has drifted from the distkit template: %v\n  → run `make install.sh`", err)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// A dev build can never equal a release tag, so updating one would rename
// a release binary over a developer's own build on every run. distkit
// refuses; this asserts the refusal survives our wiring, and that the
// wiring routes distkit's output to the writers we hand it rather than to
// the process's own.
func TestMainRefusesADevBuild(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(nil, "dev", &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not a release build") {
		t.Fatalf("stderr = %q, want the dev-build refusal", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", out.String())
	}
}

// Bad flags are a usage error (2), not an update failure (1), and the
// usage text must reach the stderr we passed rather than the process's.
func TestMainRejectsBadFlags(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"-nope"}, "v0.1.0", &out, &errb); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "-nope") {
		t.Fatalf("stderr = %q, want the flag error", errb.String())
	}
}

// -h is a successful request for help, and the flag set must know the
// four flags the update skill and scripts/converge.sh actually pass.
func TestMainHelpListsTheFlags(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"-h"}, "v0.1.0", &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, flag := range []string{"-check", "-version", "-repo", "-restart-cmd"} {
		if !strings.Contains(errb.String(), flag) {
			t.Errorf("usage does not mention %s:\n%s", flag, errb.String())
		}
	}
	// The hint an operator is told to run after a swap must be the
	// graceful reload, not a restart.
	if !strings.Contains(errb.String(), RestartHint) {
		t.Errorf("usage does not offer the graceful reload:\n%s", errb.String())
	}
}
