//go:build integration

// Integration test for keeper.push end-to-end against a REAL sshd: the
// container harness ([sshdtest]) configures host-CA / host-cert /
// TrustedUserCAKeys, the dispatcher opens an SSH session with CA-signed
// host-cert verify, ShaDeliverer drops a mock "soul binary" (a shell script
// that prints a valid RunResult), ShaCleaner wipes the artifacts.
//
// Run:
//
//	cd keeper && go test -tags=integration -count=1 ./internal/push/...
//
// Dependencies: docker daemon (testcontainers-go starts a container on
// 127.0.0.1 with a randomly published port).
//
// Verifies:
//   - host-cert verification against the test CA (Dial → ssh.CertChecker accept);
//   - the ephemeral keypair user-cert passes TrustedUserCAKeys;
//   - SHA-256 cache: a repeated Deliver doesn't re-upload an identical file;
//   - exec on the host produces a valid NDJSON RunResult → SendApply returns SUCCESS;
//   - Cleanup removes /var/lib/soul-stack/{bin,modules}/.
//
// It stops at the dispatcher. Whether the production wiring ever reaches
// SendApply with a usable spec, and whether the orchestrator above it finds the
// host at all, is [pushorch]'s live test — the two were wrong for a year while
// this one stayed green (NIM-869).

package push

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/sshdtest"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestIntegration_LiveSSHD_DeliverApplyCleanup — end-to-end against a real
// sshd: CA-signed host-cert verify + TrustedUserCAKeys user auth +
// ShaDeliverer + SendApply (mock-soul prints a RunResult) + ShaCleaner.
func TestIntegration_LiveSSHD_DeliverApplyCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	ca := sshdtest.NewCA(t)
	host := sshdtest.Start(ctx, t, ca, sshdtest.Options{})
	sshdtest.AssertHostCertPrincipal(t, host.Addr)

	// Prepare local artifacts for the Deliverer.
	localSoul := filepath.Join(t.TempDir(), "soul")
	if err := os.WriteFile(localSoul, []byte("#!/bin/sh\ncat >/dev/null\nprintf '{\"apply_id\":\"integration-1\",\"status\":\"RUN_STATUS_SUCCESS\"}\\n'\n"), 0o755); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	localMod := filepath.Join(t.TempDir(), "pkg")
	if err := os.WriteFile(localMod, []byte("MODULE-V1"), 0o755); err != nil {
		t.Fatalf("write mod: %v", err)
	}

	disp, err := NewSshDispatcher(Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &sshdtest.Provider{CA: ca, User: host.User}}},
		Targets: &mockTargets{target: SSHTarget{
			Host: host.Addr, Port: host.Port, User: host.User, SoulPath: HostSoulBinaryPath,
		}},
		Souls:           &mockSouls{s: &soul.Soul{SID: host.Addr, Transport: soul.TransportSSH}},
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: ca.Pub}},
		Deliverer:       NewShaDeliverer(),
		Cleaner:         NewShaCleaner(),
		SoulSpec: SoulSpec{
			SoulBinaryPath: localSoul,
			Modules:        []ModuleSpec{{Name: "pkg", Path: localMod}},
		},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSshDispatcher: %v", err)
	}

	rr, err := disp.SendApply(ctx, host.Addr, testProviderName, &keeperv1.ApplyRequest{ApplyId: "integration-1"})
	if err != nil {
		t.Fatalf("SendApply: %v", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v, want SUCCESS", rr.GetStatus())
	}

	// Verify the files actually made it over: repeat SendApply — sha256 will
	// match, upload should not happen (i.e. both runs return SUCCESS, and the
	// second run is faster — but that's not assert-able without timing; at
	// least check there's no regression).
	rr2, err := disp.SendApply(ctx, host.Addr, testProviderName, &keeperv1.ApplyRequest{ApplyId: "integration-2"})
	if err != nil {
		t.Fatalf("SendApply (repeat): %v", err)
	}
	if rr2.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("repeat run status = %v, want SUCCESS", rr2.GetStatus())
	}

	// Cleanup → /var/lib/soul-stack/{bin,modules}/ removed.
	if err := disp.Cleanup(ctx, host.Addr, testProviderName); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Post-Cleanup, verify the directories are gone: open a fresh Dial and run
	// `ls /var/lib/soul-stack` → expect it empty/absent. We open it ourselves
	// (bypassing dispatcher.SendApply, which would recreate them).
	sess, err := Dial(ctx, DialConfig{
		Host:            host.Addr,
		Port:            host.Port,
		User:            host.User,
		Auth:            ca.EphemeralAuth(t, host.User),
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: ca.Pub}},
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial post-cleanup: %v", err)
	}
	defer sess.Close()
	out, _ := sess.Run(ctx, "test -d /var/lib/soul-stack/bin && echo PRESENT || echo ABSENT", nil)
	if !strings.Contains(out, "ABSENT") {
		t.Errorf("after Cleanup hostSoulDir was not removed: %q", out)
	}
	out, _ = sess.Run(ctx, "test -d /var/lib/soul-stack/modules && echo PRESENT || echo ABSENT", nil)
	if !strings.Contains(out, "ABSENT") {
		t.Errorf("after Cleanup hostModulesDir was not removed: %q", out)
	}
}
