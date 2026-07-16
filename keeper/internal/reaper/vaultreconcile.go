package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// vaultreconcile.go implements the cross-store Reaper rule
// `reap_orphan_vault_keys` (report-only, ADR-026(h), GATE-2). Unlike [Purger],
// which is pure pgx DELETE, this reconciles two stores: Vault KV, containing
// names of Sigil signing private keys, against the Postgres
// `sigil_signing_keys` registry, the authoritative set of live keys. That is
// why this is a separate type instead of a Purger method.
//
// An orphan is a Vault secret `secret/keeper/sigil-keys/<key_id>` that has NO
// row in `sigil_signing_keys` in ANY status. active AND retired count as live
// because a retired private key is still needed to verify old Sigils. This can
// happen, for example, if Introduce wrote the private key to Vault but the
// following PG insert failed. keyservice intentionally does NOT do reverse
// cleanup, so the Vault secret remains abandoned.
//
// SAFETY, the rule invariant:
//   - report-only: the rule ONLY counts, emits metrics, and logs; it deletes
//     nothing from Vault.
//   - the private key is NEVER read: only LIST names and metadata (created_time)
//     are used, and the secret data path is never requested.
//   - scope-prefix [orphanScanPrefix] is HARDCODED, not read from config, so the
//     rule physically cannot scan another Vault path.

// orphanScanPrefix is the only Vault prefix scanned by the rule. It is
// HARDCODED as a scope guard: orphan logic is meaningful ONLY for Sigil signing
// private keys. This is the relative form; the vault client supplies the mount.
const orphanScanPrefix = "keeper/sigil-keys"

// orphanLogCap is how many orphaned-secret key_id values to log by name. The
// rest is folded into "and N more" so Warn logs do not explode during mass
// drift. key_id is SHA-256 SPKI hex, a public identifier, so logging it is safe
// because the private key never enters the rule.
const orphanLogCap = 20

// VaultKVLister is the narrow vault-client subset needed by reconcile: LIST
// names under prefix plus read metadata (created_time) for grace. Real
// [*keepervault.Client] satisfies it automatically; tests use a fake. It
// intentionally does NOT include ReadKV(data) because the private key is not
// read, preserving the security invariant. Exported for wiring in
// keeper/cmd/keeper (typed-nil guard).
type VaultKVLister interface {
	ListKV(ctx context.Context, prefix string) ([]string, error)
	ReadKVMetadata(ctx context.Context, path string) (time.Time, error)
}

// liveKeyIDsReader resolves the authoritative set of live key_id values from
// Postgres (`sigil.ListAllKeyIDs` over pool, all statuses). It is a narrow
// interface for unit-test replacement without starting PG.
type liveKeyIDsReader interface {
	ListAllKeyIDs(ctx context.Context) (map[string]struct{}, error)
}

// VaultReconciler runs `reap_orphan_vault_keys`. There is one instance per
// Keeper process; it is concurrency-safe because the vault client and pool are
// thread-safe and it has no mutable state of its own.
type VaultReconciler struct {
	vault VaultKVLister
	keys  liveKeyIDsReader
	log   *slog.Logger
	now   func() time.Time
}

// NewVaultReconciler builds the runner. vault may be nil when Vault is not
// configured; then [VaultReconciler.ReportOrphanVaultKeys] returns (0, error)
// with a clear message, and runner logs the failure and continues. The rule is
// disabled by default, so a typical deploy does not call it.
//
// now is the time source for grace comparison; nil means [time.Now]. Tests
// replace it with a fixed time.
func NewVaultReconciler(vault VaultKVLister, keys liveKeyIDsReader, log *slog.Logger, now func() time.Time) *VaultReconciler {
	if now == nil {
		now = time.Now
	}
	return &VaultReconciler{vault: vault, keys: keys, log: log, now: now}
}

// ReportOrphanVaultKeys is report-only: it finds orphaned Sigil signing private
// keys in Vault and returns their count. It deletes NOTHING.
//
// Algorithm:
//  1. LIST names under [orphanScanPrefix], the Vault metadata path, without
//     reading data.
//  2. ListAllKeyIDs gives the live set from Postgres, all statuses.
//  3. candidates are Vault names missing from the set.
//  4. For each candidate, up to batchSize metadata round trips per run,
//     ReadKVMetadata; if created_time is older than now()-grace, it is an
//     orphan. grace filters out the Introduce race (write-before-PG-commit): a
//     fresh secret may still get a PG row.
//  5. Return orphan count and Warn-log key_id values capped by [orphanLogCap].
//
// grace is passed by runner from rule.MaxAge (MaxAge-as-grace, like
// purge_apply_task_register). batchSize limits metadata reads per run.
func (vr *VaultReconciler) ReportOrphanVaultKeys(ctx context.Context, grace time.Duration, batchSize int) (int64, error) {
	if vr.vault == nil {
		return 0, errors.New("reaper: reap_orphan_vault_keys requires a Vault client (vault not configured)")
	}

	names, err := vr.vault.ListKV(ctx, orphanScanPrefix)
	if err != nil {
		return 0, fmt.Errorf("reaper: list vault sigil-keys: %w", err)
	}
	if len(names) == 0 {
		// No orphans, or the subfolder is empty/missing; exit without calling PG.
		return 0, nil
	}

	live, err := vr.keys.ListAllKeyIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("reaper: list live sigil key ids: %w", err)
	}

	// candidates are in Vault but not in PG, neither active nor retired.
	candidates := make([]string, 0)
	for _, keyID := range names {
		if _, ok := live[keyID]; ok {
			continue
		}
		candidates = append(candidates, keyID)
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	cutoff := vr.now().Add(-grace)
	var orphans []string
	var checked int
	for _, keyID := range candidates {
		// batchSize limits metadata round trips per run.
		if batchSize > 0 && checked >= batchSize {
			break
		}
		checked++

		created, err := vr.vault.ReadKVMetadata(ctx, orphanScanPrefix+"/"+keyID)
		if err != nil {
			// The secret may have been deleted between LIST and read, or transport
			// failed. Do not fail the whole run; skip the candidate with a Warn.
			vr.log.Warn("reaper: read vault metadata for orphan candidate failed, skipping",
				slog.String("rule", "reap_orphan_vault_keys"),
				slog.String("key_id", keyID),
				slog.Any("error", err),
			)
			continue
		}
		// grace: a young secret may still receive a PG row through Introduce
		// write-before-commit, so do not count it as an orphan.
		if created.After(cutoff) {
			continue
		}
		orphans = append(orphans, keyID)
	}

	if len(orphans) > 0 {
		logged := orphans
		extra := 0
		if len(logged) > orphanLogCap {
			extra = len(logged) - orphanLogCap
			logged = logged[:orphanLogCap]
		}
		attrs := []any{
			slog.String("rule", "reap_orphan_vault_keys"),
			slog.Int("orphan_count", len(orphans)),
			slog.Any("orphan_key_ids", logged),
		}
		if extra > 0 {
			attrs = append(attrs, slog.Int("orphan_key_ids_omitted", extra))
		}
		// report-only: record the finding; the operator handles it manually.
		vr.log.Warn("reaper: orphan vault sigil-keys detected (report-only, nothing deleted)", attrs...)
	}

	return int64(len(orphans)), nil
}
