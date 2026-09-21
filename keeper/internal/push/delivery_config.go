package push

import (
	"fmt"
	"os"

	"github.com/souls-guild/soul-stack/shared/config"
)

// DeliveryFromConfig decides what a push run ships to the host before
// `soul apply`, from `keeper.yml::push`.
//
// It is a function, rather than two lines inside the daemon's dispatcher
// wire-up, because that wire-up is 120 lines of plugin spawning that no test
// brings up — which is how NIM-869 happened: the daemon passed
// `Deliverer: NewShaDeliverer()` and left `SoulSpec` zero, so [ShaDeliverer.
// Deliver] returned "SoulBinaryPath is required" on every run, and the only
// thing that would have caught it was a test of this decision.
//
// Three outcomes, all of them explicit:
//   - path unset → (nil, zero spec, nil): delivery OFF. The dispatcher execs
//     whatever is already at the target's SoulPath. A nil Deliverer is the
//     documented way to say that ([Deps.Deliverer]); pairing a non-nil
//     Deliverer with an empty spec is the bug, not a configuration.
//   - path set and readable → (ShaDeliverer, spec, nil).
//   - path set and NOT readable → an error, and the daemon refuses to start.
//     The operator asked for delivery and it cannot happen; starting anyway
//     means a daemon that fails every push run at the same point it did before
//     this key existed.
//
// [SoulSpec.Modules] stays empty: see the note on that field.
func DeliveryFromConfig(cfg *config.KeeperPush) (Deliverer, SoulSpec, error) {
	if cfg == nil || cfg.SoulBinaryPath == "" {
		return nil, SoulSpec{}, nil
	}
	// Opened, not stat'd: every run reads these bytes ([fileSha256]), so a file
	// the keeper user cannot open is the once-per-run failure this key exists to
	// prevent, and os.Stat succeeds on it.
	f, err := os.Open(cfg.SoulBinaryPath) //nolint:gosec // the operator's own keeper.yml path
	if err != nil {
		return nil, SoulSpec{}, fmt.Errorf("push.soul_binary_path %q: %w", cfg.SoulBinaryPath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, SoulSpec{}, fmt.Errorf("push.soul_binary_path %q: %w", cfg.SoulBinaryPath, err)
	}
	if info.IsDir() {
		return nil, SoulSpec{}, fmt.Errorf("push.soul_binary_path %q is a directory, want the soul binary", cfg.SoulBinaryPath)
	}
	return NewShaDeliverer(), SoulSpec{SoulBinaryPath: cfg.SoulBinaryPath}, nil
}
