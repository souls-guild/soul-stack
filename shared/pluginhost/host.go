// Package pluginhost is the shared runtime for Soul Stack plugin hosts
// (ADR-020, docs/keeper/plugins.md).
//
// It implements the kind-agnostic part of a plugin subprocess lifecycle:
// fork → handshake → dial gRPC → graceful shutdown. Kind-specific wrappers
// (SoulModule / CloudDriver / SshProvider) live in host-side packages
// (`soul/internal/pluginhost`, `keeper/internal/pluginhost`) and build on top
// of [BasePlugin].
//
// Schema-document reading lives in [shared/plugin]; this package receives an
// already-validated document via [Discovered], one entry per module.
//
// # One spawn, one module
//
// An artifact serves several modules and dispatch is a subcommand
// (`soul-mod-redis acl`, NIM-377). Every [Host.Spawn] therefore names exactly one
// module, and everything the host decides from the declaration — the capability check,
// the disclosure it reports — reads that module alone. Widening one module's
// declaration with another's would approve a footprint nobody agreed to.
package pluginhost

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Host is the shared runtime for spawning any plugin kind. Safe for concurrent
// [Host.Spawn] calls.
//
// Config comes from `<binary>.yml::plugin_runtime` (shared/config.PluginRuntime).
// A nil config applies ADR-020(d) defaults, except SocketDir — it is
// kind-host-specific and must be set via the [NewHost] defaultSocketDir
// parameter.
type Host struct {
	// SocketDir is where the host creates Unix sockets for plugins. Mode 0700,
	// owned by the service user. Created via mkdir on first Spawn (idempotent).
	SocketDir string
	// StartupTimeout is the window from exec to the handshake line.
	StartupTimeout time.Duration
	// ShutdownGrace is the window from SIGTERM to SIGKILL.
	ShutdownGrace time.Duration
	// AllowedCapabilities is the closed set of capabilities allowed on this
	// host, holding pluginv1.Capability values for fast comparison.
	// nil/empty = all capabilities allowed (ADR-020(d) default).
	AllowedCapabilities map[pluginv1.Capability]struct{}
	// SigilAnchors is the SET of Sigil verify trust anchors (ADR-026(h), R3
	// multi-anchor): an atomically swappable holder of the public ed25519 keys
	// that sign plugin grants delivered at bootstrap. Verify passes if ANY
	// anchor in the set confirms the signature (OR — seamless key rotation). An
	// empty set (or nil holder) = Sigil not configured on the Keeper → verify of
	// any custom plugin is fail-closed (reason no_trust_anchor). The holder is
	// read as a snapshot in [Host.Spawn]; S6 will swap the set at runtime via
	// [AnchorSet.SetAnchors] without a restart. Core modules are static and do
	// not undergo Spawn-verify — this field does not affect them.
	SigilAnchors *AnchorSet
	// Sigils is the read surface for active Sigil grants by registration alias,
	// for fail-closed verify in [Host.Spawn]. nil = no grants → verify fail-closed
	// (reason no_sigil). DI: the Soul-side adapter wraps the Sigil runtime cache
	// so shared does NOT pull in keeper-proto.
	Sigils SigilLookup
}

// ADR-020(d) defaults. DefaultSocketDir is deliberately NOT exported: each host
// (soul / keeper) sets its own via [NewHost] to avoid path collisions under
// different service users.
const (
	DefaultStartupTimeout = 10 * time.Second
	DefaultShutdownGrace  = 10 * time.Second
)

// sockCounter is a monotonic counter for Unix-socket name uniqueness across
// concurrent Spawns (UnixNano on one machine can collide for goroutines within
// the same ms, especially under the race detector).
var sockCounter atomic.Uint64

// NewHost builds a Host from the shared config block, applying ADR-020(d)
// defaults for missing fields. Duration strings are already validated in the
// shared/config semantic phase; the repeat ParseDuration here is
// defense-in-depth (the host is only built with a valid config).
//
// defaultSocketDir is the kind-host-specific default (e.g.
// `/var/run/soul-stack/plugins` for soul, `/var/run/soul-stack-keeper/plugins`
// for keeper); used when cfg.SocketDir is empty.
func NewHost(cfg *config.PluginRuntime, defaultSocketDir string) (*Host, error) {
	h := &Host{
		SocketDir:      defaultSocketDir,
		StartupTimeout: DefaultStartupTimeout,
		ShutdownGrace:  DefaultShutdownGrace,
	}
	if cfg == nil {
		return h, nil
	}
	if cfg.SocketDir != "" {
		h.SocketDir = cfg.SocketDir
	}
	if cfg.StartupTimeout != "" {
		d, err := parseDuration(cfg.StartupTimeout)
		if err != nil {
			return nil, fmt.Errorf("pluginhost: plugin_runtime.startup_timeout: %w", err)
		}
		h.StartupTimeout = d
	}
	if cfg.ShutdownGrace != "" {
		d, err := parseDuration(cfg.ShutdownGrace)
		if err != nil {
			return nil, fmt.Errorf("pluginhost: plugin_runtime.shutdown_grace: %w", err)
		}
		h.ShutdownGrace = d
	}
	if len(cfg.AllowedCapabilities) > 0 {
		h.AllowedCapabilities = make(map[pluginv1.Capability]struct{}, len(cfg.AllowedCapabilities))
		for _, s := range cfg.AllowedCapabilities {
			c, ok := sharedplugin.CapabilityFromString(s)
			if !ok {
				return nil, fmt.Errorf("pluginhost: plugin_runtime.allowed_capabilities: unknown %q", s)
			}
			h.AllowedCapabilities[c] = struct{}{}
		}
	}
	return h, nil
}

// CheckCapabilities checks the capabilities THIS MODULE declares against the host's
// allowed list. Returns an error on the first disallowed capability. nil
// AllowedCapabilities = everything allowed (default).
//
// The scope is the module, not the artifact: a spawn of `redis.acl` is checked against
// what `acl` declares, and `config` in the same artifact contributes nothing. Checking
// the union would let an unrelated module's declaration decide whether this one may
// run — in both directions, since a union both over-restricts and over-approves.
func (h *Host) CheckCapabilities(d Discovered) error {
	if h.AllowedCapabilities == nil {
		return nil
	}
	for _, c := range d.Capabilities() {
		pc, ok := sharedplugin.CapabilityFromString(string(c))
		if !ok {
			// Should not happen — the document was already validated; the host
			// checks anyway rather than silently ignoring it.
			return fmt.Errorf("pluginhost: unknown capability %q in %s", c, d.Address())
		}
		if _, allowed := h.AllowedCapabilities[pc]; !allowed {
			return fmt.Errorf("pluginhost: capability %q is not allowed by host (module %s)", c, d.Address())
		}
	}
	return nil
}

// SpawnOption is a functional option for [Host.Spawn]: the caller passes extra
// spawn-cycle settings without widening the method signature or mutating
// [Discovered] (which is the discovery result, shared across all spawns).
//
// Pilot example (ADR-020 amendment l, 2026-05-26) — env-payload of
// provider-specific params for SshProvider plugins via [WithEnv]: the caller
// serializes params to JSON into an env variable with the fixed name
// `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS`, which the plugin reads at startup.
type SpawnOption func(*spawnOpts)

type spawnOpts struct {
	extraEnv []string
}

// WithEnv adds extra `KEY=VALUE` pairs to the spawned plugin's environment.
// Applied on top of [os.Environ] and [handshake.SocketEnv]; on a name collision
// the extraEnv entry wins (the last entry in `cmd.Env` takes precedence per exec
// semantics). Used by the env-convention mode of plugin param delivery
// (ADR-020 amendment l).
func WithEnv(env []string) SpawnOption {
	return func(o *spawnOpts) { o.extraEnv = append(o.extraEnv, env...) }
}

// Spawn forks the artifact for ONE named module, reads the handshake, and dials gRPC.
// Returns a [BasePlugin] ready to accept RPC via [BasePlugin.Conn]. On any error
// (timeout, handshake drift, dial fail) the plugin process is stopped and the socket
// removed — no BasePlugin is returned.
//
// The module travels as argv: `soul-mod-redis acl`. d.Module says which one, and it is
// the only one this spawn discloses, checks or serves; an artifact asked for a module
// it does not have exits non-zero rather than falling through to another (sdk/module
// → ServeBundle). A single-endpoint kind (cloud_driver / ssh_provider / soul_beacon)
// has no module and is spawned with no arguments.
//
// One-shot per Spawn contract (ADR-020(d)): the caller invokes Spawn before each
// RPC series and [BasePlugin.Close] after. Connection pooling is not provided —
// this simplifies task isolation and removes proxied state between RPCs.
//
// Kind-specific gRPC client stubs (SoulModuleClient / CloudDriverClient /
// SshProviderClient) are built by the caller from [BasePlugin.Conn] — this
// package is kind-agnostic.
//
// opts are optional functional spawn-cycle settings (e.g. [WithEnv] for the
// env-payload of plugin params).
func (h *Host) Spawn(ctx context.Context, d Discovered, opts ...SpawnOption) (*BasePlugin, error) {
	var so spawnOpts
	for _, opt := range opts {
		opt(&so)
	}
	if d.Doc == nil {
		return nil, fmt.Errorf("pluginhost: %s: no schema document (the artifact was never read)", d.Address())
	}
	if err := h.CheckCapabilities(d); err != nil {
		return nil, err
	}
	// Integrity gate (ADR-026, S6b): fail-closed verify of the artifact against the
	// Sigil trust seal BEFORE any exec. Replaces the first-load TOFU branch: an
	// artifact with no valid grant (no_sigil), no trust anchor (no_trust_anchor), a
	// mismatched digest (digest_mismatch), or a broken signature (bad_signature) →
	// *VerifyError, plugin NOT started. This is the one real control on the spawn
	// path — the operator approved a sha256, and nothing else may exec. The re-exec
	// sidecar recheck stays inside as defense-in-depth. See docs/keeper/plugins.md →
	// Integrity-model.
	//
	// Verify is per ARTIFACT, not per module: a grant approves bytes, and all of an
	// artifact's modules are the same bytes.
	if err := verifySigilAndSeal(d.Dir, d.BinaryPath, d.Alias, h.SigilAnchors.snapshot(), h.Sigils); err != nil {
		return nil, fmt.Errorf("pluginhost: %s: %w", d.Address(), err)
	}
	if err := os.MkdirAll(h.SocketDir, 0o700); err != nil {
		return nil, fmt.Errorf("pluginhost: mkdir socket dir %q: %w", h.SocketDir, err)
	}
	// The socket name omits the plugin pid — that is only known after exec. We
	// use the host process pid + an atomic counter for uniqueness; the name
	// format matters for logs, not the protocol (the plugin reads the path from env).
	sockName := fmt.Sprintf("%s-%d-%d.sock",
		strings.ReplaceAll(d.Address(), ".", "-"), os.Getpid(), sockCounter.Add(1))
	sockPath := filepath.Join(h.SocketDir, sockName)

	// Dispatch is a subcommand (NIM-377): the host decides which module runs, per
	// spawn. The artifact never picks for itself — which module runs decides which
	// host gets changed.
	var argv []string
	if d.Module != "" {
		argv = append(argv, d.Module)
	}
	cmd := exec.CommandContext(ctx, d.BinaryPath, argv...)
	cmd.Env = append(os.Environ(), handshake.SocketEnv+"="+sockPath)
	// Extra env from the caller (ADR-020 amendment l, env-convention for
	// SshProvider). Appended AFTER the handshake socket: the last entry in
	// `cmd.Env` wins per exec semantics, so the caller can in theory override
	// anything — a deliberate caller responsibility (the host does not
	// introspect the payload, only passes it).
	if len(so.extraEnv) > 0 {
		cmd.Env = append(cmd.Env, so.extraEnv...)
	}
	cmd.Dir = d.Dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pluginhost: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("pluginhost: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pluginhost: start plugin %s: %w", d.BinaryPath, err)
	}

	// Plugin stderr is the diagnostics channel (ADR-020 → plugins.md → Handshake).
	// Tail the last 4KB into a ring buffer for failure reporting.
	errTail := newTailBuffer(4 * 1024)
	go func() { _, _ = io.Copy(errTail, stderr) }()

	hs, err := readHandshake(stdout, h.StartupTimeout)
	if err != nil {
		_ = killAndWait(cmd, h.ShutdownGrace)
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("pluginhost: %s: %w (stderr-tail: %s)", d.Address(), err, errTail.String())
	}

	if err := validateHandshake(d.Doc, hs, sockPath); err != nil {
		_ = killAndWait(cmd, h.ShutdownGrace)
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("pluginhost: %s: %w (stderr-tail: %s)", d.Address(), err, errTail.String())
	}

	conn, err := grpc.NewClient("unix:"+sockPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = killAndWait(cmd, h.ShutdownGrace)
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("pluginhost: %s: dial unix %s: %w", d.Address(), sockPath, err)
	}

	return &BasePlugin{
		discovered: d,
		cmd:        cmd,
		conn:       conn,
		sockPath:   sockPath,
		stderrTail: errTail,
		grace:      h.ShutdownGrace,
	}, nil
}

// parseDuration wraps time.ParseDuration with a `<N>d` extension for days
// (docs/keeper/config.md → type conventions). The shared/config semantic phase
// does the same; duplicated here for defense-in-depth.
func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// killAndWait is a best-effort plugin stop: SIGTERM, wait grace, SIGKILL.
// Returns the cmd.Wait error for logging (not for control flow: on the
// hard-fail path the handshake error matters more).
func killAndWait(cmd *exec.Cmd, grace time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		return <-done
	}
}
