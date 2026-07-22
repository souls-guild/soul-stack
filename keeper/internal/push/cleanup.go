package push

import (
	"context"
	"errors"
	"fmt"
)

// Cleaner removes soul artifacts from a push host after a run (binary +
// plugins). Called by the operator explicitly via [SshDispatcher.Cleanup],
// not automatically at the end of SendApply: repeat runs benefit from the
// SHA-256 delivery cache, and cleaning between them would add pointless
// roundtrips. Cleanup makes sense when retiring a host from the push flow /
// swapping the soul agent.
//
// Logs/journal (`/var/log/soul-stack/` etc.) are NOT touched — that's an
// audit trail, not a short-lived install zone.
type Cleaner interface {
	Cleanup(ctx context.Context, session Session) error
}

// ShaCleaner — the default implementation over ssh exec `rm -rf`. The host
// layout is fixed (`/var/lib/soul-stack/{bin,modules}/`), so the path never
// comes from outside and shell injection is impossible.
type ShaCleaner struct{}

// NewShaCleaner — constructor for DI-explicitness.
func NewShaCleaner() *ShaCleaner { return &ShaCleaner{} }

// Cleanup removes hostSoulDir and hostModulesDir along with their contents.
// /var/log/soul-stack/ is intentionally NOT touched (audit data).
//
// fail-closed: a nonzero `rm` exit (e.g. permissions) propagates up, so the
// caller can't assume the host was cleaned up "somehow".
func (c *ShaCleaner) Cleanup(ctx context.Context, session Session) error {
	if session == nil {
		return errors.New("push/cleanup: session is nil")
	}
	cmd := fmt.Sprintf("rm -rf %s %s", hostSoulDir, hostModulesDir)
	if _, err := session.Run(ctx, cmd, nil); err != nil {
		return fmt.Errorf("push/cleanup: rm -rf %s %s: %w", hostSoulDir, hostModulesDir, err)
	}
	return nil
}
