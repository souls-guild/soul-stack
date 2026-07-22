package pluginhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
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

func TestCheckCapabilities(t *testing.T) {
	h, _ := NewHost(&config.PluginRuntime{AllowedCapabilities: []string{"network_outbound"}}, "/tmp")

	allowed := &sharedplugin.Manifest{
		Kind: sharedplugin.KindSoulModule, ProtocolVersion: 1, Namespace: "acme", Name: "ok",
		RequiredCapabilities: []string{"network_outbound"},
	}
	if err := h.CheckCapabilities(allowed); err != nil {
		t.Errorf("CheckCapabilities(allowed): %v", err)
	}

	forbidden := &sharedplugin.Manifest{
		Kind: sharedplugin.KindSoulModule, ProtocolVersion: 1, Namespace: "acme", Name: "bad",
		RequiredCapabilities: []string{"vault_access"},
	}
	err := h.CheckCapabilities(forbidden)
	if err == nil {
		t.Fatalf("expected denial for vault_access")
	}
	if !strings.Contains(err.Error(), "vault_access") {
		t.Errorf("error %q does not mention vault_access", err.Error())
	}
}

func TestCheckCapabilitiesNoFilterAllowsAll(t *testing.T) {
	h, _ := NewHost(nil, "/tmp") // AllowedCapabilities == nil = all allowed.
	m := &sharedplugin.Manifest{
		Kind: sharedplugin.KindSoulModule, ProtocolVersion: 1, Namespace: "acme", Name: "ok",
		RequiredCapabilities: []string{"network_outbound", "vault_access", "exec_subprocess"},
	}
	if err := h.CheckCapabilities(m); err != nil {
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
	binPath := filepath.Join(dir, "soul-mod-x")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}

	h, _ := NewHost(nil, filepath.Join(t.TempDir(), "sock"))
	// SigilAnchors and Sigils are unset → no trust-anchors and no grants.
	d := Discovered{
		Manifest: &sharedplugin.Manifest{
			Kind: sharedplugin.KindSoulModule, ProtocolVersion: 1, Namespace: "acme", Name: "x",
		},
		BinaryPath: binPath,
		Dir:        dir,
	}

	_, err := h.Spawn(context.Background(), d)
	if !errors.Is(err, ErrSigilVerify) {
		t.Fatalf("expected ErrSigilVerify (fail-closed), got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, DigestSidecarName)); !os.IsNotExist(serr) {
		t.Fatalf("sidecar must NOT be sealed without Sigil, stat err = %v", serr)
	}
}
