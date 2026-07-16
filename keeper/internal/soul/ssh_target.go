package soul

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SSHTarget is the typed form of the souls.ssh_target jsonb column (ADR-032
// amendment 2026-05-26, S7-1; extended by amendment 2026-05-27, P2 W-1). Holds
// per-host SSH credentials for the push flow directly in the souls registry,
// replacing the pilot form `keeper.yml::push.targets[]` (S6).
//
// Defaults for omitted fields (port 22 / user root / soul-path /usr/local/bin/soul)
// are resolved NOT here but in keeper/internal/push.PGFallbackTargetResolver:
// storage holds ONLY what the operator explicitly set (NULL/0/"" → default at
// resolve time). This lets defaults change centrally in the future without a
// data migration.
//
// `SSHProvider` (P2 W-1) — an optional per-SID explicit choice of SshProvider
// plugin (Level 1 of the 3-tier resolve: per-SID → per-coven → cluster-default).
// nil → routing falls to levels 2/3 (see keeper/internal/push/router.go::PGRouter).
// Same name format as push_providers.name (regex `^[a-z][a-z0-9-]{0,62}$`),
// validated by CHECK `souls_ssh_target_shape` (migration 056).
type SSHTarget struct {
	SSHPort     int     `json:"ssh_port"`
	SSHUser     string  `json:"ssh_user"`
	SoulPath    string  `json:"soul_path"`
	SSHProvider *string `json:"ssh_provider,omitempty"`
}

const updateSshTargetSQL = `
UPDATE souls
SET ssh_target = $2::jsonb
WHERE sid = $1
RETURNING sid
`

const selectSshTargetSQL = `
SELECT ssh_target
FROM souls
WHERE sid = $1
`

// UpdateSshTarget updates souls.ssh_target by sid. target==nil writes
// NULL to the column (clear a configured target → fall back to keeper.yml
// under the push.allow_legacy_push_targets flag).
//
// Audit-event and RBAC-permission are checked by the caller (handler / MCP tool) —
// the soul.* layer stays storage-neutral to authz, symmetric with UpdateCoven /
// UpdateStatus.
//
// Returns [ErrSoulNotFound] if the SID doesn't exist. An invalid jsonb shape
// is rejected by the PG CHECK souls_ssh_target_shape (migration 053) — this
// surfaces as a CHECK violation wrapped in a generic error; the handler side
// matches on the constraint text, but in normal flow an invalid shape is
// impossible (the caller marshals from a typed SSHTarget).
func UpdateSshTarget(ctx context.Context, db ExecQueryRower, sid string, target *SSHTarget) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q (must match %s)", sid, SIDPattern)
	}
	var payload any
	if target != nil {
		b, err := json.Marshal(target)
		if err != nil {
			return fmt.Errorf("soul: marshal ssh_target: %w", err)
		}
		payload = b
	}
	var actualSID string
	err := db.QueryRow(ctx, updateSshTargetSQL, sid, payload).Scan(&actualSID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSoulNotFound
	}
	if err != nil {
		return fmt.Errorf("soul: update ssh_target: %w", err)
	}
	return nil
}

// SelectSshTarget reads souls.ssh_target by SID. Returns:
//
//   - (target, nil) — the Soul record exists, ssh_target is set;
//   - (nil, nil) — the Soul record exists, ssh_target IS NULL (target not
//     configured — falls back to keeper.yml in PGFallbackTargetResolver);
//   - (nil, ErrSoulNotFound) — no Soul record in the registry;
//   - (nil, fmt.Errorf-wrapped) — a DB error or malformed JSONB in the column
//     (CHECK souls_ssh_target_shape makes this impossible, but the guard stays).
//
// This is a hot-path method (called from SshDispatcher.SendApply on every push),
// hence no round-trips beyond one SELECT.
func SelectSshTarget(ctx context.Context, db ExecQueryRower, sid string) (*SSHTarget, error) {
	if !ValidSID(sid) {
		return nil, fmt.Errorf("soul: invalid SID %q (must match %s)", sid, SIDPattern)
	}
	var payload []byte
	err := db.QueryRow(ctx, selectSshTargetSQL, sid).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSoulNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("soul: select ssh_target: %w", err)
	}
	if len(payload) == 0 {
		return nil, nil
	}
	var target SSHTarget
	if err := json.Unmarshal(payload, &target); err != nil {
		return nil, fmt.Errorf("soul: unmarshal ssh_target: %w", err)
	}
	return &target, nil
}
