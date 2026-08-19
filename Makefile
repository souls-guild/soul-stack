# Makefile for Soul Stack. POSIX-compatible targets for macOS-dev and Linux-CI.
#
# protoc plugins are installed via `go install` and land in $(go env GOPATH)/bin,
# which may not be in PATH. We propagate it explicitly so `protoc --go_out`
# finds `protoc-gen-go` and `protoc-gen-go-grpc`.

SHELL := /bin/sh

GOPATH_BIN := $(shell go env GOPATH)/bin
export PATH := $(GOPATH_BIN):$(PATH)

# Keeper protocols and plugin protocols.
# Keeper lives in the shared proto/ module (ADR-011) -> one protoc invocation with
# proto_path=proto and output=proto/gen/go/. Plugin is a separate nested
# go.mod submodule -> a second protoc invocation, its own proto_path/output.
KEEPER_PROTO_ROOT := proto
KEEPER_PROTO_OUT  := proto/gen/go
KEEPER_PROTO_FILES := $(shell find $(KEEPER_PROTO_ROOT)/keeper -name '*.proto')

PLUGIN_PROTO_ROOT := proto/plugin
PLUGIN_PROTO_OUT  := proto/plugin/gen/go
PLUGIN_PROTO_FILES := $(shell find $(PLUGIN_PROTO_ROOT)/v1 -name '*.proto')

# govulncheck - supply-chain CI gate (org security-audit recommendation before beta).
# The binary is installed via `go install` into $(GOPATH)/bin (same pattern as protoc plugins).
# Version is pinned - a floating version would surface as gate instability. Full path to
# the binary (macOS-make bypasses exported PATH for plain recipe lines).
GOVULNCHECK_VERSION := v1.3.0
GOVULNCHECK := $(GOPATH_BIN)/govulncheck

MODULES := proto proto/plugin shared sdk keeper soul soul-lint soulctl

# `<dir>:<tag>` pairs whose sources only build under their own tag, vetted by
# `vet-tags` on top of the `integration` pass over $(MODULES). The tests/ modules
# are outside $(MODULES) (their own go.mod, no non-test packages); `keeper:smoke`
# is one legacy ad-hoc file that would otherwise never be compiled by anything.
TAGGED_DIRS := tests/e2e:e2e tests/e2e-live:e2e_live tests/e2e-k8s:e2e_k8s keeper:smoke

# Directory for built binaries relative to the root of each module with `main`.
# Covered by `.gitignore` (`*/bin/`).
BIN_DIR := bin

# Path to the built offline linter (used by the `lint` target).
LINT_BIN := soul-lint/$(BIN_DIR)/soul-lint

# Path to the built L0 runner (used by the `trial` target). The binary is built
# as part of `build`, same as soul-lint.
TRIAL_BIN := keeper/$(BIN_DIR)/soul-trial

# Services EXCLUDED from the gate run of `trial` (see target). This is NOT "green
# by default" - the list is explicit and loud: every skip prints its reason so the
# exclusion is visible in the CI log and doesn't mask a new regression. Currently empty:
# the sole former entry (redis-monitored) was removed along with the service during
# the redis consolidation.
TRIAL_SKIP :=

# Build version. Defaults to git-derived: nearest tag + short hash,
# `-dirty` suffix for uncommitted changes (`git describe`). On a bare
# checkout without tags it falls back to a short hash (`--always`). Overridden externally:
# `make build VERSION=v1.2.3` (this is what the release pipeline does).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)

# ldflags version injection. `-X <pkg>.<var>=<value>` overwrites a package-level
# string variable at link time - without touching the source. IMPORTANT: inside
# the binary being built, the entrypoint package is addressed as `main`, NOT as its
# import path - so the variable path is `main.<var>`, not the full
# `github.com/.../cmd/<x>.<var>` (the linker silently ignores the latter - the string
# lands in the data section but isn't bound to the symbol). Each binary is
# built with a separate `go build ./cmd/<x>`, and each has exactly one `main` - `-X
# main.<var>` is unambiguous. One git tag = the version of all modules (ADR-011), so
# $(VERSION) is shared. The version variable's name differs per binary (historically):
# `soul` -> soulVersion, `keeper` -> version, `soulctl` -> soulctlVersion.
# `soul-lint` has no version variable (offline linter, never prints a version).
SOUL_LDFLAGS := -X main.soulVersion=$(VERSION)
KEEPER_LDFLAGS := -X main.version=$(VERSION)
SOULCTL_LDFLAGS := -X main.soulctlVersion=$(VERSION)
LEGION_LDFLAGS := -X main.legionVersion=$(VERSION)

# --- Release/packaging ---
# Root of build artifacts (SBOM, native packages). Entirely in .gitignore (dist/) -
# build artifacts are not committed.
DIST_DIR := dist
SBOM_DIR := $(DIST_DIR)/sbom
PKG_DIR  := $(DIST_DIR)/pkg

# Prod-image names (docker-keeper / docker-soul targets). Local tags default to
# `soul-stack/keeper` and `soul-stack/soul`; the operator retags them for their
# own registry before push (`docker tag soul-stack/keeper:$(VERSION)
# <registry>/keeper:$(VERSION)`) or builds directly against it:
# `make docker-keeper KEEPER_IMAGE=<registry>/keeper`. The version tag is the shared
# $(VERSION) (git describe / release-override), which also lands in the binary's
# ldflags and the image's OCI label. soul-lint has no prod image (offline linter, not a prod runtime).
KEEPER_IMAGE ?= soul-stack/keeper
SOUL_IMAGE   ?= soul-stack/soul

.PHONY: gen build build-keeper build-soul build-soulctl build-linux bin-keeper bin-soul bin-soul-lint test test-plugins test-race test-integration e2e e2e-live e2e-live-gate e2e-k8s e2e-cloud check-e2e-cloud check-all check-ci check-integration-set check-e2e-set check-gate check-ci-status docker-build-keeper docker-build-soul docker-keeper docker-soul tidy check check-fmt vet vet-tags check-gen check-doc-links check-approle-template check-vuln lint trial dev-up dev-down dev-stop dev-reset dev-provision dev-smoke dev-keeper dev-jwt dev-souls dev-web dev-stand dev-stand-free gen-audit-catalog gen-openapi check-openapi check-template check-stand-template check-soul-template check-dev-stand-build sync-webui check-webui check-webui-embed check-webui-provenance sbom pkg sign stress load-test help dev-souls-docker dev-souls-docker-down

gen: gen-openapi
	@mkdir -p $(KEEPER_PROTO_OUT) $(PLUGIN_PROTO_OUT)
	@if [ -z "$(KEEPER_PROTO_FILES)" ]; then \
		echo "no .proto files under $(KEEPER_PROTO_ROOT)/keeper"; \
	else \
		echo "protoc keeper: $(KEEPER_PROTO_FILES)"; \
		protoc \
			--proto_path=$(KEEPER_PROTO_ROOT) \
			--go_out=$(KEEPER_PROTO_OUT) \
			--go_opt=paths=source_relative \
			--go-grpc_out=$(KEEPER_PROTO_OUT) \
			--go-grpc_opt=paths=source_relative \
			$(KEEPER_PROTO_FILES); \
	fi
	@if [ -z "$(PLUGIN_PROTO_FILES)" ]; then \
		echo "no .proto files under $(PLUGIN_PROTO_ROOT)/v1"; \
	else \
		echo "protoc plugin: $(PLUGIN_PROTO_FILES)"; \
		protoc \
			--proto_path=$(PLUGIN_PROTO_ROOT) \
			--go_out=$(PLUGIN_PROTO_OUT) \
			--go_opt=paths=source_relative \
			--go-grpc_out=$(PLUGIN_PROTO_OUT) \
			--go-grpc_opt=paths=source_relative \
			$(PLUGIN_PROTO_FILES); \
	fi

# gen-audit-catalog - commits shared/audit/event_types_gen.go as a DERIVED file: the list of
# every EventType constant, which Go cannot enumerate at runtime. The huma layer turns it into
# the OpenAPI enum of AuditEvent.type (NIM-346), so gen-openapi depends on it - the dump embeds
# whatever the catalog held at compile time, and regenerating the spec from a stale catalog would
# publish a set that is missing the event just added.
# Same mechanism as gen-openapi: a generate-test writes under GEN_AUDIT_CATALOG=1 and compares
# without it, so the drift guard runs inside the ordinary `make test`.
gen-audit-catalog:
	@echo "audit event-type declarations -> $(AUDIT_CATALOG_COMMITTED)"
	@GEN_AUDIT_CATALOG=1 go test ./shared/audit/ -run TestGeneratedEventTypes_NoDrift -count=1 >/dev/null

# gen-openapi - commits docs/keeper/openapi.yaml as a DERIVED huma-generated file
# (OpenAPI 3.1, for UI-vendor + git-review). Source of truth is the huma aggregator in
# the code (HumaFullSpecYAML); there's no hand-written openapi.yaml anymore. The write is done by
# a generate-test in the api package under GEN_OPENAPI=1 (no separate cmd-binary).
# `-count=1` disables the cache (the test writes a file - nothing to cache).
gen-openapi: gen-audit-catalog
	@echo "huma-dump -> $(OPENAPI_COMMITTED)"
	@GEN_OPENAPI=1 go test ./keeper/internal/api/ -run TestCommittedOpenAPI_NoDrift -count=1 >/dev/null

# Builds the three binaries (`keeper`, `soul`, `soul-lint`) with an explicit `-o <module>/bin/<name>`,
# so artifacts don't land in the module root and get picked up by git.
# Library modules (`proto`, `proto/plugin`, `shared`, `sdk`) are built without
# `-o` via `go build ./...` to verify compilation; they don't produce
# executables. Modules without go packages (e.g. `proto/plugin/` before
# its first .proto appears) are skipped via `go list ./...`, otherwise
# `go build ./...` fails with "matched no packages".
build:
	@for m in proto proto/plugin shared sdk; do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go build ./... in $$m"; \
		(cd $$m && go build ./...) || exit 1; \
	done
	@$(MAKE) build-keeper
	@echo "go build -o keeper/$(BIN_DIR)/soul-trial ./cmd/soul-trial in keeper"
	@cd keeper && go build -o $(BIN_DIR)/soul-trial ./cmd/soul-trial
	@$(MAKE) build-soul
	@echo "go build -o soul-lint/$(BIN_DIR)/soul-lint ./cmd/soul-lint in soul-lint"
	@cd soul-lint && go build -o $(BIN_DIR)/soul-lint ./cmd/soul-lint
	@$(MAKE) build-soulctl
	@$(MAKE) build-soul-legion

# Single-binary targets, split out of `build` so the dev stand can rebuild ONE
# binary without paying for the whole set. `dev/keeper-run.sh` and
# `dev/souls-up.sh` go through these rather than calling `go build` themselves:
# the version stamp then has exactly one definition ($(VERSION)), and a running
# stand can always be asked which commit it serves (`curl /healthz` -> version,
# ADR-0076(h)).
#
# Before NIM-342 both scripts built with NO -ldflags and only when the binary was
# MISSING. A stand therefore served an arbitrarily old build that reported
# `0.0.0-dev`, and nothing on any surface could contradict it - an observation made
# against such a stand ("a narrowly scoped role sees everything") cost a critical
# priority and a dedicated session before the binary turned out not to be the
# merged code at all.
build-keeper:
	@echo "go build -o keeper/$(BIN_DIR)/keeper ./cmd/keeper in keeper (VERSION=$(VERSION))"
	@cd keeper && go build -ldflags '$(KEEPER_LDFLAGS)' -o $(BIN_DIR)/keeper ./cmd/keeper

build-soul:
	@echo "go build -o soul/$(BIN_DIR)/soul ./cmd/soul in soul (VERSION=$(VERSION))"
	@cd soul && go build -ldflags '$(SOUL_LDFLAGS)' -o $(BIN_DIR)/soul ./cmd/soul

# soul-legion is a shipped artifact (ADR-004 Amendment 2026-07-26), so `build`
# has to produce it like the rest. Its code lives in the tests/load module, which
# is outside MODULES - a separate target keeps that seam explicit (NIM-327 tracks
# bringing the module under the check gate).
build-soul-legion:
	@echo "go build -o tests/load/$(BIN_DIR)/soul-legion ./cmd/soul-legion in tests/load (VERSION=$(VERSION))"
	@cd tests/load && go build -ldflags '$(LEGION_LDFLAGS)' -o $(BIN_DIR)/soul-legion ./cmd/soul-legion

# Builds the operator's client CLI (see docs/naming-rules.md -> soulctl).
# Cobra scaffold with no command bodies implemented yet - a separate target so it
# can be built independently of keeper/soul (different lifecycle, no depend-infra).
build-soulctl:
	@echo "go build -o soulctl/$(BIN_DIR)/soulctl ./cmd/soulctl in soulctl (VERSION=$(VERSION))"
	@cd soulctl && go build -ldflags '$(SOULCTL_LDFLAGS)' -o $(BIN_DIR)/soulctl ./cmd/soulctl

# Modules without go packages are skipped via `go list ./...` - same rule
# as in `build`. At this stage `proto/plugin/` falls under the filter.
#
# `-count=1` disables the go-test cache. CRITICAL for the gate, not an optimization: go caches
# a package's result by the hash of its `.go` sources (+ declared inputs), but NOT by
# the content of arbitrary files read at runtime via `os.ReadFile` by
# path (e.g. keeper/internal/render renders examples/destiny/*/templates/*.tmpl).
# Without `-count=1`, editing such a .tmpl (without touching the .go test) leaves the result
# `(cached) ok` - a broken test passes the gate silently (this is how the broken redis-render
# slipped through in f40da00: a conf_dir/data_dir wave changed the .tmpl without touching the .go test).
# The same trick is already in place in test-plugins / test-integration / gen-openapi.
test:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go test -count=1 ./... in $$m"; \
		(cd $$m && go test -count=1 ./...) || exit 1; \
	done

# Tests for community plugins examples/module/* - each is a SEPARATE go.mod OUTSIDE go.work
# (ADR-016: community plugins pull the core as a regular dependency, not a workspace member).
# So they run with `GOWORK=off` per-module, and NOT via the MODULES list in `make test`
# (which doesn't see them at all). This also covers the security guard on secret
# masking in the community.redis plugin (59 test functions), which would otherwise
# stay outside the gate.
#
# Skip-on-unresolvable: cloud/ssh plugins (soul-cloud-*/soul-ssh-*) don't resolve
# standalone-offline (workspace go.mod pins diverge from standalone-tidy, needs network).
# `go list ./...` under GOWORK=off fails for them -> we skip LOUDLY with a warning (the same
# trick as `go list` empty -> skip in `test`/`vet`). This is NOT a silent pass: the skip
# is printed, and a plugin that *does* resolve offline (community.redis) isn't covered by it -
# its regressions are caught by the gate. Merge() tests are NOT here: they live in shared/cel
# (workspace, covered by `make test`), no need to duplicate.
# `-count=1` - no cache (the plugin may depend on external fake state).
test-plugins:
	@for d in examples/module/*/go.mod; do \
		[ -e "$$d" ] || continue; \
		m=$$(dirname "$$d"); \
		if ! (cd "$$m" && GOWORK=off go list ./... >/dev/null 2>&1); then \
			echo "skip $$m (standalone-offline doesn't resolve - GOWORK=off go list failed; cloud/ssh plugin or go.mod drift)"; \
			continue; \
		fi; \
		echo "GOWORK=off go test -count=1 ./... in $$m"; \
		(cd "$$m" && GOWORK=off go test -count=1 ./...) || exit 1; \
	done
	@echo "test-plugins: community plugins (resolvable offline) green"

# Runs tests with the race detector - a separate target so the plain `make test`
# stays fast. CI should run both: `test` (fast, on every push) and
# `test-race` (a separate step before merge).
#
# `-count=1` is load-bearing HERE for a reason that does not apply to the other
# targets (NIM-312). Everywhere else it defeats the file-content blind spot of the
# build cache (see `test`); for the race detector it defeats something worse. A
# race is found by OBSERVING an interleaving, so a green sweep means "no race was
# observed this time", not "there is no race" — the claim is only worth anything
# per run. Cached, `go test` replays that one historical observation forever: two
# consecutive sweeps of this target finished in 8 s reporting `(cached) ok` for all
# 140 packages, which is a gate that cannot fail no matter what the code does.
# That is the NIM-238 shape once more — a check that did not happen, printing the
# words of one that passed.
test-race:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go test -race -count=1 ./... in $$m"; \
		(cd $$m && go test -race -count=1 ./...) || exit 1; \
	done

# Integration tests under the `integration` build tag (testcontainers-go).
# A separate target - `make test` doesn't need docker and stays fast.
# Files tagged `integration` are NOT built by a plain `go test ./...`, so
# we pass `-tags=integration` explicitly here. `-count=1` disables the Go test cache
# (the container spun up is new every time - nothing to cache).
#
# SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 used to be the point of this target: a
# TestMain whose container failed to come up logged "docker unavailable" and returned
# 0, so whole packages silently did not run while the target reported success (that is
# how 4 packages "passed" in the NIM-221 run). Since NIM-238 that is the DEFAULT
# everywhere — `-tags=integration` is taken as the statement that you want the suites —
# so this line no longer grants anything; it is kept because callers and CI logs still
# name it, and because a target that says what it requires is easier to read than one
# that relies on a default. The opt-OUT is now explicit and lives in
# keeper/internal/integrationenv: SOUL_STACK_INTEGRATION_SKIP_DOCKER=1.
#
# INTEGRATION_PARALLEL caps how many packages - i.e. how many container sets - start
# at once. Unbounded, the default GOMAXPROCS-wide start swamps the docker daemon and
# packages fail on the testcontainers reaper rather than on their own assertions.
INTEGRATION_PARALLEL ?= 4
SOUL_STACK_INTEGRATION_REQUIRE_DOCKER ?= 1

# PKG narrows the run to one package while keeping every other flag identical:
#
#     make test-integration PKG=./internal/scenario/
#
# It exists because the alternative people reached for -- a bare
# `go test -tags=integration ./internal/scenario/` -- silently drops `-race`, and
# a green L1 then means something WEAKER locally than the same words mean in CI.
# That is the failure mode NIM-238 is about, one level up: the check that did not
# happen is indistinguishable from the check that passed. There is now one way to
# run L1, and it matches CI by construction (TestIntegrationSuiteRunsUnderRace in
# keeper/internal/integrationenv fails a full sweep that lost the flag anyway).
PKG ?= ./...

# The package set is the packages that actually carry integration-tagged tests,
# derived from the tree by scripts/integration-packages.sh (43 today: 41 in
# keeper, 2 in soul) rather than taken as `./...` (~155).
#
# The difference is not tidiness (NIM-349). `-tags=integration` WIDENS a package
# set instead of narrowing it, so `./...` handed the whole unit corpus to a
# command carrying `-race` — inside the one job that is also starting container
# sets at `-p $(INTEGRATION_PARALLEL)`. Unit tests written against uninstrumented
# timing then fail on instrumentation speed: `TestRun_CancelDuringTask` polled
# Cancel every 5 ms and lost ~1% of the time under `-race` while staying green in
# `make test`, on the same sha, differing only in this flag. A job that fails on a
# coin toss is untrustworthy in both directions — red stops meaning regression,
# and once people learn to rerun it, red stops meaning anything.
#
# `-race` is NOT removed anywhere; the SET is corrected. The excluded packages are
# exactly those with no tagged file, they keep running in `test` and now under the
# detector in `test-race`, and `vet-tags` still compiles the whole tree under the
# tag. So this narrows what L1 RUNS without narrowing what is CHECKED — which is
# the only reason it is allowed, given NIM-238.
# On failure the output goes through scripts/classify-l1-failure.py, which says of
# each failing package whether it died at the container layer or on an assertion.
# `CLUSTERDOWN`, `connection refused` against a mapped port and "wait until ready:
# context deadline exceeded" arrive formatted exactly like a caught regression, and
# the two have opposite answers — rerun that package, versus fix the code. Read by
# eye, every red run costs an hour deciding which happened, and the cheap way out of
# that hour ("L1 is flaky, rerun it") is how a real regression gets dismissed.
# Nothing is downgraded: the classification is a label, the exit code is still 1,
# and a failure that survives a solitary rerun is a finding whatever its label says.
#
# The loop no longer stops at the first failing module. It used to `exit 1` there,
# which hid whether the other modules were also red — and the classifier is worth
# more with the whole picture than with the first fragment of it. bash + pipefail
# so `| tee` cannot swallow a non-zero status.
test-integration: SHELL := /bin/bash
test-integration: $(if $(filter ./...,$(PKG)),check-integration-set,)
	@scripts/integration-scope.sh "$(PKG)"
	@set -o pipefail; \
	log="$$(mktemp -t soul-stack-l1-XXXXXX.log)"; rc=0; \
	trap 'rm -f "$$log"' EXIT; \
	for m in $(MODULES); do \
		pkgs="$$(cd $$m && $(CURDIR)/scripts/integration-packages.sh $(PKG))"; \
		if [ -z "$$pkgs" ]; then \
			echo "skip $$m (no package under $(PKG) carries integration-tagged tests)"; \
			continue; \
		fi; \
		echo "go test -tags=integration -race -count=1 -p $(INTEGRATION_PARALLEL) in $$m ($$(echo $$pkgs | wc -w) tagged pkg; untagged ones run in make test / test-race)"; \
		if ! (cd $$m && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=$(SOUL_STACK_INTEGRATION_REQUIRE_DOCKER) \
			go test -tags=integration -race -count=1 -p $(INTEGRATION_PARALLEL) $$pkgs 2>&1 | tee -a "$$log"); then \
			rc=1; \
		fi; \
	done; \
	if [ "$$rc" -ne 0 ]; then $(CURDIR)/scripts/classify-l1-failure.py "$$log" || true; fi; \
	exit "$$rc"

# L3a fast-loop E2E (ADR-039): the working harness - testcontainers (PG+Redis+Vault) +
# a real Keeper process + a soul-stub with live gRPC-mTLS. A separate go module
# tests/e2e/ under the `e2e` build tag (testcontainers deps don't leak into the main
# keeper/soul). NOT part of `check` (requires docker); details in tests/e2e/README.md.
#
# Flags, each earned rather than copied (NIM-469):
#   -count=1  the tier is judged by whether it passes REPEATEDLY, and without
#             this a second `make e2e` prints the first one's verdict from the
#             test cache. A tier that cannot be re-run has no way to show that a
#             fix to an intermittent failure worked (same trap as NIM-388).
#   -timeout  was 10m. A full local run measures ~666s, so the budget sat BELOW
#             the runtime: the suite was killed mid-flight and every test still
#             queued never ran, which arrives as one panic plus a silently
#             truncated tier. 30m matches e2e-live and leaves the timeout doing
#             its actual job — catching a hang, not enforcing a schedule.
#   -p 1      one test package at a time. Each L3a test owns three containers
#             and a keeper process; overlapping packages double that and put two
#             independent binaries in the same ephemeral port range.
# bash + pipefail so `| tee` cannot swallow a non-zero status.
e2e: SHELL := /bin/bash
e2e:
	@if [ -z "$$(cd tests/e2e && go list -tags=e2e ./...)" ]; then \
		echo "tests/e2e: the e2e package set is EMPTY - this tier has no tests to run."; \
		echo "  An empty suite used to print a skip line and exit 0, which every gate above"; \
		echo "  read as a pass (NIM-392). Run 'make check-e2e-set' - it says whether the tag"; \
		echo "  vanished from the tree or the toolchain stopped reporting it."; \
		exit 1; \
	else \
		set -o pipefail; \
		log="$$(mktemp -t soul-stack-l3a-XXXXXX.log)"; rc=0; \
		trap 'rm -f "$$log"' EXIT; \
		echo "go test -tags=e2e -count=1 -p 1 ./... in tests/e2e"; \
		(cd tests/e2e && go test -tags=e2e -count=1 -p 1 -timeout=30m ./... 2>&1 | tee "$$log") || rc=1; \
		if [ "$$rc" -ne 0 ]; then $(CURDIR)/scripts/classify-l3a-failure.py "$$log" || true; fi; \
		exit "$$rc"; \
	fi

# Cross-compiles a single binary for Linux amd64 into its `bin/` with the
# `-linux-amd64` suffix. Shared recipe for the per-component bin-targets and the
# build-linux aggregate. $(1) - module/directory (keeper|soul|soul-lint), $(2) - the
# cmd-package name (= binary name: keeper|soul|soul-lint), $(3) - ldflags (empty for
# soul-lint - no version variable). CGO_ENABLED=0 - static, no dependency on libc inside
# the container (Debian-12 compatible, but static is simpler). Doesn't touch the
# native `make build` (macOS developers build host-arch).
define bin-one
	@echo "GOOS=linux GOARCH=amd64 go build -o $(1)/$(BIN_DIR)/$(2)-linux-amd64 ./cmd/$(2) in $(1) (VERSION=$(VERSION))"
	@cd $(1) && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '$(3)' -o $(BIN_DIR)/$(2)-linux-amd64 ./cmd/$(2)
endef

# Per-component cross-compile (linux-amd64). bin-keeper / bin-soul - artifacts for
# L3b real-soul-container and ad-hoc runs; build-linux - the aggregate of all three.
bin-keeper:
	$(call bin-one,keeper,keeper,$(KEEPER_LDFLAGS))

bin-soul:
	$(call bin-one,soul,soul,$(SOUL_LDFLAGS))

bin-soul-lint:
	$(call bin-one,soul-lint,soul-lint,)

# build-linux - the aggregate: keeper+soul for Linux amd64 (for L3b real-soul-container,
# ADR-039). Not changing the set (soul-lint isn't needed in L3b) - for solo builds there's
# bin-keeper / bin-soul / bin-soul-lint.
build-linux: bin-keeper bin-soul

# L3b smoke-loop E2E (ADR-039): a real-soul-binary in a privileged Debian-12
# container + a Keeper process on the host. Requires docker and `make build-linux`
# (cross-compile the linux-amd64 binary for mounting into the container). NOT part of `check`
# (long-running - 5-15 min per test; runs nightly/on-demand).
#
# `-p 1` - serial (RAM-heavy: privileged containers with systemd + apt-install
# running concurrently would kill a developer's laptop). Architect recommendation.
e2e-live: build-linux
	@if [ -z "$$(cd tests/e2e-live && go list -tags=e2e_live ./...)" ]; then \
		echo "tests/e2e-live: the e2e_live package set is EMPTY - this tier has no tests to run."; \
		echo "  An empty suite used to print a skip line and exit 0, which every gate above"; \
		echo "  read as a pass (NIM-392). Run 'make check-e2e-set' - it says whether the tag"; \
		echo "  vanished from the tree or the toolchain stopped reporting it."; \
		exit 1; \
	else \
		echo "go test -tags=e2e_live ./... in tests/e2e-live"; \
		(cd tests/e2e-live && go test -tags=e2e_live -count=1 -timeout=30m -p 1 ./...) || exit 1; \
	fi

# e2e-live-gate - the MANDATORY local live gate before a batch-commit of a large
# feature (~15-25 min, docker). L3b subset: SoulModule delivery mechanics
# (TestL3bModuleDeliveryLive - ADR-065 install-synthesis -> FetchModule -> Sigil-verify
# -> hot-register -> live apply against redis) + nginx apply smoke (TestL3bSmokeNginxLive)
# + plugin-channel smoke (TestL3bPluginChannel) + operational add_user against live redis
# (TestL3bRedisLive_Day2AddUser - the full ADR-065 plugin channel against real redis+sentinel)
# + operational update_config/restart/update_users/destroy/rotate_tls (CA rollover) on the same channel.
# The full `make e2e-live` remains nightly/pre-release.
#
# Deps DIFFER from e2e-live: also needs the native `build` - the harness runs
# Keeper ON THE HOST (host-arch keeper/bin/keeper, see locateKeeperBinary), not in a
# container. The community.redis plugin is built by the test itself (harness.BuildCommunityRedisPlugin),
# no need to build it in the Makefile.
#
# E2E_KEEPER_HOST - the IP the soul container uses to reach Keeper on the host;
# on WSL2 an explicit LAN IP is needed (localhost isn't visible from the container). If not set
# externally - auto-detects the first IP via `hostname -I`.
#
# NIM-45 anti-false-green (the gate must either actually run, or fail honestly):
#  - `build` - the native keeper at the harness's default path `keeper/bin/keeper`;
#    without it `build-linux` only gives keeper-linux-amd64, and NewStack SILENTLY skips.
#  - `-count=1` - otherwise the go-test cache returns `ok (cached)` in seconds (false-green).
#  - guard: fails on `(cached)` in the summary, or if any gate test didn't give `--- PASS`.
#    That grep carries a trailing ` (` on purpose: without it `--- PASS: TestFoo`
#    is also satisfied by `--- PASS: TestFooBar (12s)`, so a skipped test could be
#    signed off by a prefix-sharing neighbour (NIM-507).
# SHELL=bash - for `set -o pipefail` (preserve go test's exit code through tee).
#
# NIM-406: on red, the log goes through scripts/classify-e2e-live-failure.py,
# which says PER TEST whether the stand failed to come up or the test body did.
# Those two arrive identically formatted and have opposite answers, and this is
# a blocking pre-tag gate (RELEASING.md step e) - a blocking step whose red is
# illegible is one people learn to rerun until green. Nothing is downgraded: the
# recipe still exits non-zero either way.
#
# The transcript is a fresh file per invocation, and the recipe prints where it
# put it. The name used to be fixed - $TMPDIR/soul-e2e-live-gate.log - which is
# one path shared by every worktree on the machine, and this repo is worked in
# several at once. Two gates running together then read a file the other one is
# writing: `tee` truncates it at start, so the checks below can see a foreign
# run's `--- PASS` lines in place of a missing one of their own, and on red the
# classifier explains someone else's failure. Both are the NIM-406 defect -
# a verdict that is not about this run.
#
# E2E_GATE_TESTS is the single source for the -run mask, the per-test `--- PASS`
# guard and the classifier's NOT-RUN list. It used to be spelled out twice, and
# a test present in one copy but not the other is silently ungated.
#
# Entries are EXACT test names, and scripts/e2e-gate-mask.sh enforces that
# (NIM-507). Three of them used to be prefixes — `TestL3bPluginChannel` for
# `TestL3bPluginChannel_CatalogAndAllow`. `-run` takes an unanchored regexp so
# the gate still ran the right nine, but the other two readers take these as
# names: the `--- PASS` loop below could be satisfied by a prefix-sharing
# neighbour's line, and the classifier reported all three as NOT-RUN on every
# red run. Read the script's header before editing this list.
E2E_GATE_TESTS := TestL3bModuleDeliveryLive_SynthesisFetchHotRegister \
	TestL3bSmokeNginxLive_InstallAndStart TestL3bPluginChannel_CatalogAndAllow \
	TestL3bRedisLive_Day2AddUser TestL3bRedisLive_Day2UpdateConfig TestL3bRedisLive_Day2Restart \
	TestL3bRedisLive_Day2UpdateUsers TestL3bRedisLive_Day2Destroy TestL3bRedisLive_Day2RotateTls

e2e-live-gate: SHELL := /bin/bash
e2e-live-gate: build build-linux
	@echo "e2e-live-gate: harness unit-guards (docker-free) - apply bracket NIM-46, stand readiness NIM-406"
	@(cd tests/e2e-live && go test -count=1 ./harness/) \
		|| { echo "e2e-live-gate: FALSE-GREEN - a docker-free harness unit-guard failed" >&2; exit 1; }
	@scripts/e2e-gate-mask.sh verify $(E2E_GATE_TESTS) \
		|| { echo "e2e-live-gate: the gate list does not name real tests - fix it before spending 20 minutes on a run whose verdict would be about the wrong set" >&2; exit 1; }
	@if [ -z "$$(cd tests/e2e-live && go list -tags=e2e_live ./...)" ]; then \
		echo "tests/e2e-live: the e2e_live package set is EMPTY - this tier has no tests to run."; \
		echo "  An empty suite used to print a skip line and exit 0, which every gate above"; \
		echo "  read as a pass (NIM-392). Run 'make check-e2e-set' - it says whether the tag"; \
		echo "  vanished from the tree or the toolchain stopped reporting it."; \
		exit 1; \
	else \
		host="$${E2E_KEEPER_HOST:-$$(hostname -I | awk '{print $$1}')}"; \
		log=$$(mktemp "$${TMPDIR:-/tmp}/soul-e2e-live-gate.XXXXXXXX.log") || exit 1; \
		mask=$$(scripts/e2e-gate-mask.sh mask $(E2E_GATE_TESTS)) || exit 1; \
		echo "e2e-live-gate: transcript -> $$log"; \
		echo "e2e-live-gate: go test -tags=e2e_live -v -count=1 -run '$$mask' . (E2E_KEEPER_HOST=$$host)"; \
		set -o pipefail; \
		(cd tests/e2e-live && E2E_KEEPER_HOST=$$host go test -tags=e2e_live -v -count=1 -timeout 45m -p 1 -run "$$mask" .) 2>&1 | tee "$$log"; \
		rc=$$?; \
		if grep -qE '^ok[[:space:]].*\(cached\)' "$$log"; then \
			echo "e2e-live-gate: FALSE-GREEN - '(cached)' in summary (cache not disabled, -count=1 lost)" >&2; exit 1; \
		fi; \
		missing=""; \
		for tc in $(E2E_GATE_TESTS); do \
			grep -q "^--- PASS: $$tc (" "$$log" || missing="$$missing $$tc"; \
		done; \
		if [ $$rc -ne 0 ] || [ -n "$$missing" ]; then \
			scripts/classify-e2e-live-failure.py "$$log" $(E2E_GATE_TESTS) || true; \
			if [ $$rc -eq 0 ] && [ -n "$$missing" ]; then \
				echo "e2e-live-gate: FALSE-GREEN - go test exited 0, yet these gave no '--- PASS':$$missing" >&2; \
			fi; \
			exit 1; \
		fi; \
		echo "e2e-live-gate: OK - all gate tests actually ran (not cached, not skipped)"; \
	fi

# docker-build-keeper - builds the `keeper:e2e-k8s` image for the L3c kind cluster.
# Reuses the `make build-linux` artifact (cross-compiled keeper-linux-amd64);
# single-stage Dockerfile on top of the distroless runtime. PM decision: the image
# is disposable, loaded into kind via `kind load docker-image`, NOT
# published to a registry. Build context - repo root (the Dockerfile COPYs from
# `keeper/bin/keeper-linux-amd64`).
#
# Dependency: `make build-linux` builds the linux-amd64 binary.
docker-build-keeper: build-linux
	@echo "docker build -t keeper:e2e-k8s -f tests/e2e-k8s/dockerfiles/keeper.Dockerfile ."
	@docker build -t keeper:e2e-k8s -f tests/e2e-k8s/dockerfiles/keeper.Dockerfile .

# docker-keeper - the PROD image of keeper for publishing to the operator's registry. Unlike
# docker-build-keeper (a disposable kind image, single-stage from the
# build-linux artifact) - a multi-stage self-contained build from
# deploy/docker/keeper.Dockerfile: pins the golang toolchain, doesn't depend on the state of
# keeper/bin/, the version is injected into the binary (ldflags) and the OCI label
# (--build-arg VERSION). Tag - $(KEEPER_IMAGE):$(VERSION) (versioned, not latest:
# reproducible rollback). Build context - repo root.
#
# From there it's on the operator: `docker tag $(KEEPER_IMAGE):$(VERSION) <registry>/keeper:$(VERSION)`
# -> `docker push <registry>/keeper:$(VERSION)`. Bootstrapping the first Archon and
# prod config - deploy/README.md -> "Keeper in production".
#
# Requires docker in PATH (unlike build-linux/pkg). NOT part of `check`.
docker-keeper:
	@echo "docker build -t $(KEEPER_IMAGE):$(VERSION) --build-arg VERSION=$(VERSION) -f deploy/docker/keeper.Dockerfile ."
	@docker build -t $(KEEPER_IMAGE):$(VERSION) --build-arg VERSION='$(VERSION)' -f deploy/docker/keeper.Dockerfile .
	@echo "built $(KEEPER_IMAGE):$(VERSION) - retag it for your registry and push (see deploy/README.md)"

# docker-soul - the PROD image of soul (the daemon agent) for publishing to the operator's registry.
# Symmetric with docker-keeper: a multi-stage self-contained build from
# deploy/docker/soul.Dockerfile (distroless static-nonroot), the version is injected into the
# binary (ldflags main.soulVersion) and the OCI label (--build-arg VERSION). Tag -
# $(SOUL_IMAGE):$(VERSION). Build context - repo root.
#
# Unlike docker-build-soul (a disposable privileged systemd image for L3c
# kind, from the build-linux artifact) - this image is self-contained and intended for the
# registry. soul-lint has no prod image (offline linter).
#
# Requires docker in PATH (like docker-keeper). NOT part of `check`.
docker-soul:
	@echo "docker build -t $(SOUL_IMAGE):$(VERSION) --build-arg VERSION=$(VERSION) -f deploy/docker/soul.Dockerfile ."
	@docker build -t $(SOUL_IMAGE):$(VERSION) --build-arg VERSION='$(VERSION)' -f deploy/docker/soul.Dockerfile .
	@echo "built $(SOUL_IMAGE):$(VERSION) - retag it for your registry and push (see deploy/README.md)"

# docker-build-soul - builds the `soul:e2e-k8s` image for the L3c kind cluster
# (L3c-3+). Privileged systemd-PID-1 Debian-12 base (parity with L3b), bakes in the
# cross-compiled soul-linux-amd64 from the `make build-linux` artifact. Loaded
# into kind via `kind load docker-image soul:e2e-k8s` (harness DeploySoul).
#
# Build context - repo root (the Dockerfile COPYs from soul/bin/ and
# tests/e2e-k8s/manifests/soul/soul.service).
docker-build-soul: build-linux
	@echo "docker build -t soul:e2e-k8s -f tests/e2e-k8s/dockerfiles/soul.Dockerfile ."
	@docker build -t soul:e2e-k8s -f tests/e2e-k8s/dockerfiles/soul.Dockerfile .

# L3c k8s-loop E2E (ADR-039): kind cluster + bitnami Helm (PG/Redis/Vault) +
# raw YAML Keeper/Soul. Requires docker and kind CLI in PATH; without them the tests
# are skipped (see tests/e2e-k8s/harness/cluster.go::NewCluster pre-flight).
# NOT part of `check` (long-running: kind spin-up + helm-install + image-load,
# 5-15 min per test; runs weekly / pre-release).
#
# Dependency: `make docker-build-keeper` + `make docker-build-soul` build the
# keeper:e2e-k8s / soul:e2e-k8s images that the L3c-2+ tests load into kind
# via `kind load docker-image`.
#
# `-p 1` - serial (RAM-heavy: each test spins up its own kind cluster with
# its own PG/Redis/Vault via bitnami Helm; running in parallel would kill a laptop).
e2e-k8s: docker-build-keeper docker-build-soul
	@if [ -z "$$(cd tests/e2e-k8s && go list -tags=e2e_k8s ./...)" ]; then \
		echo "tests/e2e-k8s: the e2e_k8s package set is EMPTY - this tier has no tests to run."; \
		echo "  An empty suite used to print a skip line and exit 0, which every gate above"; \
		echo "  read as a pass (NIM-392). Run 'make check-e2e-set' - it says whether the tag"; \
		echo "  vanished from the tree or the toolchain stopped reporting it."; \
		exit 1; \
	else \
		echo "go test -tags=e2e_k8s ./... in tests/e2e-k8s"; \
		(cd tests/e2e-k8s && go test -tags=e2e_k8s -timeout=30m -p 1 ./...) || exit 1; \
	fi

# --- Cloud live-E2E orchestrator (NIM-31) ---
# e2e-cloud - runs the cloud live-E2E against the keeper's Operator API via teleport
# (EXEC_MODE=tsh) or direct curl (EXEC_MODE=local). NOT part of `check` (requires
# a cloud/teleport + pre-built artifacts; symmetric with e2e / e2e-live). Bring-up
# scripts (environment-specific) live locally in $$SCRIPTS_DIR and are NOT committed to git - the runner
# invokes them at runtime. Suite - the SUITE variable (create|create-destroy|day2). Examples:
#   make e2e-cloud SUITE=create-destroy
#   DRY_RUN=1 make e2e-cloud SUITE=day2 SCENARIO=add_user   # print calls without network
SUITE ?= create-destroy
e2e-cloud:
	@bash scripts/e2e-cloud/runbook.sh $(SUITE)

# check-e2e-cloud - a docker-free guard for the orchestrator's core logic: classify/poll/
# assert/run_scenario against a keeper_api stub on JSON fixtures (testdata/), RED/GREEN
# mutation pairs. Part of `check` alongside the other guard steps. Requires jq (like the repo's
# other dev scripts: dev/provision.sh etc.); a jq-less environment fails loudly with a hint.
check-e2e-cloud:
	@bash scripts/e2e-cloud/test/guard.sh

# `go mod tidy` on a module with no go files prints "no Go files" and fails,
# so modules with an empty `go list ./...` are skipped here too.
tidy:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go mod tidy in $$m"; \
		(cd $$m && go mod tidy) || exit 1; \
	done

# Local dev stack (docker-compose). See `docs/dev/local-setup.md`.
# `dev/docker-compose.yml` brings up the full required stack: Postgres, Redis,
# Vault (dev-mode), OTel collector and Jaeger.
#
# `dev-down` does NOT remove the volume - `postgres_data` persists across
# `up/down` cycles. For a full reset (migration changed, DB in an
# inconsistent state) - `make dev-reset`.
#
# Stand-aware (NIM-25): DEDICATED_INFRA=1 -> its own docker project (COMPOSE_PROJECT_NAME=
# ${STACK_PREFIX} = soul-stack-<slug>) + offset ports, down/reset hit ONLY that project.
# Lightweight mode (empty DEV_STAND or DEDICATED_INFRA=0) - shared infra, project as before.
dev-up:
	@bash -c '. dev/stand-env.sh && stand_summary'
	@bash -c 'set -e; . dev/stand-env.sh >/dev/null; if [ "$${DEDICATED_INFRA}" = "1" ]; then export COMPOSE_PROJECT_NAME="$${STACK_PREFIX}"; fi; cd dev && docker compose up -d'

# `dev-down` first kills the local keeper/soul dev-workflow daemons
# (see `dev-stop`), then the docker-compose infra. Otherwise an orphan `keeper run`
# from a previous session lingers and holds the ports (8080/8081/9090/9442/9443) -
# a fresh start fails with `bind: address already in use`.
dev-down: dev-stop
	@bash -c 'set -e; . dev/stand-env.sh >/dev/null; if [ "$${DEDICATED_INFRA}" = "1" ]; then export COMPOSE_PROJECT_NAME="$${STACK_PREFIX}"; fi; cd dev && docker compose down'

# Kills THIS stand's daemons (DEV_STAND): keeper/web by pidfile in ${STAND_DEV_DIR}
# (written by keeper-run/web-run), souls - by a stand-scoped pattern (--config under
# ${STAND_DEV_DIR}/). Empty DEV_STAND = default stand only; neighboring stands are NOT
# touched (previously: a broad pkill by name that killed every stand). NIM-25.
dev-stop:
	@bash -c 'set -e; . dev/stand-env.sh; stand_summary; d="$${STAND_DEV_DIR}"; kp="$$(cat "$$d/keeper.pid" 2>/dev/null || true)"; if [ -n "$$kp" ] && kill -0 "$$kp" 2>/dev/null && grep -qa keeper "/proc/$$kp/cmdline" 2>/dev/null; then kill -9 "$$kp" 2>/dev/null || true; fi; rm -f "$$d/keeper.pid"; wp="$$(cat "$$d/web.pid" 2>/dev/null || true)"; if [ -n "$$wp" ] && kill -0 "$$wp" 2>/dev/null && grep -qaE 'vite|node|npm' "/proc/$$wp/cmdline" 2>/dev/null; then pkill -9 -P "$$wp" 2>/dev/null || true; kill -9 "$$wp" 2>/dev/null || true; fi; rm -f "$$d/web.pid"; pkill -f "soul run.*$$d/" 2>/dev/null || true; echo "dev-stop: stand $${STAND_SLUG:-<default>} stopped (keeper/web by pidfile, souls by stand-pattern)"'

dev-reset:
	@bash -c '. dev/stand-env.sh && stand_summary'
	@bash -c 'set -e; . dev/stand-env.sh >/dev/null; if [ "$${DEDICATED_INFRA}" = "1" ]; then export COMPOSE_PROJECT_NAME="$${STACK_PREFIX}"; fi; cd dev && docker compose down -v && docker compose up -d'

# Idempotent bootstrap provisioning of secrets and TLS material for local-dev.
# The script is safe to re-run: each step checks its own state.
# Details - `docs/dev/local-setup.md`.
dev-provision:
	@bash -c '. dev/stand-env.sh && stand_summary'
	@bash dev/provision.sh

# Full smoke cycle: bring up the stack -> provision Vault/TLS -> `keeper init` ->
# seed the service registry. Builds the keeper binary before running (depends on the
# Go code, so we don't shortcut via `keeper/bin/keeper`-as-is). Stand-aware
# (DEV_STAND): init runs against the rendered ${STAND_DEV_DIR}/keeper.dev.yml, the operator's
# JWT file is ${STAND_DEV_DIR}/archon-alice.jwt (default /tmp/keeper-dev/...). The next
# smoke run will fail on `keeper init` (the operators registry is no longer empty) - to
# re-run, do `make dev-reset && make dev-smoke`.
#
# The second `dev-provision` - AFTER `keeper init`: on a fresh DB (dev-reset) the schema
# (service_registry / keeper_settings) doesn't exist yet, so the first provision pass
# skips seeding it (see dev/provision.sh::seed_service_registry, step 10). `keeper init`
# creates the schema (migrate.Apply), so the service registry is only seeded by the
# repeated provision pass. provision is idempotent - calling it twice is safe;
# without this step a single-pass `make dev-smoke` would leave an empty service registry
# (config-S4 removed services[] from keeper.dev.yml - resolution now reads only the DB).
dev-smoke:
	@$(MAKE) dev-up
	@$(MAKE) dev-provision
	@$(MAKE) build-keeper
	@VAULT_TOKEN=root bash -c '. dev/stand-env.sh >/dev/null && \
		mkdir -p "$${STAND_DEV_DIR}" && \
		envsubst "$${KEEPER_RENDER_WHITELIST}" < dev/keeper.dev.yml.tmpl > "$${STAND_DEV_DIR}/keeper.dev.yml" && \
		./keeper/bin/keeper init \
			--archon=archon-alice \
			--config="$${STAND_DEV_DIR}/keeper.dev.yml" \
			--credential-out="$${STAND_DEV_DIR}/archon-alice.jwt"'
	@$(MAKE) dev-provision

# Restarts keeper with the FULL dev-env (SOUL_STACK_ALLOW_FILE_REPOS=1 + writable
# cache-dirs): without it, file:// service resolution fails (502). Kills the old
# keeper, clears leader leases, waits for healthz 200. If there's no TLS material -
# hints at `make dev-provision`. Script - dev/keeper-run.sh.
dev-keeper:
	@bash -c '. dev/stand-env.sh && stand_summary'
	@bash dev/keeper-run.sh

# Issues an Archon JWT for ad-hoc dev API calls (without `keeper init`). The key
# comes from the same Vault KV as keeper's (NOT hardcoded). Prints ONLY the
# token to stdout -> `TOKEN=$$(make dev-jwt)`. Parameters - via variables:
# `make dev-jwt AID=archon-keyset ROLES='["keyset-demo"]' TTL=3600`.
AID ?= archon-alice
ROLES ?= ["cluster-admin"]
TTL ?= 43200
dev-jwt:
	@bash -c '. dev/stand-env.sh && stand_summary' >&2
	@AID='$(AID)' ROLES='$(ROLES)' TTL='$(TTL)' bash dev/mint-jwt.sh

# Brings the local souls back up from the DB registry: writes soul.yml for each sid
# (if missing), onboards it if there's no seed, (re)starts `soul run`. Covens
# in the DB are preserved (NOT re-registered). Script - dev/souls-up.sh.
dev-souls:
	@bash -c '. dev/stand-env.sh && stand_summary'
	@bash dev/souls-up.sh

# Brings up the local souls as docker containers (soul-docker-1..N) for operational
# scenarios and UI tests without a cloud (NIM-26). N - the SOULS_COUNT variable. WSL2:
# KEEPER_HOST=host-IP (see docs/dev/local-setup.md). Script - dev/souls-docker-up.sh.
SOULS_COUNT ?= 3
dev-souls-docker:
	@bash -c '. dev/stand-env.sh && stand_summary'
	@bash dev/souls-docker-up.sh $(SOULS_COUNT)

# Tears down the docker souls: soul-docker-* containers + registry entries + dev directories.
# Script - dev/souls-docker-down.sh.
dev-souls-docker-down:
	@bash -c '. dev/stand-env.sh && stand_summary'
	@bash dev/souls-docker-down.sh

# Vite dev server for the web repo (companion ../soul-stack-web). `--host` is required,
# otherwise vite only listens on [::1] and 127.0.0.1:5173 refuses connections. The web path -
# the WEB_DIR variable. Script - dev/web-run.sh.
WEB_DIR ?= ../soul-stack-web
dev-web:
	@bash -c '. dev/stand-env.sh && stand_summary'
	@WEB_DIR='$(WEB_DIR)' bash dev/web-run.sh

# Full dev-stand bring-up in one command: provision -> keeper -> souls -> web.
# Convenient after a restart / day change (/tmp gets cleared). At the end - a summary + a
# reminder about `make dev-jwt` for the token.
dev-stand:
	@$(MAKE) dev-provision
	@$(MAKE) dev-keeper
	@$(MAKE) dev-souls
	@$(MAKE) dev-web
	@bash -c 'set -e; . dev/stand-env.sh >/dev/null; \
		echo ""; \
		echo "=== dev-stand is up ($${STAND_SLUG:-<default>}) ==="; \
		echo "keeper:  healthz http://127.0.0.1:$${OPENAPI_PORT}/healthz | openapi :$${OPENAPI_PORT} | mcp :$${MCP_PORT} | metrics :$${METRICS_PORT}"; \
		echo "souls:   statuses - docker exec $${STACK_PREFIX}-postgres psql -U keeper -d $${PG_DB} -c '\''SELECT status, count(*) FROM souls GROUP BY status'\''"; \
		echo "web:     http://127.0.0.1:$${WEB_PORT}"; \
		echo "token:   TOKEN=\$$(make dev-jwt)   (parameters: AID=... ROLES='\''[...]'\'' TTL=...)"'

# Frees a stand's slot: removes the slug row from the slot registry (idempotent -
# no row = no-op). The slot's ports become available to the next stand again. NIM-25.
dev-stand-free:
	@test -n "$(DEV_STAND)" || { echo "dev-stand-free: specify DEV_STAND=<slug>"; exit 1; }
	@DEV_STAND='' bash -c '. dev/stand-env.sh >/dev/null && _stand_free_slot "$(DEV_STAND)"'
	@echo "dev-stand-free: slug slot '$(DEV_STAND)' freed (registry updated)"

# OpenAPI committed snapshot: source of truth is the huma aggregator in the code
# (HumaFullSpecYAML, served on GET /openapi.yaml). docs/keeper/openapi.yaml is a
# DERIVED dump (for UI-vendor + git-review), not a hand-written file. Two targets:
#
#   gen-openapi   - overwrites the committed file with the current huma dump (after editing
#                   the huma domain). Defined above, next to gen.
#   check-openapi - drift guard (CI): committed file == huma dump byte-for-byte;
#                   a failure means "forgot make gen-openapi". Delegates to the same
#                   generate-test (without GEN_OPENAPI it compares instead of writing).
OPENAPI_COMMITTED := docs/keeper/openapi.yaml

# The audit event-type catalog: same derived-file model one module down. It has no separate
# check- target because its guard is an ordinary package test (TestGeneratedEventTypes_NoDrift),
# which `make test` already runs - a second entry point would just be another way to run it.
AUDIT_CATALOG_COMMITTED := shared/audit/event_types_gen.go

check-openapi:
	@echo "openapi drift-guard: $(OPENAPI_COMMITTED) == huma-dump"
	@go test ./keeper/internal/api/ -run TestCommittedOpenAPI_NoDrift -count=1 >/dev/null || { \
		echo ""; \
		echo "openapi.yaml drift: committed $(OPENAPI_COMMITTED) diverges from the huma dump"; \
		echo "run 'make gen-openapi' to regenerate the committed snapshot"; \
		exit 1; \
	}
	@echo "openapi.yaml: committed snapshot matches the huma dump"

# Plugin-template self-serve: the plugin-author template tree's source of truth lives
# in the companion repo ../soul-stack-plugins/soul-mod-template/, and core embeds a copy
# via soul-lint/internal/plugininit/template/ (go:embed). Drift between the trees
# is caught the same way as openapi:
#
#   sync-template.sh - updates the copy from the companion (rsync --delete mirrors the whole tree).
#   check-template   - CI guard for divergence; a failure means "forgot to sync after
#                      editing the template in the companion".
#
# Companion is a SEPARATE repository: it may not exist on someone else's machine/CI. If SRC
# is missing - the gate doesn't fail (otherwise it would break `make check` without the companion),
# it's skipped with a warning. Drift is only caught when the companion is available alongside.
TEMPLATE_SRC := ../soul-stack-plugins/soul-mod-template
TEMPLATE_DST := soul-lint/internal/plugininit/template

check-template:
	@if [ ! -d "$(TEMPLATE_SRC)" ]; then \
		echo "companion soul-stack-plugins not found, skipping template-drift check"; \
	elif ! diff -r -q $(TEMPLATE_SRC) $(TEMPLATE_DST) >/dev/null; then \
		echo "plugin-template drift detected:"; \
		diff -r $(TEMPLATE_SRC) $(TEMPLATE_DST) || true; \
		echo ""; \
		echo "run 'scripts/sync-template.sh' to update the embedded copy"; \
		exit 1; \
	else \
		echo "plugin-template: copy in sync"; \
	fi

# Embed-UI vendoring: the built UI snapshot's source of truth lives in the
# companion repo ../soul-stack-web/dist/, and core embeds a copy via
# keeper/internal/webui/assets/ (go:embed, served by keeper at /ui, ADR-055).
# Drift between them is caught the same way as plugin-template:
#
#   sync-webui.sh - updates the copy from the companion (rsync --delete mirrors dist/,
#                   builds the companion via `npm run build` if dist/ is missing).
#   check-webui   - CI guard for divergence; a failure means "forgot to sync after
#                   rebuilding the UI in the companion".
#
# Companion is a SEPARATE repository: it may not exist on someone else's machine/CI. If SRC
# is missing - the gate doesn't fail (otherwise it would break `make check` without the companion),
# it's skipped with a warning. Drift is only caught when the companion is available alongside.
WEBUI_SRC := ../soul-stack-web/dist
WEBUI_DST := keeper/internal/webui/assets

sync-webui:
	@bash scripts/sync-webui.sh

# WEBUI_REPO is the companion checkout; WEBUI_SRC is its build output. They are
# separate because the two absences mean different things (NIM-277).
WEBUI_REPO := ../soul-stack-web

# THREE outcomes, not two. The old form collapsed "no companion here" and
# "companion here but not built" into one `skipping`, and the second is the
# release-worktree case — the one place the gate was supposed to work. So the
# rule that catches an unpaired web merge was itself skipped, silently, in
# exactly the situation it exists for, and that is how NIM-273 reached the
# release. Same shape as NIM-238: an unperformed check must never read like a
# passed one.
check-webui:
	@if [ ! -d "$(WEBUI_REPO)" ]; then \
		echo "check-webui: companion $(WEBUI_REPO) not present - skipping (expected in CI and third-party clones)"; \
	elif [ ! -d "$(WEBUI_SRC)" ]; then \
		echo "check-webui: companion IS present but $(WEBUI_SRC) is not built."; \
		echo "  This is the release-worktree case, and it is the one the drift guard exists for:"; \
		echo "  an unpaired web merge is invisible until the bundle is compared against a real build."; \
		echo "  Build it (cd $(WEBUI_REPO) && npm run build), or state that you are skipping:"; \
		echo "      make check WEBUI_SKIP=1"; \
		test -n "$(WEBUI_SKIP)" || exit 1; \
		echo "check-webui: skipped by WEBUI_SKIP - declared, not accidental"; \
	elif ! diff -r -q $(WEBUI_SRC) $(WEBUI_DST) >/dev/null; then \
		echo "embed-UI drift detected:"; \
		diff -r $(WEBUI_SRC) $(WEBUI_DST) || true; \
		echo ""; \
		echo "run 'make sync-webui' (or 'scripts/sync-webui.sh') to update the embedded copy"; \
		exit 1; \
	else \
		echo "embed-UI: copy in sync"; \
		$(MAKE) --no-print-directory check-webui-provenance; \
	fi

# Even a byte-identical bundle can be stale: `diff` only says the copy matches
# the build sitting in dist/, and dist/ is whatever was built last -- possibly
# from an older commit than the branch the release is assembling. The recorded
# SHA answers the question diff cannot: WHICH companion commit these bytes came
# from. Advisory, not fatal: the companion may legitimately sit on another
# branch, and a hard failure there would be the permanently-red gate that
# teaches everyone to stop reading it.
# check-webui-embed — the embedded bundle matches the fingerprint recorded when it
# was vendored. Runs in `check`, so CI runs it: no companion needed, no docker, no
# token (NIM-341).
#
# What it is for. `check-webui` compares the mirror against the companion's build
# and therefore cannot run in CI at all — the companion is never checked out
# there, so it takes its skip branch on every push. That left the whole class
# (NIM-273: web merged, bundle not re-synced, keeper served a stale UI) resting on
# a reviewer noticing that WEBUI_SOURCE's commit= did not move. This check is what
# makes that signal mean something: if the bundle can change WITHOUT the
# provenance changing, then "the SHA did not move" no longer implies "the bundle
# did not change", and the reviewer is reading a line that guarantees nothing.
#
# What it does NOT catch, stated so nobody mistakes its scope: a companion that
# moved on while core was never re-synced at all. No commit here touches assets in
# that scenario, so nothing inside this repository can see it — detecting it needs
# read access to the private companion, which is the open half of NIM-341.
check-webui-embed:
	@rec=$$(sed -n 's/^assets_sha256=//p' keeper/internal/webui/WEBUI_SOURCE 2>/dev/null); \
	if [ ! -d keeper/internal/webui/assets ]; then \
		echo "check-webui-embed: no embedded bundle directory - nothing to verify"; \
		exit 0; \
	fi; \
	act=$$(cd keeper/internal/webui/assets && find . -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1); \
	if [ -z "$$rec" ]; then \
		echo "check-webui-embed: FAIL - the embedded bundle carries no fingerprint."; \
		echo "  keeper/internal/webui/WEBUI_SOURCE has no assets_sha256= line, so nothing"; \
		echo "  ties the served bytes to the companion commit recorded next to them."; \
		echo "  Re-vendor through the script, which writes both: make sync-webui"; \
		exit 1; \
	fi; \
	if [ "$$rec" != "$$act" ]; then \
		echo "check-webui-embed: FAIL - the embedded bundle does not match its own provenance."; \
		echo "  recorded assets_sha256 = $$rec"; \
		echo "  actual   assets_sha256 = $$act"; \
		echo "  The bundle in keeper/internal/webui/assets/ changed without going through"; \
		echo "  scripts/sync-webui.sh, so the commit= line next to it now describes different"; \
		echo "  bytes than the ones keeper serves. Re-vendor: make sync-webui"; \
		exit 1; \
	fi; \
	echo "check-webui-embed: embedded bundle matches its recorded fingerprint ($$(echo $$rec | cut -c1-12))"

check-webui-provenance:
	@rec=$$(sed -n 's/^commit=//p' keeper/internal/webui/WEBUI_SOURCE 2>/dev/null); \
	if [ -z "$$rec" ]; then \
		echo "check-webui: no provenance recorded yet (keeper/internal/webui/WEBUI_SOURCE) - run 'make sync-webui'"; \
	else \
		head=$$(git -C $(WEBUI_REPO) rev-parse HEAD 2>/dev/null); \
		if [ "$$rec" = "$$head" ]; then \
			echo "check-webui: bundle provenance matches companion HEAD ($$rec)"; \
		else \
			echo "check-webui: NOTE - bundle was vendored from $$rec, companion HEAD is $$head."; \
			echo "  Not a failure (the companion may be on another branch), but if the UI changed"; \
			echo "  on the release branch and this SHA did not move, the embedded bundle is stale."; \
		fi; \
	fi

# keeper.dev.yml: the committed copy (dev/keeper.dev.yml) - golden, read by dev-smoke
# and docs; keeper-run/dev-smoke render the config from keeper.dev.yml.tmpl. check-stand-template
# keeps them in sync: rendering the template with an EMPTY DEV_STAND (default stand) must be
# byte-for-byte identical to the committed file. A mismatch means a .tmpl edit wasn't ported to committed. NIM-25.
STAND_TMPL := dev/keeper.dev.yml.tmpl
STAND_GOLDEN := dev/keeper.dev.yml

check-stand-template:
	@command -v envsubst >/dev/null 2>&1 || { echo "check-stand-template: envsubst not found (gettext package)"; exit 1; }
	@out=$$(mktemp); \
	DEV_STAND='' bash -c '. dev/stand-env.sh >/dev/null && envsubst "$${KEEPER_RENDER_WHITELIST}" < $(STAND_TMPL)' > "$$out"; \
	if diff -q $(STAND_GOLDEN) "$$out" >/dev/null; then \
		echo "keeper.dev.yml: committed matches the .tmpl render (default stand)"; \
		rm -f "$$out"; \
	else \
		echo "keeper.dev.yml drift: .tmpl and committed keeper.dev.yml diverged:"; \
		diff $(STAND_GOLDEN) "$$out" || true; \
		rm -f "$$out"; \
		echo ""; \
		echo "rebuild committed: bash -c '. dev/stand-env.sh && envsubst \"\$$KEEPER_RENDER_WHITELIST\" < $(STAND_TMPL)' > $(STAND_GOLDEN)"; \
		exit 1; \
	fi

# soul.dev.yml: the committed copy (dev/soul.dev.yml) - golden; keeper-run/souls-up render the
# soul config from soul.dev.yml.tmpl. check-soul-template keeps them in sync: rendering with an
# EMPTY DEV_STAND (default stand) must be byte-for-byte identical to the committed file. Symmetric with
# check-stand-template, whitelist SOUL_RENDER_WHITELIST - from dev/stand-env.sh. NIM-25.
# While dev/soul.dev.yml.tmpl isn't merged yet (soul.dev-developer zone) - the target quietly
# skips, so it doesn't break `make check` before the merge (same as check-template/check-webui without a companion).
SOUL_TMPL := dev/soul.dev.yml.tmpl
SOUL_GOLDEN := dev/soul.dev.yml

check-soul-template:
	@command -v envsubst >/dev/null 2>&1 || { echo "check-soul-template: envsubst not found (gettext package)"; exit 1; }
	@if [ ! -f "$(SOUL_TMPL)" ]; then \
		echo "check-soul-template: $(SOUL_TMPL) is missing (soul.dev zone not merged) - skipping"; \
	else \
		out=$$(mktemp); \
		DEV_STAND='' bash -c '. dev/stand-env.sh >/dev/null && envsubst "$${SOUL_RENDER_WHITELIST}" < $(SOUL_TMPL)' > "$$out"; \
		if diff -q "$(SOUL_GOLDEN)" "$$out" >/dev/null; then \
			echo "soul.dev.yml: committed matches the .tmpl render (default stand)"; \
			rm -f "$$out"; \
		else \
			echo "soul.dev.yml drift: .tmpl and committed $(SOUL_GOLDEN) diverged:"; \
			diff "$(SOUL_GOLDEN)" "$$out" || true; \
			rm -f "$$out"; \
			echo ""; \
			echo "rebuild committed via envsubst SOUL_RENDER_WHITELIST < $(SOUL_TMPL) > $(SOUL_GOLDEN)"; \
			exit 1; \
		fi; \
	fi

# check-dev-stand-build - keeps a dev stand falsifiable (NIM-342, NIM-516). Three properties:
#
#   1. no script in dev/ builds keeper or soul itself - both go through
#      `make build-keeper` / `make build-soul`, so the $(VERSION) stamp cannot be
#      dropped and the build cannot go back to being conditional on the binary
#      being absent;
#   2. keeper-run.sh holds the answering /healthz to the version it just built - a
#      foreign keeper on the port is reported as such instead of as "ready";
#   3. dev/stamp-artifact.go compiles. provision.sh `go run`s it to append the schema
#      trailer to the community.redis artifact, and it sits outside the workspace
#      modules (dev/ has no go.mod), so no other tier builds it and no other tier
#      gofmts it. Without this, an SDK change breaks stand provisioning and the gate
#      stays green until somebody tries to raise a stand - which is exactly the shape
#      of NIM-516.
#
# A grep guard rather than a Go test: the subject is shell under dev/, which no test
# binary loads. It catches the actual regression - a "simplification" back to a bare
# `go build`, or the served-version comparison being deleted. What it cannot catch is
# the scripts still working; that is what `make dev-keeper` on a live stand shows.
#
# Why it is worth a gate at all: before this, a stand served an arbitrarily old
# binary reporting `0.0.0-dev`, so an observation made against it could be neither
# confirmed nor refuted. One such observation ("a narrowly scoped role sees
# everything") cost a critical priority and a dedicated session.
check-dev-stand-build:
	@bad=$$(find dev -name '*.sh' -print0 | xargs -0 grep -lE 'go build.*(cmd/keeper|cmd/soul([^-]|$$))' 2>/dev/null || true); \
	if [ -n "$$bad" ]; then \
		echo "check-dev-stand-build: a dev script builds keeper/soul directly: $$bad"; \
		echo "  use 'make -C \"\$$REPO_ROOT\" build-keeper' / 'build-soul' instead - a bare go build"; \
		echo "  drops the -ldflags stamp and the stand then reports version 0.0.0-dev (NIM-342)."; \
		exit 1; \
	fi
	@for m in build-keeper BUILT_VERSION 'FOREIGN keeper holds this port'; do \
		grep -qF "$$m" dev/keeper-run.sh || { \
			echo "check-dev-stand-build: dev/keeper-run.sh lost the marker \"$$m\"."; \
			echo "  the script must rebuild through 'make build-keeper', read the version back out of"; \
			echo "  the artifact, and refuse when /healthz reports a different one (NIM-342)."; \
			exit 1; \
		}; \
	done
	@grep -qF 'build-soul' dev/souls-up.sh || { \
		echo "check-dev-stand-build: dev/souls-up.sh no longer rebuilds through 'make build-soul' (NIM-342)."; \
		exit 1; \
	}
	@grep -qF 'FOREIGN keeper holds this port' dev/upgrade-demo/ui-stand.sh || { \
		echo "check-dev-stand-build: dev/upgrade-demo/ui-stand.sh lost the served-version check."; \
		echo "  the demo keeper runs on its OWN port (:8090) next to the default stand, so the"; \
		echo "  foreign-binary risk is per-port, not only on :8080 (NIM-342)."; \
		exit 1; \
	}
	@out=$$(gofmt -l dev 2>&1); \
	if [ -n "$$out" ]; then \
		echo "check-dev-stand-build: gofmt: $$out"; \
		echo "  dev/ is outside \$$(MODULES), so check-fmt does not see it. run 'gofmt -w' on the listed files."; \
		exit 1; \
	fi
	@out=$$(go build -o /dev/null dev/stamp-artifact.go 2>&1) || { \
		echo "check-dev-stand-build: dev/stamp-artifact.go does not build:"; \
		echo "$$out"; \
		echo "  dev/provision.sh 'go run's it to stamp the schema trailer into the community.redis"; \
		echo "  artifact, so a broken build here means no fresh dev stand comes up (NIM-516)."; \
		exit 1; \
	}
	@echo "dev stand build: keeper/soul are rebuilt through the Makefile, keeper-run verifies the served version, dev/stamp-artifact.go builds"

# --- Release/packaging ---
# These targets are additive: NOT part of `check` (require external tooling that may
# not be installed in the dev environment). Artifacts are written to dist/ (gitignored).

# CycloneDX SBOM for the three release binaries via cyclonedx-gomod (go-tool), `app`
# mode - the SBOM of exactly what's linked into the binary (more accurate for prod-readiness
# than the whole module's graph). One file per binary in dist/sbom/. The tool isn't
# in PATH automatically; if not found - we print a go install hint and exit with an error
# (not silently). `-licenses` pulls in dependency licenses, `-json` -
# machine-readable CycloneDX, `-main` points at the main package inside the module.
#
# Why `app` and not `mod`: the repo is a go.work. `mod` mode with an active workspace builds
# the root module's SBOM for ANY module (component.name is always the first module), and with
# GOWORK=off modules with local cross-module dependencies (keeper/soul/shared)
# don't resolve (go pulls a pseudo-version from the network). `app` mode understands the workspace
# and resolves local replaces correctly. The SBOM of the three binaries covers the graph of all
# library modules (proto/sdk/shared) transitively.
SBOM_APPS := keeper:./cmd/keeper soul:./cmd/soul soul-lint:./cmd/soul-lint

sbom:
	@if ! command -v cyclonedx-gomod >/dev/null 2>&1; then \
		echo "cyclonedx-gomod not found in PATH."; \
		echo "install: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest"; \
		exit 1; \
	fi
	@mkdir -p $(SBOM_DIR)
	@for spec in $(SBOM_APPS); do \
		mod="$${spec%%:*}"; main="$${spec##*:}"; \
		out="$(SBOM_DIR)/$$mod.cdx.json"; \
		echo "cyclonedx-gomod app -main $$main ./$$mod -> $$out"; \
		cyclonedx-gomod app -licenses -json -main "$$main" -output "$$out" "./$$mod" || exit 1; \
	done
	@echo "sbom: CycloneDX SBOM written to $(SBOM_DIR)/"

# Native packages, built by goreleaser from the SAME nfpms: section a release uses
# (.goreleaser.yaml). This target used to drive nfpm against a parallel set of
# configs under deploy/nfpm/, which meant two descriptions of the same packages:
# they drifted (`make pkg` produced 3 of the 7 shipped packages, and the
# soul-stack-soul-lint -> soul-stack-lint rename had to be applied in both places
# by hand). One source of truth removes that class of bug outright - a new or
# renamed package appears here for free.
#
# Consequences of delegating, accepted deliberately:
#   - needs `goreleaser`, not `nfpm`, in PATH (both are external, both via go install).
#   - builds the whole shipped matrix - 7 packages x amd64/arm64 x deb/rpm/apk -
#     rather than one architecture, so there is no PKG_ARCH knob any more.
#   - no per-component variant: goreleaser cannot emit a single nfpm package, and
#     keeping a second set of configs just for that is the very drift this removes.
#   - `--clean` wipes $(DIST_DIR) first, so a previously generated $(SBOM_DIR)
#     goes with it; re-run `make sbom` if you need both.
# --skip: everything that is not a native package (archives, images, signing, SBOM,
# and the brew/aur/winget publishers). --snapshot builds off an untagged tree.
pkg:
	@if ! command -v goreleaser >/dev/null 2>&1; then \
		echo "goreleaser not found in PATH."; \
		echo "install: go install github.com/goreleaser/goreleaser/v2@latest"; \
		exit 1; \
	fi
	@echo "goreleaser snapshot: native packages only (VERSION=$(VERSION))"
	@goreleaser release --snapshot --clean \
		--skip=archive,docker,sign,sbom,homebrew,aur,winget || exit 1
	@mkdir -p $(PKG_DIR)
	@find $(DIST_DIR) -maxdepth 1 -type f \( -name '*.deb' -o -name '*.rpm' -o -name '*.apk' \) \
		-exec mv -t $(PKG_DIR)/ {} +
	@echo "pkg: $$(find $(PKG_DIR) -maxdepth 1 -type f \( -name '*.deb' -o -name '*.rpm' -o -name '*.apk' \) | wc -l) package(s) written to $(PKG_DIR)/"

# Image signing (cosign) - DOCUMENTED STUB. Real signing requires a registry +
# OIDC/keyless-identity (or a private key), which a local repo without
# CI/publishing doesn't have. See docs/deploy README, "Image Signing" section.
sign:
	@echo "make sign: image signing deferred (post-publish)."
	@echo "Requires a registry + cosign keyless-identity (OIDC) or a private key."
	@echo "Details and plan - the \"Image Signing (cosign)\" section in deploy/README.md."
	@exit 0

# The single local CI gate. Order: cheap static checks -> build ->
# tests (workspace + community plugins) -> drift checks -> supply-chain scan ->
# lint the examples/ corpus -> L0 trials (soul-trial).
# `test-integration` is NOT part of this - it requires docker (see the comment on
# `test`); run it separately. `vet-tags` is the docker-free half of it: it compiles
# the tag-guarded sets (integration/e2e/...) without running them, so they cannot
# rot out of sight between docker runs (NIM-207). Release/packaging targets
# (sbom/pkg/sign) are NOT part of this - external tooling.
# `check-vuln` requires access to vuln.go.dev - offline
# it's skipped via SKIP_VULNCHECK=1 (see the target), in CI it runs for real.
# `test-plugins` - go.mod plugins outside go.work (GOWORK=off). `trial` - L0-render
# over the examples/service/ corpus (catches broken case.yml assertions).
# GATE_CHECK_TIERS / GATE_L1_TIERS — the gate's tiers, in the order they run.
#
# These are a LIST, not a prerequisite chain, and that is the whole point
# (NIM-373). As prerequisites, the first red tier stopped every tier behind it
# and left no record that they had not run: three times in one release a gate
# that stopped after five of nineteen tiers was reported as "the gate was run".
# scripts/gate.sh runs each one, records PASS / FAIL / NOT RUN, and returns
# non-zero if anything is not PASS.
#
# `tier@build` declares the one causal dependency worth keeping: if compilation
# fails, the tiers that RUN the compiled code say nothing the build failure has
# not already said, so they are reported NOT RUN rather than run for a wall of
# identical errors. Everything else is independent and now behaves that way —
# an embedded-bundle drift tells you nothing about whether L1 passes, and it
# must no longer be able to prevent L1 from answering.
#
# ★ The order is the order `check` has always had, deliberately. Moving the
# static tiers in front of the tests looks like an improvement and is a trade:
# their failures would then be the ones silencing the tests, and the blind spot
# would move rather than close. With the chain gone the order no longer decides
# what runs at all, so there is nothing to buy by changing it. `vet` and
# `vet-tags` keep their place ahead of `build` and are NOT marked: they compile
# the tree themselves, so on a broken build they report the same root cause
# first-hand instead of being skipped for it.
GATE_CHECK_TIERS := check-fmt vet vet-tags build test@build test-plugins@build \
	check-integration-set check-e2e-set check-gen check-openapi@build check-template check-stand-template \
	check-soul-template check-dev-stand-build check-webui check-webui-embed check-doc-links \
	check-approle-template \
	check-vuln@build lint@build trial@build check-e2e-cloud check-gate check-ci-status
GATE_L1_TIERS := test-race@build test-integration@build e2e@build

check:
	@scripts/gate.sh check $(GATE_CHECK_TIERS)
	@echo "check: all docker-free checks passed"
	@echo "check: NOT RUN — L1 integration, L3a e2e, L3b live. This gate is docker-free BY"
	@echo "check:   DESIGN (a contributor without docker must be able to run it), so a green"
	@echo "check:   result here is silent about every defect those tiers catch. It is not a"
	@echo "check:   weaker version of CI — it is a different, smaller claim."
	@echo "check: NOT RUN — the RACE DETECTOR. \`test\` runs the unit corpus uninstrumented,"
	@echo "check:   so this gate is also silent about every data race in it, and that is where"
	@echo "check:   the concurrent code lives (async runner + barriers, console pumps, applybus"
	@echo "check:   fan-out). Docker-free, so it IS runnable here:   make test-race"
	@echo "check:   Say what CI says:   make check-all"
	@echo "check:   or one tier:        make test-race  |  make test-integration  |  make e2e"

# check-all — the composite whose green result means what a green CI run means.
#
# Why it exists (NIM-316). `make check` and CI were two different assertions
# that both ended in the word "passed", and neither implied the other: check is
# docker-free and skips L1/L3a entirely, while CI runs both (with `-race`), and
# neither of them ran the detector over the unit corpus at all (NIM-312).
# "Everything is green locally" therefore referred to something narrower than
# anyone reading it assumed — which is how a rotted L1 suite and four L3a tests
# failing since ADR-029 stayed invisible for a release (NIM-221, NIM-317).
#
# The fix is not a note in the docs: it is one command that covers the same
# ground, plus `check` stating out loud what it left out. L3b (`make e2e-live`)
# stays outside on purpose — CI does not run it either; it is nightly /
# pre-release, see docs/testing/README.md.
#
# One honest difference from CI, stated here because it bites on the first run:
# CI gives each tier its own runner, this target gives them one docker daemon, and
# it starts L3a right after ~300 container-starting L1 packages. A failure at
# container startup ("wait until ready: context deadline exceeded", "connection
# refused" against a mapped port, a Vault mount that never answered) is that
# contention, not a regression — rerun the affected package in isolation before
# believing it. Only a failure that survives the rerun is a finding. Do not
# "fix" it by loosening a readiness wait: that trades a loud infra flake for a
# quiet one.
#
# Which of the two you got is no longer left to the reader: on failure
# `test-integration` labels every failing package REGRESSION / INFRA / UNCLEAR
# and prints the `PKG=` line to rerun the suspect one alone (NIM-349).
# check-ci — "has CI verified THIS commit?", asked about a sha derived from git
# rather than read off a branch listing (NIM-339). REF= to ask about another ref.
#
# Separate from check-all on purpose: check-all runs tiers locally and works
# offline, this one is a network question about the remote's verdict. The two
# answer different things and a green one does not substitute for the other.
check-ci:
	@REF="$(REF)"; scripts/ci-status.sh $${REF:-HEAD}

# check-integration-set — the L1 package set is real, cross-checked against the
# tree (NIM-349). Docker-free, so it lives in `check`: the failure it guards
# against is a green, fast, empty L1, and that must be caught by the gate everyone
# runs rather than by the job that would be reporting the lie. Details and the
# second derivation: scripts/check-integration-set.sh.
#
# It also self-tests the L1 failure classifier. That guard exists because the
# classifier is the one piece of this work `make test` cannot see — it is not Go —
# and its first version mislabelled two container failures as REGRESSION with
# nothing going red. Pinned fixtures, one per verdict, taken from real failures.
check-integration-set:
	@scripts/check-integration-set.sh
	@scripts/classify-l1-failure.py --self-test

# check-e2e-set — the same guard check-integration-set gives L1, for the tiers
# that never had one (NIM-392). `make e2e` took its package list from `go list
# -tags=e2e` and, on an empty answer, printed a skip and exited 0: a lost L3a and
# a tree with no e2e tests were the same observation. Docker-free on purpose —
# the failure it guards against is a green, fast, empty suite, and that has to be
# caught by the gate everyone runs rather than by the job that would be doing the
# lying. Details and the second derivation: scripts/check-e2e-set.sh.
#
# It also carries the two docker-free guards the e2e-live tier owns (NIM-406),
# because `test` iterates $(MODULES) and that list has no tests/* in it — so
# without this line the only thing running them is `e2e-live-gate`, i.e. the
# 20-minute job they exist to keep honest:
#   - tests/e2e-live/harness untagged tests: the stand's readiness properties,
#     pinned so deleting a mapped-port wait is loud instead of silent;
#   - the e2e-live failure classifier's self-test, which is not Go and so is
#     invisible to `make test` — the same reason check-integration-set carries
#     the L1 one;
#   - that $(E2E_GATE_TESTS) names tests that exist (NIM-507). check-e2e-set.sh
#     above counts PACKAGES, which stays green while the list inside the one
#     package names nothing real — the gate would then run a set nobody checked
#     and label the rest NOT-RUN forever.
#
# L3a gets the same two things, for the same reasons (NIM-469):
#   - scripts/classify-l3a-failure.py's self-test. Same argument as its L1 and
#     e2e-live twins: the classifier is not Go, so `make test` never sees it,
#     and a label that silently goes wrong is worse than no label — it sends
#     someone hunting a defect that does not exist. Pinned fixtures, one per
#     family, taken from real L3a failure shapes.
#   - tests/e2e/harness's own guards. Ordinary Go tests behind the e2e tag that
#     touch no docker — AST and pure functions over the harness sources — and
#     they hold the properties the stands' readiness rests on: that each
#     container is waited on through its mapped port rather than a log line,
#     that the budgets are stated rather than inherited, and that every place
#     raising the stands bounds them by the derived one. Those are what made
#     L3a green alone and red in company, so they belong in the gate everyone
#     runs and not only in the tier that needs a docker daemon to say anything
#     at all. They cost ~0.05s. The package is its own module, hence the
#     subshell.
check-e2e-set:
	@scripts/check-e2e-set.sh
	@scripts/e2e-gate-mask.sh verify $(E2E_GATE_TESTS)
	@scripts/classify-e2e-live-failure.py --self-test
	@echo "go test -count=1 ./harness/ in tests/e2e-live (docker-free stand-readiness guards)"
	@(cd tests/e2e-live && go test -count=1 ./harness/)
	@scripts/classify-l3a-failure.py --self-test
	@echo "go test -tags=e2e -count=1 ./harness/... in tests/e2e (docker-free stand-readiness guards)"
	@(cd tests/e2e && go test -tags=e2e -count=1 ./harness/...)

# check-gate — the gate's guard on itself (NIM-373). scripts/gate.sh is what
# decides whether a tier ran and what it said, so a regression there misreports
# every other tier at once, and it would misreport them in the quiet direction:
# an early exit restored by accident looks like a gate that finished. The guard
# runs gate.sh against a throwaway Makefile of fake tiers and asserts all three
# outcomes, including that the dependent of a failed tier leaves no trace in the
# output. Docker-free, about a second.
check-gate:
	@scripts/gate-test.sh

# check-ci-status — the guard on check-ci's report (NIM-393). Same reason
# check-gate exists, one tool over: ci-status.sh is what answers "has CI verified
# THIS sha", so a regression there is invisible by construction — it misreports
# the very thing you would use to notice. It took best-outcome-per-workflow,
# which is right for an evicted attempt and wrong for a failed one: a red attempt
# followed by a green rerun printed VERIFIED with no trace of the red.
#
# The guard runs ci-status.sh against a `gh` stub serving pinned fixtures from
# the real API, applying the script's own jq filters — so trimming a filter goes
# red here rather than silently un-reporting. Docker-free and network-free (it
# has to be: a test whose subject is "the report omits nothing" cannot depend on
# which runs GitHub still retains), about a second.
check-ci-status:
	@scripts/ci-status-test.sh

check-all:
	@scripts/gate.sh check-all $(GATE_CHECK_TIERS) $(GATE_L1_TIERS)
	@echo "check-all: docker-free gate + unit -race + L1 (integration, -race) + L3a (e2e) all passed"
	@echo "check-all: this is the same claim a green CI run makes. L3b live is still NOT run:"
	@echo "check-all:   make e2e-live-gate   (curated subset, before a major batch commit)"
	@echo "check-all:   make e2e-live        (full, nightly / pre-release)"
	@echo "check-all: a container-startup failure here is contention (one docker daemon,"
	@echo "check-all:   two tiers back to back), not a regression — rerun that package alone."

# gofmt formatting across all modules. `gofmt -l` only prints files that
# differ from the canonical format; a non-empty list is a gate failure.
# Scoped by module roots (gofmt recurses into directories itself), we aggregate
# the output of a single `gofmt -l` and fail if anything was found.
check-fmt:
	@out=$$(gofmt -l $(MODULES) 2>/dev/null); \
	if [ -n "$$out" ]; then \
		echo "gofmt: the following files are not formatted:"; \
		echo "$$out"; \
		echo ""; \
		echo "run 'gofmt -w' on listed files"; \
		exit 1; \
	fi; \
	echo "gofmt: all files are formatted"

# `go vet ./...` for each module. The same skip-empty-module pattern as in
# `test`/`build` (`go list ./...` empty -> a module with no go packages, skip),
# otherwise `go vet ./...` fails with "matched no packages".
vet:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go vet ./... in $$m"; \
		(cd $$m && go vet ./...) || exit 1; \
	done

# `go vet` under the build tags a plain `go vet ./...` never builds. Tag-guarded
# files sit outside the default build, so a signature change on the other side of
# the fence rots them silently: by NIM-207 the whole `integration` set of
# `soul/cmd/soul` had been unbuildable for several tickets (`reconnectLoop` grew
# from 12 to 14 params across NIM-142/144/157 and nothing updated the call), and
# `keeper/internal/redis` the same way after `NewClient` took a password
# resolver. Neither failed an assert -- they failed to compile, so the suites
# could not run at all while the gate stayed green.
#
# vet compiles without running anything, so this stays docker-free and belongs in
# `check` (unlike `test-integration` / `e2e`, which need containers and are
# opt-in). One tag per pass: tags are not mutually compatible, and a combined
# `-tags=a,b` would build files that were never meant to coexist.
vet-tags:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go vet -tags=integration ./... in $$m"; \
		(cd $$m && go vet -tags=integration ./...) || exit 1; \
	done
	@for spec in $(TAGGED_DIRS); do \
		d=$${spec%%:*}; tag=$${spec##*:}; \
		echo "go vet -tags=$$tag ./... in $$d"; \
		(cd $$d && go vet -tags=$$tag ./...) || exit 1; \
	done

# Checks protogen idempotency (gen-drift): runs `make gen` and
# checks whether the committed generated Go changed. Scopes the diff to exactly the two
# generated-code directories - the gate shouldn't fail on unrelated working-tree
# changes. A non-empty diff means either "forgot to commit `make gen`" or
# "protogen isn't idempotent" (then it's a question for the toolchain/protoc-plugin versions).
check-gen:
	@$(MAKE) gen
	@if ! git diff --exit-code -- $(KEEPER_PROTO_OUT) $(PLUGIN_PROTO_OUT); then \
		echo ""; \
		echo "gen-drift: generated Go differs from committed"; \
		echo "commit the result of 'make gen' (or protogen isn't idempotent)"; \
		exit 1; \
	fi
	@echo "check-gen: protogen is idempotent"

# Checks the integrity of internal doc links (markdown [..](file.md#anchor) across
# all *.md, including CLAUDE.md and examples/, + docs/...#anchor in Go comments).
# The target file must exist, the anchor is generated as a GitHub slug from the heading.
# PRE-EXISTING broken links (truncated Go anchors, stale slugs) are tracked in
# scripts/doc-links-allowlist.txt and cleared out in batches during the ADR migration to docs/adr/.
check-doc-links:
	@python3 scripts/check-doc-links.py

# check-approle-template - the Vault AppRole role template we ship to operators
# must issue a PERIODIC token (NIM-429). Grouped with the doc checks rather than
# with check-template/check-stand-template: those compare a rendered artifact
# against its source, this one asserts on doc CONTENT, because no artifact is
# rendered from it - the only executor of that snippet is a human copying it out
# of the docs. Rationale in full: scripts/check-approle-template.sh.
check-approle-template:
	@scripts/check-approle-template.sh

# govulncheck - the supply-chain CI gate across all go.work modules (security audit, pre-beta).
# Symbol-scan: fails (exit 3) ONLY when a vulnerability is actually reachable through the
# code/dependency call graph - not just "present in go.sum". Same skip-empty-module
# pattern as vet/test (a module with no go packages is skipped).
#
# The binary - `go install` into $(GOPATH)/bin (the protoc-plugins pattern). If not
# found - installs the pinned version (idempotent).
#
# Offline-graceful: govulncheck pulls the vuln DB (vuln.go.dev). Without network the run
# is impossible - `SKIP_VULNCHECK=1` skips the gate with a warning (a dev machine without
# access isn't blocked). In CI the variable is NOT set -> the gate actually runs and
# must be green. Not silently-skip-by-default: skipping only via an explicit
# opt-out, otherwise a supply-chain regression would go unnoticed.
# The whole recipe is ONE shell invocation (`if ...; then ...; fi` on one logical
# line): otherwise `exit 0` in the first recipe line would only end its own sub-shell,
# and the target's following lines would still run (make runs each line in its
# own separate shell). The SKIP branch must skip the entire scan.
check-vuln:
	@if [ -n "$(SKIP_VULNCHECK)" ]; then \
		echo "check-vuln: SKIP_VULNCHECK is set - supply-chain scan skipped (offline opt-out)"; \
	else \
		if [ ! -x "$(GOVULNCHECK)" ]; then \
			echo "govulncheck not found - go install @$(GOVULNCHECK_VERSION)"; \
			go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) || exit 1; \
		fi; \
		for m in $(MODULES); do \
			if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
				echo "skip $$m (no Go packages)"; \
				continue; \
			fi; \
			echo "govulncheck ./... in $$m"; \
			(cd $$m && $(GOVULNCHECK) ./...) || exit 1; \
		done; \
		echo "check-vuln: govulncheck is clean across all modules"; \
	fi

# Offline validation of the examples/ corpus with the soul-lint linter. The binary is built
# as part of `build` (dependency). Categories: destiny / service / manifest /
# scenario. validate-scenario takes a path to scenario/<name>/main.yml
# (the scenario's entry point; secondary files are resolved via include: from main.yml).
# An empty category (no files under the glob) is skipped without error. Any
# non-zero exit from soul-lint on a committed example fails the gate.
#
# The `--modules` bindings feed `validate-scenario` (NIM-228): with them, a plugin
# module's `params:` are checked against its schema document by the same four checks
# that `core.*` already gets. Without them the linter can only say
# `plugin_params_unchecked` and move on — which is what this corpus did for every
# `community.redis` step, even though the module's own document sits in the SAME tree
# (NIM-294). The corpus is the one place where both halves are present, so leaving the
# flag off meant validating it without the check it exists to demonstrate.
#
# The flag takes `<alias>=<path>` since NIM-377, not a directory to walk. The artifact
# carries no self-name — no `namespace:`, no `name:` — so address level 1 is the
# registration alias an operator picks, and nothing in the bytes can tell the linter
# what a task will call it. It has to be stated.
#
# The binding is PER SERVICE, and that is not a convenience. The corpus holds TWO
# artifacts addressed `community.*` — soul-mod-community-redis serving module `redis`,
# soul-mod-community-mongo serving module `mongo` — and one alias names one host slot
# holding one artifact (keeper/internal/pluginhost/slot.go). So no single keeper could
# serve both, and `--modules community=A --modules community=B` is refused outright.
# One binding per lint run is exact, because no scenario in the corpus addresses both.
LINT_MODULES_REDIS ?= examples/module/soul-mod-community-redis
LINT_MODULES_MONGO ?= examples/module/soul-mod-community-mongo

lint: build
	@for f in examples/destiny/*/destiny.yml; do \
		[ -e "$$f" ] || continue; \
		echo "validate-destiny $$f"; \
		$(LINT_BIN) validate-destiny "$$f" || exit 1; \
	done
	@for f in examples/service/*/service.yml; do \
		[ -e "$$f" ] || continue; \
		echo "validate-service $$f"; \
		$(LINT_BIN) validate-service "$$f" || exit 1; \
	done
	@for f in examples/module/*/schema.json; do \
		[ -e "$$f" ] || continue; \
		echo "validate-manifest $$f"; \
		$(LINT_BIN) validate-manifest "$$f" || exit 1; \
	done
	@for f in examples/service/*/scenario/*/main.yml; do \
		[ -e "$$f" ] || continue; \
		case "$$(basename $$(dirname "$$f"))" in \
			_*|.*) echo "skip validate-scenario $$f (shared include bodies, not a scenario)"; continue;; \
		esac; \
		svc=$$(echo "$$f" | cut -d/ -f3); \
		case "$$svc" in \
			mongo) mods="--modules=community=$(LINT_MODULES_MONGO)";; \
			*)     mods="--modules=community=$(LINT_MODULES_REDIS)";; \
		esac; \
		echo "validate-scenario $$f $$mods"; \
		out=$$($(LINT_BIN) validate-scenario "$$f" "$$mods" 2>&1); rc=$$?; \
		echo "$$out"; \
		[ $$rc -eq 0 ] || exit 1; \
		if echo "$$out" | grep -F plugin_params_unchecked | grep -qv 'is a reserved name'; then \
			echo "lint: FALSE-GREEN in $$f — a plugin module in the corpus has no --modules binding," >&2; \
			echo "      so its params were NOT checked (NIM-294). Bind it above and re-run." >&2; \
			exit 1; \
		fi; \
	done
	@echo "lint: examples/ corpus is valid"

# L0 trials (soul-trial, ADR-023): render-only, hermetic. Run recursively over
# EVERY corpus directory examples/service/<svc> AND examples/destiny/<svc> with at
# least one tests/<case>/case.yml (soul-trial itself searches for case.yml recursively, including
# under _trial/scenario/.../tests/). The soul-trial binary is built as part of `build`
# (dependency). These cases used to live OUTSIDE the gate (`make lint` = soul-lint schema
# only, `make test` = go test) - so broken assertions (e.g. off-by-5 indices for
# add_node after a sentinel slice) stayed green. Now the gate actually runs
# L0. Before 2026-06-26 only examples/service/ was covered - the destiny corpus (node-exporter
# and others with _trial cases) fell outside the gate; now it's covered.
#
# The L2 harness (stand-based, requires a running stand) skips itself - we don't
# pull it into the gate; the gate's value is L0 render invariants.
#
# The skip list ($(TRIAL_SKIP)) is printed LOUDLY per directory: the exclusion is visible in the
# log, doesn't mask a regression. A directory without case.yml is silently skipped (nothing to run).
trial: build
	@for root in examples/service examples/destiny; do \
		for svc in "$$root"/*/; do \
			[ -d "$$svc" ] || continue; \
			name=$$(basename "$$svc"); \
			skip=""; \
			for s in $(TRIAL_SKIP); do [ "$$s" = "$$name" ] && skip=1; done; \
			if [ -n "$$skip" ]; then \
				echo "SKIP trial $$name (in TRIAL_SKIP - pre-existing L0-drift, see the Makefile comment)"; \
				continue; \
			fi; \
			if ! find "$$svc" -name case.yml | grep -q .; then \
				continue; \
			fi; \
			echo "soul-trial run $$svc"; \
			$(TRIAL_BIN) run "$$svc" || exit 1; \
		done; \
	done
	@echo "trial: L0 trials of the examples/service/ + examples/destiny/ corpus passed"

# --- Load testing (soul-legion) ---
# One-button run of the soul-legion load generator against the local dev stand.
# soul-legion is a shipped binary (ADR-004 Amendment 2026-07-26); it carries no
# localhost defaults of its own, so every endpoint below is passed explicitly.
# Still outside MODULES -- `make check` doesn't lint/vet it yet.
# Full plan/methodology/measured numbers -- docs/testing/load-testing.md.
#
# Precondition: a running dev stand (keeper event-stream :9443 / metrics :9090 /
# openapi :8080 + dev-PKI). The healthz-guard below checks this before build/mint and
# suggests `make dev-stand` (or `make dev-keeper`) if unavailable.
#
# Load profile is set via ENV variables (defaults below). Examples:
#   make stress                          # 1000 connections (axis A), cleanup
#   make stress COUNT=500 API=1 VOYAGE=1 # + axis B (API) + axis C (single Voyage)
#   make stress WRITE=1                  # + write axis: create->delete cycles (write+audit path)
#   make stress COUNT=2000 RAMP=500 DURATION=60s
#   make stress COUNT=10000 VOYAGE=1 VOYAGE_CONCURRENCY=100 VOYAGE_POLL=600s
#                                        # disambiguating Voyage-cliff: explicit concurrency + long poll
#   make stress COUNT=25000 ISSUE_CONCURRENCY=128
#                                        # large N: raise cert-minting parallelism in the setup phase
#
# Axis A (streams) always runs. Axes B/C/write are optional (API=1 / VOYAGE=1 /
# WRITE=1) and require admin-JWT -- it's minted by the same mechanism as `make dev-jwt`
# (dev/mint-jwt.sh, key from Vault), and passed into --jwt. Without them the token isn't
# needed (not minted).
COUNT         ?= 1000
RAMP          ?= 250
RAMP_INTERVAL ?= 300ms
DURATION      ?= 30s
COVEN         ?= legion
API           ?= 0
VOYAGE        ?= 0
# Write axis (write+audit path): create->delete cycles of safe self-cleaning
# entities (synod/role/push-provider/herald). Requires admin-JWT (like axes B/C).
WRITE          ?= 0
WRITE_DURATION ?= 15s
API_DURATION  ?= 15s
# Axis C tuning (disambiguating experiment for Voyage-cliff): VOYAGE_CONCURRENCY empty/0
# -> concurrency field is NOT sent (keeper default=1, sequential); >0 -> top-level
# voyage.concurrency in the create body. VOYAGE_POLL -- terminal-wait budget.
VOYAGE_CONCURRENCY ?=
VOYAGE_POLL        ?= 120s
# Vault-issue parallelism in the setup phase (cert minting). On large N (25k/50k)
# raise it to ~96-128 so the setup phase doesn't drag. Matches the default of the flag
# --issue-concurrency (32); ENV only overrides, we don't touch the flag default.
ISSUE_CONCURRENCY  ?= 32

# Dev-stand endpoints (checked against dev/keeper.dev.yml: event_stream :9443 /
# openapi :8080 / metrics :9090) and dev-PKI/infra (provision.sh / docker-compose).
KEEPER_ENDPOINT ?= 127.0.0.1:9443
OPENAPI         ?= http://127.0.0.1:8080
METRICS         ?= http://127.0.0.1:9090
PG              ?= postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable
VAULT           ?= http://127.0.0.1:8200
VAULT_TOKEN     ?= root
# The dev Keeper cert is issued for "localhost", while KEEPER_ENDPOINT dials
# 127.0.0.1 -- pass SNI explicitly so verification still matches.
SERVER_NAME     ?= localhost
# root CA of the Keeper server cert -- same path as listen.event_stream.tls.ca in
# dev/keeper.dev.yml (Vault PKI root, CN=soul-stack).
STRESS_CA       ?= /tmp/keeper-dev/tls/vault-ca.crt
# Health of the keeper API listener (same /healthz that dev/keeper-run.sh waits for).
STRESS_HEALTHZ  ?= http://127.0.0.1:8080/healthz

# stress -- build soul-legion -> (if API/VOYAGE) mint admin-JWT -> run ->
# clean up (--cleanup). load-test -- alias.
stress:
	@code="$$(curl -s -o /dev/null -w '%{http_code}' '$(STRESS_HEALTHZ)' 2>/dev/null || true)"; \
	if [ "$$code" != "200" ]; then \
		echo "stress: dev stand unreachable ($(STRESS_HEALTHZ) -> $$code, expected 200)."; \
		echo "  bring up the stand: 'make dev-stand' (full) or 'make dev-keeper' (keeper only)."; \
		exit 1; \
	fi
	@if [ ! -s "$(STRESS_CA)" ]; then \
		echo "stress: no dev-CA ($(STRESS_CA)) -- run 'make dev-provision' and retry."; \
		exit 1; \
	fi
	@$(MAKE) build-soul-legion
	@JWT=""; \
	if [ "$(API)" = "1" ] || [ "$(VOYAGE)" = "1" ] || [ "$(WRITE)" = "1" ]; then \
		echo "stress: minting admin-JWT (make dev-jwt mechanism) for axes B/C/write"; \
		JWT="$$(AID='$(AID)' ROLES='$(ROLES)' TTL='$(TTL)' bash dev/mint-jwt.sh)" \
			|| { echo "stress: failed to issue JWT (is Vault up? 'make dev-up' + 'make dev-provision')"; exit 1; }; \
	fi; \
	echo "stress: running soul-legion (count=$(COUNT) ramp=$(RAMP)/$(RAMP_INTERVAL) duration=$(DURATION) coven=$(COVEN) api=$(API) voyage=$(VOYAGE) write=$(WRITE))"; \
	./tests/load/bin/soul-legion \
		--keeper-endpoint='$(KEEPER_ENDPOINT)' \
		--server-name='$(SERVER_NAME)' \
		--metrics='$(METRICS)' \
		--openapi='$(OPENAPI)' \
		--pg='$(PG)' \
		--vault='$(VAULT)' \
		--vault-token='$(VAULT_TOKEN)' \
		--ca='$(STRESS_CA)' \
		--coven='$(COVEN)' \
		--count=$(COUNT) \
		--issue-concurrency=$(ISSUE_CONCURRENCY) \
		--ramp=$(RAMP) \
		--ramp-interval='$(RAMP_INTERVAL)' \
		--duration='$(DURATION)' \
		--api=$(if $(filter 1,$(API)),true,false) \
		--api-duration='$(API_DURATION)' \
		--voyage=$(if $(filter 1,$(VOYAGE)),true,false) \
		--voyage-concurrency=$(if $(VOYAGE_CONCURRENCY),$(VOYAGE_CONCURRENCY),0) \
		--voyage-poll-timeout='$(VOYAGE_POLL)' \
		--write=$(if $(filter 1,$(WRITE)),true,false) \
		--write-duration='$(WRITE_DURATION)' \
		--jwt="$$JWT" \
		--cleanup=true

load-test: stress

# Target cheat sheet. Dev-stack details -- `docs/dev/local-setup.md`.
help:
	@echo "Build and tests:"
	@echo "  gen               protoc keeper+plugin + gen-openapi → committed gen"
	@echo "  gen-openapi       huma-dump -> committed docs/keeper/openapi.yaml (derived, for UI)"
	@echo "  gen-audit-catalog audit EventType constants -> committed shared/audit/event_types_gen.go"
	@echo "  build             build keeper / soul-trial / soul / soul-lint / soulctl"
	@echo "  build-soulctl     build only soulctl (operator client CLI)"
	@echo "  test              go test ./... across all modules (no docker)"
	@echo "  test-plugins      GOWORK=off go test over go.mod plugins examples/module/* (community.redis)"
	@echo "  test-race         go test -race -count=1 ./... — the unit corpus under the detector (no docker)"
	@echo "  test-integration  go test -tags=integration -race over the tagged packages only (needs docker)"
	@echo "  e2e               L3a E2E pilot (tests/e2e, -tags=e2e, needs docker for the imp-slice)"
	@echo "  build-linux       cross-compile keeper+soul for Linux amd64 (aggregate of bin-keeper+bin-soul)"
	@echo "  bin-keeper        cross-compile only keeper (linux-amd64) -> keeper/bin/keeper-linux-amd64"
	@echo "  bin-soul          cross-compile only soul (linux-amd64) -> soul/bin/soul-linux-amd64"
	@echo "  bin-soul-lint     cross-compile only soul-lint (linux-amd64) -> soul-lint/bin/soul-lint-linux-amd64"
	@echo "  e2e-live          L3b smoke-loop (tests/e2e-live, -tags=e2e_live, privileged docker, nightly)"
	@echo "  e2e-k8s           L3c k8s-loop (tests/e2e-k8s, -tags=e2e_k8s, kind + bitnami Helm, weekly)"
	@echo "  docker-build-keeper  build the keeper:e2e-k8s image (for L3c kind load docker-image)"
	@echo "  docker-build-soul    build the soul:e2e-k8s image (privileged systemd Debian-12 for L3c-3+)"
	@echo "  tidy              go mod tidy across all modules"
	@echo ""
	@echo "Checks/gate:"
	@echo "  check             docker-free local gate (fmt+vet+build+test+test-plugins+openapi+gen+lint+trial)"
	@echo "  check-all         check + test-race + test-integration (L1) + e2e (L3a) = what a green CI run means"
	@echo "  check-ci          has CI verified THIS sha? (derives it from git; REF= for another)"
	@echo "  check-integration-set  the L1 package set matches the tree (guards a green, empty L1)"
	@echo "  check-webui-embed embedded UI bundle matches its recorded fingerprint (no companion needed)"
	@echo "  check-fmt         gofmt -l across all modules (fails on unformatted)"
	@echo "  vet               go vet ./... across all modules"
	@echo "  vet-tags          go vet under the build tags (integration/e2e/...) - compile-only, no docker"
	@echo "  check-gen         protogen idempotency (gen-drift in proto/gen/go)"
	@echo "  check-doc-links   internal doc-link integrity (markdown + Go comments)"
	@echo "  check-approle-template  shipped Vault AppRole role template issues a periodic token"
	@echo "  check-vuln        govulncheck supply-chain across all modules (offline: SKIP_VULNCHECK=1)"
	@echo "  lint              soul-lint over the examples/ corpus (destiny/service/manifest/scenario)"
	@echo "  trial             soul-trial L0 trials over the examples/service/ corpus (render invariants)"
	@echo ""
	@echo "Local dev stack:"
	@echo "  dev-up            docker compose up -d (PG / Vault / Redis)"
	@echo "  dev-stop          stop the local keeper/soul daemons of the dev workflow"
	@echo "  dev-down          dev-stop + docker compose down (data persists)"
	@echo "  dev-reset         docker compose down -v && up -d (full DB reset)"
	@echo "  dev-provision     idempotent bootstrap: Vault KV/PKI + TLS + git repo of artifacts"
	@echo "  dev-smoke         dev-up -> dev-provision -> keeper init -> dev-provision (registry seed)"
	@echo "  dev-keeper        restart keeper with a full dev-env (file:// resolve) + waits for healthz"
	@echo "  dev-jwt           issue an Archon-JWT from a Vault key (AID/ROLES/TTL); token to stdout"
	@echo "  dev-souls         re-raise local souls per the DB registry"
	@echo "  dev-web           vite dev server for the web repo (--host; WEB_DIR=<path>)"
	@echo "  dev-stand         full stand bring-up: provision -> keeper -> souls -> web"
	@echo "  dev-stand-free    free up a stand slot (DEV_STAND=<slug>): remove the row from the registry"
	@echo "  (all dev-* are stand-aware: DEV_STAND=<slug> -- a second+ stand alongside; DEDICATED_INFRA=1 -- its own docker project)"
	@echo ""
	@echo "Load testing (soul-legion, needs a running stand):"
	@echo "  stress            one-button load: build+mint-JWT+gon+cleanup (ENV: COUNT/RAMP/API/VOYAGE/WRITE/WRITE_DURATION/VOYAGE_CONCURRENCY/VOYAGE_POLL/ISSUE_CONCURRENCY/...)"
	@echo "  load-test         alias for stress"
	@echo ""
	@echo "OpenAPI:"
	@echo "  gen-openapi       regenerate committed openapi.yaml from the huma aggregator"
	@echo "  check-openapi     CI guard on drift between committed openapi.yaml and huma-dump"
	@echo "  gen-audit-catalog regenerate the audit event-type catalog feeding the AuditEvent.type enum"
	@echo "  check-template    CI guard on drift of the embedded plugin template (skip without companion)"
	@echo "  sync-webui        vendor dist/ from companion soul-stack-web -> keeper/internal/webui/assets/"
	@echo "  check-webui       CI guard on drift of the embedded UI (skip without companion)"
	@echo "  check-stand-template  CI guard on drift keeper.dev.yml.tmpl <-> committed keeper.dev.yml"
	@echo "  check-soul-template   CI guard on drift soul.dev.yml.tmpl <-> committed soul.dev.yml (skip without .tmpl)"
	@echo ""
	@echo "Release/packaging (additive, NOT part of check):"
	@echo "  docker-keeper     PROD image of keeper (multi-stage distroless) -> \$$(KEEPER_IMAGE):\$$(VERSION); push to your own registry"
	@echo "  docker-soul       PROD image of soul (multi-stage distroless) -> \$$(SOUL_IMAGE):\$$(VERSION); push to your own registry"
	@echo "  sbom              CycloneDX SBOM over go modules (cyclonedx-gomod) -> dist/sbom/"
	@echo "  pkg               native deb/rpm/apk for the whole shipped set (goreleaser) -> dist/pkg/"
	@echo "  sign              image signing (cosign) -- deferred, documented-stub"
