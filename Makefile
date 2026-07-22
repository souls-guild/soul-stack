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

# --- Release/packaging ---
# Root of build artifacts (SBOM, native packages). Entirely in .gitignore (dist/) -
# build artifacts are not committed.
DIST_DIR := dist
SBOM_DIR := $(DIST_DIR)/sbom
PKG_DIR  := $(DIST_DIR)/pkg

# Target Linux architecture for native packages (deb/rpm are always Linux).
# Overridden externally: `make pkg PKG_ARCH=arm64`. nfpm reads ${ARCH} from
# the environment for ${ARCH}-substitution in deploy/nfpm/*.yaml.
PKG_ARCH ?= amd64

# Prod-image names (docker-keeper / docker-soul targets). Local tags default to
# `soul-stack/keeper` and `soul-stack/soul`; the operator retags them for their
# own registry before push (`docker tag soul-stack/keeper:$(VERSION)
# <registry>/keeper:$(VERSION)`) or builds directly against it:
# `make docker-keeper KEEPER_IMAGE=<registry>/keeper`. The version tag is the shared
# $(VERSION) (git describe / release-override), which also lands in the binary's
# ldflags and the image's OCI label. soul-lint has no prod image (offline linter, not a prod runtime).
KEEPER_IMAGE ?= soul-stack/keeper
SOUL_IMAGE   ?= soul-stack/soul

.PHONY: gen build build-soulctl build-linux bin-keeper bin-soul bin-soul-lint test test-plugins test-race test-integration e2e e2e-live e2e-live-gate e2e-k8s e2e-cloud check-e2e-cloud docker-build-keeper docker-build-soul docker-keeper docker-soul tidy check check-fmt vet check-gen check-doc-links check-vuln lint trial dev-up dev-down dev-stop dev-reset dev-provision dev-smoke dev-keeper dev-jwt dev-souls dev-web dev-stand dev-stand-free gen-openapi check-openapi check-template check-stand-template check-soul-template sync-webui check-webui sbom pkg pkg-keeper pkg-soul pkg-soul-lint sign stress load-test help dev-souls-docker dev-souls-docker-down

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

# gen-openapi - commits docs/keeper/openapi.yaml as a DERIVED huma-generated file
# (OpenAPI 3.1, for UI-vendor + git-review). Source of truth is the huma aggregator in
# the code (HumaFullSpecYAML); there's no hand-written openapi.yaml anymore. The write is done by
# a generate-test in the api package under GEN_OPENAPI=1 (no separate cmd-binary).
# `-count=1` disables the cache (the test writes a file - nothing to cache).
gen-openapi:
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
	@echo "go build -o keeper/$(BIN_DIR)/keeper ./cmd/keeper in keeper (VERSION=$(VERSION))"
	@cd keeper && go build -ldflags '$(KEEPER_LDFLAGS)' -o $(BIN_DIR)/keeper ./cmd/keeper
	@echo "go build -o keeper/$(BIN_DIR)/soul-trial ./cmd/soul-trial in keeper"
	@cd keeper && go build -o $(BIN_DIR)/soul-trial ./cmd/soul-trial
	@echo "go build -o soul/$(BIN_DIR)/soul ./cmd/soul in soul (VERSION=$(VERSION))"
	@cd soul && go build -ldflags '$(SOUL_LDFLAGS)' -o $(BIN_DIR)/soul ./cmd/soul
	@echo "go build -o soul-lint/$(BIN_DIR)/soul-lint ./cmd/soul-lint in soul-lint"
	@cd soul-lint && go build -o $(BIN_DIR)/soul-lint ./cmd/soul-lint
	@$(MAKE) build-soulctl

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
test-race:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go test -race ./... in $$m"; \
		(cd $$m && go test -race ./...) || exit 1; \
	done

# Integration tests under the `integration` build tag (testcontainers-go).
# A separate target - `make test` doesn't need docker and stays fast.
# Files tagged `integration` are NOT built by a plain `go test ./...`, so
# we pass `-tags=integration` explicitly here. `-count=1` disables the Go test cache
# (the container spun up is new every time - nothing to cache).
test-integration:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go test -tags=integration -race -count=1 ./... in $$m"; \
		(cd $$m && go test -tags=integration -race -count=1 ./...) || exit 1; \
	done

# L3a fast-loop E2E (ADR-039): the working harness - testcontainers (PG+Redis+Vault) +
# a real Keeper process + a soul-stub with live gRPC-mTLS. A separate go module
# tests/e2e/ under the `e2e` build tag (testcontainers deps don't leak into the main
# keeper/soul). NOT part of `check` (requires docker); details in tests/e2e/README.md.
e2e:
	@if [ -z "$$(cd tests/e2e && go list -tags=e2e ./... 2>/dev/null)" ]; then \
		echo "skip tests/e2e (no Go packages under build-tag e2e)"; \
	else \
		echo "go test -tags=e2e ./... in tests/e2e"; \
		(cd tests/e2e && go test -tags=e2e -timeout=10m ./...) || exit 1; \
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
	@if [ -z "$$(cd tests/e2e-live && go list -tags=e2e_live ./... 2>/dev/null)" ]; then \
		echo "skip tests/e2e-live (no Go packages under build-tag e2e_live)"; \
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
# SHELL=bash - for `set -o pipefail` (preserve go test's exit code through tee).
e2e-live-gate: SHELL := /bin/bash
e2e-live-gate: build build-linux
	@echo "e2e-live-gate: harness unit-guard (docker-free) - WaitApplySuccess apply bracket NIM-46"
	@(cd tests/e2e-live && go test -run '^TestApplySettled$$' -count=1 ./harness/) \
		|| { echo "e2e-live-gate: FALSE-GREEN - harness unit-guard TestApplySettled failed" >&2; exit 1; }
	@if [ -z "$$(cd tests/e2e-live && go list -tags=e2e_live ./... 2>/dev/null)" ]; then \
		echo "skip tests/e2e-live (no Go packages under build-tag e2e_live)"; \
	else \
		host="$${E2E_KEEPER_HOST:-$$(hostname -I | awk '{print $$1}')}"; \
		log="$${TMPDIR:-/tmp}/soul-e2e-live-gate.log"; \
		mask='TestL3bModuleDeliveryLive|TestL3bSmokeNginxLive|TestL3bPluginChannel|TestL3bRedisLive_Day2AddUser|TestL3bRedisLive_Day2UpdateConfig|TestL3bRedisLive_Day2Restart|TestL3bRedisLive_Day2UpdateUsers|TestL3bRedisLive_Day2Destroy|TestL3bRedisLive_Day2RotateTls'; \
		echo "e2e-live-gate: go test -tags=e2e_live -v -count=1 -run '$$mask' . (E2E_KEEPER_HOST=$$host)"; \
		set -o pipefail; \
		(cd tests/e2e-live && E2E_KEEPER_HOST=$$host go test -tags=e2e_live -v -count=1 -timeout 45m -p 1 -run "$$mask" .) 2>&1 | tee "$$log"; \
		rc=$$?; \
		if grep -qE '^ok[[:space:]].*\(cached\)' "$$log"; then \
			echo "e2e-live-gate: FALSE-GREEN - '(cached)' in summary (cache not disabled, -count=1 lost)" >&2; exit 1; \
		fi; \
		for tc in TestL3bModuleDeliveryLive TestL3bSmokeNginxLive TestL3bPluginChannel TestL3bRedisLive_Day2AddUser TestL3bRedisLive_Day2UpdateConfig TestL3bRedisLive_Day2Restart TestL3bRedisLive_Day2UpdateUsers TestL3bRedisLive_Day2Destroy TestL3bRedisLive_Day2RotateTls; do \
			grep -q "^--- PASS: $$tc" "$$log" || { \
				echo "e2e-live-gate: FALSE-GREEN - $$tc didn't give '--- PASS' (skip/fail/not run)" >&2; exit 1; }; \
		done; \
		[ $$rc -eq 0 ] && echo "e2e-live-gate: OK - all gate tests actually ran (not cached, not skipped)"; \
		exit $$rc; \
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
	@if [ -z "$$(cd tests/e2e-k8s && go list -tags=e2e_k8s ./... 2>/dev/null)" ]; then \
		echo "skip tests/e2e-k8s (no Go packages under build-tag e2e_k8s)"; \
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
	@cd keeper && go build -o $(BIN_DIR)/keeper ./cmd/keeper
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

check-webui:
	@if [ ! -d "$(WEBUI_SRC)" ]; then \
		echo "companion soul-stack-web/dist not found, skipping webui-drift check"; \
	elif ! diff -r -q $(WEBUI_SRC) $(WEBUI_DST) >/dev/null; then \
		echo "embed-UI drift detected:"; \
		diff -r $(WEBUI_SRC) $(WEBUI_DST) || true; \
		echo ""; \
		echo "run 'make sync-webui' (or 'scripts/sync-webui.sh') to update the embedded copy"; \
		exit 1; \
	else \
		echo "embed-UI: copy in sync"; \
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

# Native deb + rpm packages via nfpm. Binaries are rebuilt for Linux/$(PKG_ARCH)
# (deb/rpm are always Linux, while the dev machine may be darwin) with the same ldflags as
# `build` (+ `-s -w -trimpath` for a trimmed-down prod binary). nfpm configs -
# deploy/nfpm/*.yaml; ${VERSION}/${ARCH} are substituted from the environment. The tool isn't
# in PATH automatically; if not found - we print a go install hint and exit
# with an error (not silently).
#
# Three canned recipes are reused by the per-component pkg-targets and the pkg aggregate:
#   ensure-nfpm    - guard on nfpm being present in PATH.
#   pkg-build-one  - cross-builds a single binary for linux/$(PKG_ARCH) (prod ldflags).
#                    $(1) module, $(2) cmd-package/name, $(3) version-ldflags (empty for soul-lint).
#   pkg-nfpm-one   - deb+rpm for a single nfpm config. $(1) - config name (=deploy/nfpm/$(1).yaml).
define ensure-nfpm
	@if ! command -v nfpm >/dev/null 2>&1; then \
		echo "nfpm not found in PATH."; \
		echo "install: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest"; \
		exit 1; \
	fi
endef

define pkg-build-one
	@echo "build linux/$(PKG_ARCH) $(2) for packaging (VERSION=$(VERSION))"
	@cd $(1) && CGO_ENABLED=0 GOOS=linux GOARCH=$(PKG_ARCH) go build -trimpath -ldflags '-s -w $(3)' -o $(BIN_DIR)/$(2) ./cmd/$(2)
endef

define pkg-nfpm-one
	@for fmt in deb rpm; do \
		echo "nfpm package $(1) ($$fmt)"; \
		VERSION='$(VERSION)' ARCH='$(PKG_ARCH)' nfpm package \
			--config deploy/nfpm/$(1).yaml \
			--packager $$fmt \
			--target $(PKG_DIR)/ || exit 1; \
	done
endef

# Per-component native packages: build ONLY its own binary and package it as deb+rpm.
# pkg-keeper / pkg-soul / pkg-soul-lint. The aggregate of all three is pkg.
pkg-keeper:
	$(call ensure-nfpm)
	@mkdir -p $(PKG_DIR)
	$(call pkg-build-one,keeper,keeper,$(KEEPER_LDFLAGS))
	$(call pkg-nfpm-one,keeper)
	@echo "pkg-keeper: deb+rpm written to $(PKG_DIR)/"

pkg-soul:
	$(call ensure-nfpm)
	@mkdir -p $(PKG_DIR)
	$(call pkg-build-one,soul,soul,$(SOUL_LDFLAGS))
	$(call pkg-nfpm-one,soul)
	@echo "pkg-soul: deb+rpm written to $(PKG_DIR)/"

pkg-soul-lint:
	$(call ensure-nfpm)
	@mkdir -p $(PKG_DIR)
	$(call pkg-build-one,soul-lint,soul-lint,)
	$(call pkg-nfpm-one,soul-lint)
	@echo "pkg-soul-lint: deb+rpm written to $(PKG_DIR)/"

# pkg - the aggregate: deb+rpm for all three components at once. Reuses the same
# canned recipes as the per-component targets (no duplicated logic).
pkg:
	$(call ensure-nfpm)
	@mkdir -p $(PKG_DIR)
	$(call pkg-build-one,keeper,keeper,$(KEEPER_LDFLAGS))
	$(call pkg-build-one,soul,soul,$(SOUL_LDFLAGS))
	$(call pkg-build-one,soul-lint,soul-lint,)
	$(call pkg-nfpm-one,keeper)
	$(call pkg-nfpm-one,soul)
	$(call pkg-nfpm-one,soul-lint)
	@echo "pkg: deb+rpm written to $(PKG_DIR)/"

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
# `test`); run it separately. Release/packaging targets (sbom/pkg/sign) are NOT
# part of this - external tooling. `check-vuln` requires access to vuln.go.dev - offline
# it's skipped via SKIP_VULNCHECK=1 (see the target), in CI it runs for real.
# `test-plugins` - go.mod plugins outside go.work (GOWORK=off). `trial` - L0-render
# over the examples/service/ corpus (catches broken case.yml assertions).
check: check-fmt vet build test test-plugins check-gen check-openapi check-template check-stand-template check-soul-template check-webui check-doc-links check-vuln lint trial check-e2e-cloud
	@echo "check: all checks passed"

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
	@for f in examples/module/*/manifest.yaml; do \
		[ -e "$$f" ] || continue; \
		echo "validate-manifest $$f"; \
		$(LINT_BIN) validate-manifest "$$f" || exit 1; \
	done
	@for f in examples/service/*/scenario/*/main.yml; do \
		[ -e "$$f" ] || continue; \
		echo "validate-scenario $$f"; \
		$(LINT_BIN) validate-scenario "$$f" || exit 1; \
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
# One-button run of the soul-legion load generator (tests/load/, ADR-004:
# test-only, NOT a shipped binary; outside MODULES -- `make check` doesn't touch it).
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
	@echo "go build -o tests/load/bin/soul-legion ./cmd/soul-legion in tests/load"
	@cd tests/load && go build -o bin/soul-legion ./cmd/soul-legion
	@JWT=""; \
	if [ "$(API)" = "1" ] || [ "$(VOYAGE)" = "1" ] || [ "$(WRITE)" = "1" ]; then \
		echo "stress: minting admin-JWT (make dev-jwt mechanism) for axes B/C/write"; \
		JWT="$$(AID='$(AID)' ROLES='$(ROLES)' TTL='$(TTL)' bash dev/mint-jwt.sh)" \
			|| { echo "stress: failed to issue JWT (is Vault up? 'make dev-up' + 'make dev-provision')"; exit 1; }; \
	fi; \
	echo "stress: running soul-legion (count=$(COUNT) ramp=$(RAMP)/$(RAMP_INTERVAL) duration=$(DURATION) coven=$(COVEN) api=$(API) voyage=$(VOYAGE) write=$(WRITE))"; \
	./tests/load/bin/soul-legion \
		--keeper-endpoint='$(KEEPER_ENDPOINT)' \
		--metrics='$(METRICS)' \
		--openapi='$(OPENAPI)' \
		--pg='$(PG)' \
		--vault='$(VAULT)' \
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
	@echo "  build             build keeper / soul-trial / soul / soul-lint / soulctl"
	@echo "  build-soulctl     build only soulctl (operator client CLI)"
	@echo "  test              go test ./... across all modules (no docker)"
	@echo "  test-plugins      GOWORK=off go test over go.mod plugins examples/module/* (community.redis)"
	@echo "  test-race         go test -race ./..."
	@echo "  test-integration  go test -tags=integration (testcontainers, needs docker)"
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
	@echo "  check             single local CI gate (fmt+vet+build+test+test-plugins+openapi+gen+lint+trial)"
	@echo "  check-fmt         gofmt -l across all modules (fails on unformatted)"
	@echo "  vet               go vet ./... across all modules"
	@echo "  check-gen         protogen idempotency (gen-drift in proto/gen/go)"
	@echo "  check-doc-links   internal doc-link integrity (markdown + Go comments)"
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
	@echo "  pkg               native deb+rpm packages for all three components (nfpm) -> dist/pkg/"
	@echo "  pkg-keeper        deb+rpm only keeper (nfpm) -> dist/pkg/"
	@echo "  pkg-soul          deb+rpm only soul (nfpm) -> dist/pkg/"
	@echo "  pkg-soul-lint     deb+rpm only soul-lint (nfpm) -> dist/pkg/"
	@echo "  sign              image signing (cosign) -- deferred, documented-stub"
