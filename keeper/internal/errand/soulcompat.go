package errand

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Soul-side engine-compat gate for the Errand contour (ADR-0076(i)) — the
// single-host sibling of the roster gates in keeper/internal/scenario. Same axis,
// same fail-closed shape, one target instead of N.
//
// What it closes is specific to `dry_run`. ADR-031(b)/(c) promises a PURE READ:
// the Soul calls SoulModule.Plan instead of Apply, so the operator can ask "what
// would this do" without touching the host. That promise is structural only if
// the binary on the far end implements the flag — a Soul predating it reads
// ErrandRequest by the keys it knows, misses `dry_run`, and runs Apply. For the
// modules Errand can actually reach that means a shell command executed for real
// (`core.cmd.shell` / `core.exec.run`) or a live HTTP call (`core.http.probe`):
// a mutation inside an operation that advertised a read.
//
// So the capability the Soul announces (config.CapabilityDryRun) is checked
// BEFORE the request goes out. The check-drift gate on the scenario side used to
// be the only reader of that announcement and left with the drift circuit
// (NIM-446); Errand, the remaining producer of a dry_run request, never had one
// (NIM-456).
//
// Only a dry_run dispatch pays for this. An ordinary Errand — the overwhelming
// majority — asks nothing of the checker and is unaffected by its absence.

// Sentinel errors of the dry-run capability gate. Both refuse the same request
// for the same reason (dry_run cannot be honored here) and both map to one HTTP
// status; they are separate values because the operator's next move differs —
// upgrade the agent, or fix the presence source.
var (
	// ErrDryRunNotAnnounced — the target holds a lease (so it is connected) and
	// its announced capability set does not carry config.CapabilityDryRun.
	// Usually an agent predating the flag; possibly a current one whose
	// announcement never reached the presence store. Either way keeper cannot
	// rule out a binary that would apply for real, so it refuses — but it does
	// not claim to know which, because the presence layer cannot tell them apart
	// (see [Dispatcher.gateDryRun]).
	ErrDryRunNotAnnounced = errors.New("errand: target soul did not announce the dry_run capability")

	// ErrDryRunUnverifiable — support could not be confirmed either way: no
	// presence checker wired (no Redis) or the check itself failed. Refused
	// rather than attempted, because the cost of guessing wrong is a real Apply
	// on a host the operator only asked to read. Resolution: restore the
	// presence source, or dispatch without dry_run.
	ErrDryRunUnverifiable = errors.New("errand: cannot confirm the target soul honors dry_run")
)

// SoulCapabilityChecker answers "which of these SIDs did NOT announce this
// capability" — the narrow surface [Dispatcher] needs of the heartbeat presence
// hash. Satisfied at wire-up by a thin wrapper over
// redis.SoulsLackingCapability, the same function the scenario-side gates use
// (one implementation, two callers, no second notion of what an announcement
// means).
type SoulCapabilityChecker interface {
	SoulsLackingCapability(ctx context.Context, sids []string, capability string) ([]string, error)
}

// gateDryRun refuses a dry-run dispatch unless the target host announced
// config.CapabilityDryRun. Fail-closed on every edge — a missing announcement, a
// nil checker, and a checker failure all reject; nothing here falls through to
// "probably supported".
//
// Called only when DispatchRequest.DryRun is set, and before the errands row
// exists: a refused request never invoked anything, so it leaves no row to
// explain and no `errand.invoked` claiming it did.
//
// The three-way split of the refusal is the fiddly part, and it is not cosmetic —
// each answer sends the operator somewhere different, and the presence layer
// cannot make the distinction on its own. [redis.SoulsLackingCapability] reports
// a SID as lacking when the caps field says so AND when the heartbeat hash is
// absent entirely (its own fail-closed default), so "announced a set without
// dry_run" and "has no presence record at all" arrive identical. Taken at face
// value that turns a typo'd SID into "update the soul binary on that host",
// advice about a host that may not exist. So a lacking verdict is cross-checked
// against the lease, the same connectivity signal [Dispatcher.send] uses: no
// holder means the Soul is not connected to any instance — the documented 404,
// and a target nothing could have been dispatched to anyway.
//
// A lease read that fails leaves the verdict at not-announced. Both answers are
// refusals, so nothing unsafe follows from guessing here; only the diagnosis can
// be off, and the message is worded not to overclaim.
func (d *Dispatcher) gateDryRun(ctx context.Context, sid string) error {
	if d.deps.SoulCap == nil {
		return fmt.Errorf(
			"errand: dry_run for %s requires confirmation that the host honors it, but the presence checker is unavailable (no Redis) - fail-closed refusal (ADR-0076(i)): %w",
			sid, ErrDryRunUnverifiable)
	}
	lacking, err := d.deps.SoulCap.SoulsLackingCapability(ctx, []string{sid}, config.CapabilityDryRun)
	if err != nil {
		return fmt.Errorf(
			"errand: dry_run capability check for %s failed - fail-closed refusal (ADR-0076(i)): %w: %w",
			sid, ErrDryRunUnverifiable, err)
	}
	if len(lacking) == 0 {
		return nil
	}
	if d.deps.LeaseLookup != nil {
		if holder, lerr := d.deps.LeaseLookup.ReadHolder(ctx, sid); lerr == nil && holder == "" {
			return fmt.Errorf(
				"errand: dry_run for %s: the host holds no session lease, so it is not connected to any keeper instance (its absent presence record is what the capability check saw): %w",
				sid, ErrSoulNotConnected)
		}
	}
	return fmt.Errorf(
		"errand: host %s does not announce %q in its capability set, so keeper cannot rule out a binary that would apply for real instead of planning - refused before dispatch (ADR-031(b), ADR-0076(i)): %w",
		sid, config.CapabilityDryRun, ErrDryRunNotAnnounced)
}
