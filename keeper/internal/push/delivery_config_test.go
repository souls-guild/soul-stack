package push

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// The wire-up used to hand the dispatcher a Deliverer and no SoulSpec, and
// Deliver refuses an empty SoulBinaryPath — so every push run died on the line
// after connect, and nothing in the tree said so (NIM-869). These are the
// guards on the three outcomes of that decision.

func TestDeliveryFromConfig_UnsetPathTurnsDeliveryOff(t *testing.T) {
	deliverer, spec, err := DeliveryFromConfig(&config.KeeperPush{})
	if err != nil {
		t.Fatalf("DeliveryFromConfig: %v", err)
	}
	if deliverer != nil {
		t.Errorf("Deliverer = %T, want nil — with no artifact to ship, delivery is off", deliverer)
	}
	if spec.SoulBinaryPath != "" || len(spec.Modules) != 0 {
		t.Errorf("spec = %+v, want zero", spec)
	}
}

func TestDeliveryFromConfig_NilPushBlock(t *testing.T) {
	deliverer, _, err := DeliveryFromConfig(nil)
	if err != nil || deliverer != nil {
		t.Fatalf("DeliveryFromConfig(nil) = (%T, %v), want (nil, nil)", deliverer, err)
	}
}

func TestDeliveryFromConfig_PathFeedsASpecDeliverAccepts(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "soul")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write soul: %v", err)
	}

	deliverer, spec, err := DeliveryFromConfig(&config.KeeperPush{SoulBinaryPath: bin})
	if err != nil {
		t.Fatalf("DeliveryFromConfig: %v", err)
	}
	if deliverer == nil {
		t.Fatal("Deliverer is nil although push.soul_binary_path is set")
	}
	if spec.SoulBinaryPath != bin {
		t.Fatalf("spec.SoulBinaryPath = %q, want %q", spec.SoulBinaryPath, bin)
	}

	// The pairing the daemon depends on: the spec must be one the real
	// Deliverer accepts. Contrasted against the zero spec that shipped, and
	// over a NON-nil session — Deliver's nil-session guard runs first, so a
	// nil one answers the same for both specs and would prove nothing.
	sess := &mockSession{stdout: "MISSING\n"}
	if err := deliverer.Deliver(context.Background(), sess, SoulSpec{}); err == nil ||
		!strings.Contains(err.Error(), "SoulBinaryPath is required") {
		t.Fatalf("Deliver(zero spec) = %v; the guard that killed every push run must still fire", err)
	}
	if err := deliverer.Deliver(context.Background(), sess, spec); err != nil &&
		strings.Contains(err.Error(), "SoulBinaryPath is required") {
		t.Fatalf("Deliver rejected the spec DeliveryFromConfig built: %v", err)
	}
}

func TestDeliveryFromConfig_MissingBinaryStopsTheDaemon(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-soul")
	if _, _, err := DeliveryFromConfig(&config.KeeperPush{SoulBinaryPath: missing}); err == nil {
		t.Fatal("a configured push.soul_binary_path that does not exist must fail at start, " +
			"not once per run")
	}
}

func TestDeliveryFromConfig_DirectoryIsRejected(t *testing.T) {
	if _, _, err := DeliveryFromConfig(&config.KeeperPush{SoulBinaryPath: t.TempDir()}); err == nil {
		t.Fatal("a directory is not the soul binary")
	}
}

// TestDeliveryFromConfig_UnreadableBinaryStopsTheDaemon — existence is not the
// property the daemon needs. Every run reads these bytes to hash them, so a
// file the keeper user cannot open is exactly the once-per-run failure this
// check exists to move to start-up, and os.Stat is happy with it.
func TestDeliveryFromConfig_UnreadableBinaryStopsTheDaemon(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny a read")
	}
	bin := filepath.Join(t.TempDir(), "soul")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o000); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	if _, _, err := DeliveryFromConfig(&config.KeeperPush{SoulBinaryPath: bin}); err == nil {
		t.Fatal("an unreadable push.soul_binary_path must fail at start, not once per run")
	}
}
