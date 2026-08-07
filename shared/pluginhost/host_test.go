package pluginhost

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/config"
)

func TestNewHostDefaults(t *testing.T) {
	const defaultDir = "/var/run/soul-stack/plugins"
	h, err := NewHost(nil, defaultDir)
	if err != nil {
		t.Fatalf("NewHost(nil): %v", err)
	}
	if h.SocketDir != defaultDir {
		t.Errorf("SocketDir = %q, want %q", h.SocketDir, defaultDir)
	}
	if h.StartupTimeout != DefaultStartupTimeout {
		t.Errorf("StartupTimeout = %v, want %v", h.StartupTimeout, DefaultStartupTimeout)
	}
	if h.ShutdownGrace != DefaultShutdownGrace {
		t.Errorf("ShutdownGrace = %v, want %v", h.ShutdownGrace, DefaultShutdownGrace)
	}
	if h.AllowedCapabilities != nil {
		t.Errorf("AllowedCapabilities = %v, want nil (all allowed)", h.AllowedCapabilities)
	}
}

func TestNewHostFromConfig(t *testing.T) {
	cfg := &config.PluginRuntime{
		SocketDir:           "/tmp/plugins",
		StartupTimeout:      "5s",
		ShutdownGrace:       "2s",
		AllowedCapabilities: []string{"network_outbound", "vault_access"},
	}
	h, err := NewHost(cfg, "/var/run/soul-stack/plugins")
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if h.SocketDir != "/tmp/plugins" {
		t.Errorf("SocketDir = %q", h.SocketDir)
	}
	if h.StartupTimeout != 5*time.Second {
		t.Errorf("StartupTimeout = %v", h.StartupTimeout)
	}
	if h.ShutdownGrace != 2*time.Second {
		t.Errorf("ShutdownGrace = %v", h.ShutdownGrace)
	}
	if _, ok := h.AllowedCapabilities[pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND]; !ok {
		t.Errorf("network_outbound not in AllowedCapabilities")
	}
	if _, ok := h.AllowedCapabilities[pluginv1.Capability_CAPABILITY_VAULT_ACCESS]; !ok {
		t.Errorf("vault_access not in AllowedCapabilities")
	}
}

func TestNewHostRejectsBadDuration(t *testing.T) {
	cfg := &config.PluginRuntime{StartupTimeout: "5kg"}
	if _, err := NewHost(cfg, "/tmp"); err == nil {
		t.Fatal("expected error for bad duration")
	}
}

func TestNewHostRejectsUnknownCapability(t *testing.T) {
	cfg := &config.PluginRuntime{AllowedCapabilities: []string{"magic"}}
	if _, err := NewHost(cfg, "/tmp"); err == nil {
		t.Fatal("expected error for unknown capability")
	}
}

// mixedBundle is one artifact whose two modules need different capabilities — the
// shape every per-module check is about.
func mixedBundle(t *testing.T) map[string]Discovered {
	t.Helper()
	found := discoveredFor(t, "redis", soulModuleDoc(
		modDef("acl", []schema.Capability{schema.NetworkOutbound}, nil),
		modDef("config", []schema.Capability{schema.VaultAccess}, nil),
	), exitScript)
	byAddr := make(map[string]Discovered, len(found))
	for _, d := range found {
		byAddr[d.Address()] = d
	}
	return byAddr
}

// GUARD: the capability check reads the module being spawned, not the union across the
// artifact. `acl` needs network_outbound and the host allows it; `config` in the very
// same artifact needs vault_access and is refused. A union would have failed both, or
// passed both.
func TestCheckCapabilitiesIsPerModule(t *testing.T) {
	h, _ := NewHost(&config.PluginRuntime{AllowedCapabilities: []string{"network_outbound"}}, "/tmp")
	mods := mixedBundle(t)

	if err := h.CheckCapabilities(mods["redis.acl"]); err != nil {
		t.Errorf("CheckCapabilities(redis.acl): %v", err)
	}
	err := h.CheckCapabilities(mods["redis.config"])
	if err == nil {
		t.Fatal("expected denial for redis.config (vault_access)")
	}
	if !strings.Contains(err.Error(), "vault_access") {
		t.Errorf("error %q does not mention vault_access", err.Error())
	}
	if strings.Contains(err.Error(), "network_outbound") {
		t.Errorf("error %q mentions a sibling module's capability", err.Error())
	}
}

// GUARD, the other direction: a sibling's capability must not be enough to let a
// module through. The host allows only vault_access, so `acl` (network_outbound) is
// refused even though `config` in the same artifact would pass.
func TestCheckCapabilitiesSiblingDoesNotWiden(t *testing.T) {
	h, _ := NewHost(&config.PluginRuntime{AllowedCapabilities: []string{"vault_access"}}, "/tmp")
	mods := mixedBundle(t)

	if err := h.CheckCapabilities(mods["redis.acl"]); err == nil {
		t.Fatal("redis.acl passed the check on its sibling's capability")
	}
	if err := h.CheckCapabilities(mods["redis.config"]); err != nil {
		t.Errorf("CheckCapabilities(redis.config): %v", err)
	}
}

func TestCheckCapabilitiesNoFilterAllowsAll(t *testing.T) {
	h, _ := NewHost(nil, "/tmp") // AllowedCapabilities == nil = all allowed.
	found := discoveredFor(t, "redis", soulModuleDoc(
		modDef("acl", []schema.Capability{
			schema.NetworkOutbound, schema.VaultAccess, schema.ExecSubprocess,
		}, nil),
	), exitScript)

	if err := h.CheckCapabilities(found[0]); err != nil {
		t.Errorf("CheckCapabilities with nil filter: %v", err)
	}
}

// TestSpawnWithoutSigilRefused — without a Sigil trust seal (the grant didn't
// arrive and no trust-anchor is configured) Spawn is fail-closed before exec and
// does NOT seal the sidecar: first-load no longer trusts "as-is" (ADR-026, S6b).
// The digest_mismatch tamper scenario and other fail-closed reasons are covered in
// sigil_verify_test.go.
func TestSpawnWithoutSigilRefused(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "redis", soulModuleDoc(modDef("acl", nil, nil)), exitScript)
	found, warns := DiscoverSlot("redis", dir)
	if len(warns) != 0 {
		t.Fatalf("discover: %v", warns)
	}

	h, _ := NewHost(nil, filepath.Join(t.TempDir(), "sock"))
	// SigilAnchors and Sigils are unset → no trust-anchors and no grants.
	_, err := h.Spawn(context.Background(), found[0])
	if !errors.Is(err, ErrSigilVerify) {
		t.Fatalf("expected ErrSigilVerify (fail-closed), got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, DigestSidecarName)); !os.IsNotExist(serr) {
		t.Fatalf("sidecar must NOT be sealed without Sigil, stat err = %v", serr)
	}
}

// Spawn needs a document: an entry that never went through discovery has no
// disclosure, and a host that spawned it would be running code nobody described.
func TestSpawnWithoutDocumentRefused(t *testing.T) {
	h, _ := NewHost(nil, filepath.Join(t.TempDir(), "sock"))
	_, err := h.Spawn(context.Background(), Discovered{Alias: "redis", Module: "acl"})
	if err == nil || !strings.Contains(err.Error(), "no schema document") {
		t.Fatalf("err = %v, want a refusal about the missing document", err)
	}
}

// argvScript records the artifact's arguments and then emits a valid handshake, so a
// successful Spawn proves what the host actually passed on the command line.
const argvScript = `#!/bin/sh
printf '%s\n' "$@" > "$SPAWN_ARGV_FILE"
printf '{"soul_stack":"plugin-v1","protocol_version":1,"kind":"KIND_SOUL_MODULE","network":"unix","address":"%s"}\n' "$SOUL_PLUGIN_SOCKET"
exit 0
`

// GUARD: the module travels as argv. Spawning `redis.acl` runs the artifact as
// `<artifact> acl`, and spawning `redis.config` runs the same file as
// `<artifact> config` — which module runs decides which host gets changed, so it can
// never be left to the artifact.
func TestSpawnPassesTheModuleAsArgv(t *testing.T) {
	e := setupSigilEnvForBundle(t, argvScript)
	h := e.host(t, true)

	for _, module := range []string{"acl", "config"} {
		argvFile := filepath.Join(t.TempDir(), "argv")
		p, err := h.Spawn(context.Background(), e.byAddr["redis."+module],
			WithEnv([]string{"SPAWN_ARGV_FILE=" + argvFile}))
		if err != nil {
			t.Fatalf("Spawn(redis.%s): %v", module, err)
		}
		_ = p.Close()

		got, rerr := os.ReadFile(argvFile)
		if rerr != nil {
			t.Fatalf("artifact did not record its argv: %v", rerr)
		}
		if strings.TrimSpace(string(got)) != module {
			t.Errorf("argv = %q, want %q", strings.TrimSpace(string(got)), module)
		}
	}
}

// A refused digest means the artifact is never executed at all — not executed and then
// judged. The recording file the artifact would have written stays absent.
func TestSpawnDigestMismatchDoesNotExec(t *testing.T) {
	e := setupSigilEnvForBundle(t, argvScript)
	h := e.host(t, true)
	e.rec.BinarySHA256hex = strings.Repeat("ab", 32)

	argvFile := filepath.Join(t.TempDir(), "argv")
	_, err := h.Spawn(context.Background(), e.byAddr["redis.acl"],
		WithEnv([]string{"SPAWN_ARGV_FILE=" + argvFile}))
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonDigestMismatch {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonDigestMismatch)
	}
	if _, serr := os.Stat(argvFile); !os.IsNotExist(serr) {
		t.Fatalf("the artifact executed despite a digest mismatch (stat err = %v)", serr)
	}
}

// bundleEnv is a two-module artifact with a valid grant — the fixture for spawn-level
// tests, where the same bytes must be reachable under two addresses.
type bundleEnv struct {
	sigilTestEnv
	byAddr map[string]Discovered
}

func setupSigilEnvForBundle(t *testing.T, script string) bundleEnv {
	t.Helper()
	dir := t.TempDir()
	doc := soulModuleDoc(
		modDef("acl", nil, nil),
		modDef("config", nil, nil),
	)
	binPath := writeArtifact(t, dir, testAlias, doc, script)

	found, warns := DiscoverSlot(testAlias, dir)
	if len(warns) != 0 || len(found) != 2 {
		t.Fatalf("discover bundle: found=%d warns=%v", len(found), warns)
	}
	schemaDoc, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	byAddr := make(map[string]Discovered, len(found))
	for _, d := range found {
		byAddr[d.Address()] = d
	}
	return bundleEnv{
		sigilTestEnv: sigilTestEnv{
			dir:        dir,
			binPath:    binPath,
			discovered: found[0],
			rec: &SigilRecord{
				Alias:           testAlias,
				Source:          testSource,
				Ref:             testRef,
				BinarySHA256hex: found[0].Digest,
				Signature:       signFixture(t, priv, testSource, testRef, found[0].Digest, schemaDoc),
				Schema:          schemaDoc,
			},
			pub: pub,
		},
		byAddr: byAddr,
	}
}
