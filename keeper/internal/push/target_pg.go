package push

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// PGTargetReader is the narrow surface over the soul.* storage layer for the
// PG resolve of ssh_target by SID. Narrowed to a single method so
// PGFallbackTargetResolver's unit tests can substitute a fake without a live
// Postgres pool (the [SoulLookup] pattern).
//
// Production wire-up is a wrapper via [NewPGTargetReader] over pgxpool.Pool.
type PGTargetReader interface {
	SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error)
}

// pgPoolTargetReader is the production implementation of [PGTargetReader]
// over pgxpool.Pool / the corresponding soul.ExecQueryRower handle.
type pgPoolTargetReader struct {
	db soul.ExecQueryRower
}

// NewPGTargetReader adapts a pgxpool.Pool (or any soul.ExecQueryRower) to
// [PGTargetReader]. Used by setupPushDispatchers in daemon wire-up.
func NewPGTargetReader(db soul.ExecQueryRower) PGTargetReader {
	return &pgPoolTargetReader{db: db}
}

func (r *pgPoolTargetReader) SelectSshTarget(ctx context.Context, sid string) (*soul.SSHTarget, error) {
	return soul.SelectSshTarget(ctx, r.db, sid)
}

// PGFallbackTargetResolver is a PG-first resolver for SSH credentials, with
// an optional fallback to keeper.yml::push.targets[] (ADR-032 amendment
// 2026-05-26, S7-1).
//
// Resolve algorithm:
//
//  1. SELECT souls.ssh_target by SID:
//     - row not found → propagate soul.ErrSoulNotFound (sounds like an
//     invariant violation: SshDispatcher has already resolved the Soul row
//     before this point in SendApply, but it's a defensive guard);
//     - ssh_target IS NULL → go to step 2;
//     - ssh_target is set → assemble [SSHTarget], filling in defaults
//     (port 22 / user root / soul-path /usr/local/bin/soul) for omitted
//     fields. Return.
//
//  2. AllowLegacy=false (default for S7-1) → return
//     `ErrTargetNotConfigured`.
//     AllowLegacy=true → log a one-time WARN deprecation notice and
//     delegate to [Fallback] (ConfigTargetResolver over
//     keeper.yml::push.targets[]).
//
// Sentinel-error semantics match [ConfigTargetResolver]:
// `ErrTargetNotConfigured` separates "the operator didn't configure this"
// from transport errors.
type PGFallbackTargetResolver struct {
	Reader       PGTargetReader
	Fallback     TargetResolver
	AllowLegacy  bool
	Logger       *slog.Logger
	legacyWarned sync.Once
}

// Resolve implements [TargetResolver].
func (r *PGFallbackTargetResolver) Resolve(ctx context.Context, sid string) (SSHTarget, error) {
	target, err := r.Reader.SelectSshTarget(ctx, sid)
	if err != nil {
		// soul.ErrSoulNotFound is an off-path case (SshDispatcher already
		// validated the soul row before Resolve), but we propagate it
		// fail-closed so the caller sees a clear message in
		// push_runs.summary.
		return SSHTarget{}, fmt.Errorf("push: read ssh_target %s: %w", sid, err)
	}
	if target != nil {
		return SSHTarget{
			Host:     sid,
			Port:     resolveInt(target.SSHPort, defaultSSHPort),
			User:     resolveStr(target.SSHUser, defaultSSHUser),
			SoulPath: resolveStr(target.SoulPath, defaultSoulPath),
		}, nil
	}

	// PG row.ssh_target IS NULL: switch to legacy fallback if the operator
	// explicitly allowed it via the `push.allow_legacy_push_targets: true`
	// flag.
	if !r.AllowLegacy || r.Fallback == nil {
		return SSHTarget{}, fmt.Errorf("%w: %s", ErrTargetNotConfigured, sid)
	}

	r.legacyWarned.Do(func() {
		if r.Logger != nil {
			r.Logger.Warn("push: S7-1 deprecation: keeper.yml::push.targets[] используется как fallback; мигрируйте на souls.ssh_target через PUT /v1/souls/{sid}/ssh-target",
				slog.String("trigger_sid", sid))
		}
	})
	return r.Fallback.Resolve(ctx, sid)
}
