.DEFAULT_GOAL := all

BINDIR      := bin
BINARY      := $(BINDIR)/zulip-acp
NOTICE_FILE := THIRD_PARTY_NOTICES.md
GOBIN       := $(shell go env GOPATH)/bin
VERSION     := $(shell cat VERSION 2>/dev/null || echo dev)

# Append -dev+<sha>[.dirty] unless HEAD is the exact release tag.
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null)
GIT_TAG    := $(shell git describe --exact-match --tags HEAD 2>/dev/null)
GIT_DIRTY  := $(shell git diff --quiet 2>/dev/null || echo .dirty)
ifneq ($(GIT_TAG),v$(VERSION))
  ifneq ($(GIT_COMMIT),)
    VERSION := $(VERSION)-dev+$(GIT_COMMIT)$(GIT_DIRTY)
  endif
endif

LDFLAGS := -s -w -X main.version=$(VERSION)

GO_LICENSES := go run github.com/google/go-licenses@v1.6.0
COVGATE     := go tool covgate

# Cross-compile matrix. Element format: <goos>-<goarch>[-<goarm>].
CROSS      := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 linux-armv6
CROSS_BINS := $(addprefix $(BINARY)-,$(CROSS))

# ---------------------------------------------------------------------------
# Quiet step helper: $(call RUN,label,command). V=1 for verbose output.
# ---------------------------------------------------------------------------
ifdef V
  define RUN
	@printf "  %-28s\n" "$(1)"
	$(2)
  endef
else
  define RUN
	@_log=$$(mktemp) && ( $(2) ) > $$_log 2>&1 \
		&& { printf "  %-28s ✓\n" "$(1)"; rm -f $$_log; } \
		|| { printf "  %-28s ✗\n" "$(1)"; cat $$_log; rm -f $$_log; exit 1; }
  endef
endif

.PHONY: all _parallel build build-all install fmt tidy vet \
        test test-race-cover test-scripts test-cover open-coverage \
        clean notices check-licenses check-installsh publish FORCE

# Used as a prereq to force pattern-rule recipes to run every invocation
# (.PHONY would short-circuit pattern-rule matching for the target itself).
FORCE:

# ---------------------------------------------------------------------------
# Top-level: bare `make` runs the full CI pipeline.
# fmt + tidy run serially (they mutate files); the rest run in parallel
# via a recursive sub-make so internal -j works without the caller
# passing -j on the command line.
# ---------------------------------------------------------------------------
all: fmt tidy
	@$(MAKE) -j --no-print-directory _parallel

_parallel: vet test-race-cover test-scripts build build-all check-licenses check-installsh

fmt:
	@gofmt -s -w .

tidy:
	@go mod tidy

vet:
	$(call RUN,vet,go vet ./...)

$(BINDIR):
	@mkdir -p $@

# ---------------------------------------------------------------------------
# Builds
# ---------------------------------------------------------------------------

build: | $(BINDIR)
	$(call RUN,build (native),go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/zulip-acp/)

build-all: $(CROSS_BINS)

# Pattern rule for cross-compiled binaries: bin/zulip-acp-<os>-<arch>.
# armv6 is special-cased to GOARCH=arm GOARM=6.
define cross_build
	$(call RUN,build $(1)/$(2),GOOS=$(1) GOARCH=$(if $(filter armv6,$(2)),arm,$(2)) $(if $(filter armv6,$(2)),GOARM=6) go build -trimpath -ldflags="$(LDFLAGS)" -o $@ ./cmd/zulip-acp/)
endef

$(BINARY)-%: FORCE | $(BINDIR)
	$(call cross_build,$(word 1,$(subst -, ,$*)),$(word 2,$(subst -, ,$*)))

install:
	go install -ldflags="$(LDFLAGS)" ./cmd/zulip-acp/

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

# Plain quick run.
test:
	go test ./...

# Full run: race + shuffle + 100% coverage gate (paths in .covignore excluded).
# This is what `make all` exercises.
test-race-cover: | $(BINDIR)
	$(call RUN,test (race+cover),\
		go test -race -shuffle=on -cover ./... -coverprofile=$(BINDIR)/coverage.tmp.out \
		&& $(COVGATE) -profile=$(BINDIR)/coverage.tmp.out -out=$(BINDIR)/coverage.out -ignore=.covignore -min=100)

# Shell tooling under scripts/ (converge.sh) — offline black-box assertions:
# golden renders, the recycle-mechanism matrix, and a fake host with stubbed
# systemd and /proc. No network, no ssh, no real service.
test-scripts:
	$(call RUN,test (scripts),./test/converge_render.sh)

# Human-friendly per-function coverage summary (no gate).
test-cover: | $(BINDIR)
	go test -coverprofile=$(BINDIR)/coverage.out ./...
	go tool cover -func=$(BINDIR)/coverage.out

open-coverage:
	go tool cover -html=$(BINDIR)/coverage.out

clean:
	rm -rf $(BINDIR) dist
	rm -f $(NOTICE_FILE)

# ---------------------------------------------------------------------------
# Third-party license notices
# ---------------------------------------------------------------------------

notices: $(NOTICE_FILE)

$(NOTICE_FILE): go.mod go.sum
	$(call RUN,generate notices,$(GO_LICENSES) report ./cmd/zulip-acp > $(NOTICE_FILE) 2>/dev/null)

check-licenses:
	$(call RUN,check licenses,$(GO_LICENSES) check ./cmd/zulip-acp --disallowed_types=forbidden,restricted 2>/dev/null)

# ---------------------------------------------------------------------------
# install.sh
#
# The root install.sh is GENERATED from distkit's canonical template plus
# install.sh.json — shell cannot be imported, so the honest form of "shared"
# is generated-and-verified. The check target is dev-only: it fails the build
# when the checked-in copy has drifted from the template, so a stale
# installer is caught here rather than by a user piping it into sh.
# The template itself lives in distkit, so the pinned version is a real
# prerequisite: without go.mod here a distkit bump leaves make reporting
# "up to date" on an install.sh that check-installsh then rejects.
install.sh: install.sh.json go.mod
	$(call RUN,generate install.sh,go run github.com/kfet/distkit/cmd/distkit-installsh -o $@)

check-installsh:
	$(call RUN,check install.sh,go run github.com/kfet/distkit/cmd/distkit-installsh -check)

# ---------------------------------------------------------------------------
# Release
# ---------------------------------------------------------------------------

RELEASE_TAG := v$(shell cat VERSION 2>/dev/null || echo 0.0.0)

publish: build notices
	@if ! git diff --quiet -- $(NOTICE_FILE); then \
		git add $(NOTICE_FILE) && git commit -m "chore: refresh THIRD_PARTY_NOTICES.md for $(RELEASE_TAG)"; \
	fi
	@echo "Preflight $(RELEASE_TAG)..."
	@BRANCH=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$BRANCH" != "main" ]; then \
		echo "ABORT: on branch '$$BRANCH'; releases publish from 'main'."; \
		exit 1; \
	fi; \
	git fetch origin >/dev/null 2>&1 || { echo "ABORT: 'git fetch origin' failed."; exit 1; }; \
	LOCAL=$$(git rev-parse main); \
	REMOTE=$$(git rev-parse origin/main); \
	BASE=$$(git merge-base main origin/main); \
	if [ "$$LOCAL" != "$$REMOTE" ] && [ "$$REMOTE" != "$$BASE" ]; then \
		echo "ABORT: origin/main has moved ahead of local main."; \
		echo "  local  main: $$LOCAL"; \
		echo "  origin/main: $$REMOTE"; \
		echo "A concurrent release landed while this one was in flight."; \
		echo "Do NOT rebase this release commit: $(RELEASE_TAG) was cut against a"; \
		echo "stale VERSION and rebasing it would publish the wrong version number."; \
		echo "Re-cut the release instead:"; \
		echo "  git tag -d $(RELEASE_TAG) && git reset --hard origin/main"; \
		echo "  then bump VERSION above the new tip and re-run the release."; \
		exit 1; \
	fi; \
	if [ -n "$$(git ls-remote --tags origin refs/tags/$(RELEASE_TAG) 2>/dev/null)" ]; then \
		echo "ABORT: tag $(RELEASE_TAG) already exists on origin."; \
		echo "Bump VERSION and re-cut the release."; \
		exit 1; \
	fi; \
	HIGHEST=$$(git ls-remote --tags origin 2>/dev/null \
		| awk '{print $$2}' \
		| sed -e 's|^refs/tags/||' -e 's|\^{}$$||' \
		| grep '^v[0-9]' | sort -V -u | tail -n 1); \
	if [ -n "$$HIGHEST" ] && [ "$$HIGHEST" != "$(RELEASE_TAG)" ] && \
	   [ "$$(printf '%s\n%s\n' "$(RELEASE_TAG)" "$$HIGHEST" | sort -V | tail -n 1)" = "$$HIGHEST" ]; then \
		echo "ABORT: origin already has a higher version tag ($$HIGHEST > $(RELEASE_TAG))."; \
		echo "Bump VERSION above $$HIGHEST and re-cut the release."; \
		exit 1; \
	fi; \
	echo "  origin/main in sync, $(RELEASE_TAG) is the newest tag - OK"
	@echo "Publishing $(RELEASE_TAG)..."
	git push --atomic origin main $(RELEASE_TAG)
	@echo "Pushed $(RELEASE_TAG)."

# There is deliberately NO `deploy` target. Hand-placing a binary over ssh
# was how a host ended up running something nobody could name: no checksum,
# no atomic swap, no record. The verbs are `zulip-acp update` (checksum-
# verified, ETXTBSY-safe, in place) for a host that already runs the relay,
# `scripts/converge.sh <bot> --apply` for a host with a spec in bots/, and
# install.sh for a first install.
