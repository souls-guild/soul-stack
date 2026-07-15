// Package bootstrap implements `keeper init` logic (ADR-013).
//
// Init, under a PG advisory lock, checks that the operators registry is
// empty, inserts the first Archon (`created_by_aid: NULL`), issues a JWT
// (TTL `auth.jwt.ttl_bootstrap`, claim `bootstrap_initial: true`, role
// `cluster-admin`), writes an `operator.created` audit event (source
// `keeper_internal`, `archon_aid: NULL`) and saves the token to a file
// with `mode 0400`. A repeat call on a non-empty registry returns
// [ErrAlreadyInitialized].
//
// The package does not manage the lifecycle of the Postgres pool / Vault
// client / JWT issuer — the caller (`keeper/cmd/keeper`) assembles the
// dependencies and passes them via [Config]; bootstrap logic is purely
// orchestrational.
package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// AdvisoryLockID is the int64 literal for the bootstrap PG advisory lock.
// The value `0x534f554c` is ASCII `"SOUL"` (one byte per character).
// All Keeper cluster nodes see the same lock namespace; even if two
// `keeper init` run concurrently, the second blocks until the first's
// COMMIT, after which it sees a non-empty registry and fails with
// [ErrAlreadyInitialized].
const AdvisoryLockID int64 = 0x534f554c

// BootstrapRoleClusterAdmin is the only role issued to the first
// Archon per ADR-013/rbac.md.
const BootstrapRoleClusterAdmin = "cluster-admin"

// vaultSigningKeyField is the field name inside the Vault KV secret that
// holds the JWT signing-key (base64-encoded). Matches the golden format
// seeded by both the integration tests and the `vault kv put` command in
// local-dev.
const vaultSigningKeyField = "signing_key"

// credentialFileMode is the permission for the JWT token file (read-only
// owner). ADR-013(c): the JWT must not be readable by other users.
const credentialFileMode os.FileMode = 0o400

// ErrAlreadyInitialized means at least one non-system operator already
// exists (`CountNonSystem > 0` under the advisory lock). The caller maps
// this to exit code 1.
var ErrAlreadyInitialized = errors.New("bootstrap: keeper already initialized (operators registry not empty)")

// ErrSigningKeyMissing is returned when the Vault KV has no
// `signing_key` field, or it is empty.
var ErrSigningKeyMissing = errors.New("bootstrap: signing_key field missing or empty in Vault KV")

// ErrAuditWriteFailed is returned when the audit write fails AFTER the
// operator insert's COMMIT has succeeded. The operator is already in the
// DB, the audit is lost — the caller should warn the administrator that
// manual reconciliation is needed. The error wraps the original pgx
// error for diagnostics.
var ErrAuditWriteFailed = errors.New("bootstrap: audit write failed")

// ErrTokenFileWriteFailed is returned when writeTokenFile fails AFTER
// both the operator insert's COMMIT and the audit write have succeeded.
// I.e. the DB is consistent, the audit is in place, but the JWT file was
// not saved.
//
// Recovery strategy (PM-decision M0.5c review:b): the caller prints the
// JWT to stderr with a warning "token compromised — rotate ASAP". The
// alternative (write before COMMIT via TempFile+Rename) was rejected:
// it doesn't guard against runtime issues in writeTokenFile itself
// (e.g. permission on the target dir is only known at write time).
//
// Result.Token is populated ONLY on ErrTokenFileWriteFailed — on the
// happy path the token is absent from Result (it lives in the file).
var ErrTokenFileWriteFailed = errors.New("bootstrap: token file write failed")

// JWTIssuer is a narrow interface over `keeper/internal/jwt.Issuer`. The
// narrowing is needed for unit tests (without loading a signing-key and
// golang-jwt). The real `*jwt.Issuer` satisfies the interface
// automatically.
type JWTIssuer interface {
	Issue(aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error)
}

// Config holds all the dependencies Init needs. Populated in
// `keeper/cmd/keeper`; does not parse keeper.yml itself.
type Config struct {
	// ArchonAID is the AID of the new Archon (flag `--archon`). Must pass
	// [operator.ValidAID].
	ArchonAID string

	// DisplayName is the display_name in the registry. If empty,
	// ArchonAID is used instead (PM-decision #5).
	DisplayName string

	// TTLBootstrap is the TTL of the first Archon's JWT token. Taken from
	// `keeper.yml::auth.jwt.ttl_bootstrap` (default 720h).
	TTLBootstrap time.Duration

	// Pool is a pgxpool.Pool with migrations applied (003 + 004 already
	// created `operators` and its FKs).
	Pool *pgxpool.Pool

	// VaultClient is used to read the signing-key. Required (nil → error
	// in validateConfig). Unit tests that check logic without Vault go
	// through integration (integration_test.go) — there is no mock Vault
	// inside the package.
	VaultClient *keepervault.Client

	// SigningKeyRef is the string from
	// `keeper.yml::auth.jwt.signing_key_ref` in the form `vault:<path>`.
	// Parsed by [parseVaultRef]. An empty string or malformed value is
	// an error.
	SigningKeyRef string

	// IssuerFactory builds a JWT issuer from signingKey. Tests pass a
	// mock; keeper/cmd/keeper passes the real jwt.NewIssuer.
	IssuerFactory func(signingKey []byte) (JWTIssuer, error)

	// AuditWriter is where the `operator.created` event is written.
	AuditWriter audit.Writer

	// CredentialOutput is the path to the file the JWT token is written
	// to. Empty string falls back to [defaultCredentialPath].
	CredentialOutput string
}

// Result is the return value of a successful Init. Used by the caller
// for the final stdout message; the token in Result is NOT logged
// (exception: ErrTokenFileWriteFailed recovery, see the Token field).
type Result struct {
	// CredentialPath is where the token was actually written (after
	// fallback).
	CredentialPath string

	// AuditID is the ID of the corresponding audit_log record.
	AuditID string

	// CorrelationID is a ULID tied to the bootstrap chain (for any
	// subsequent related events).
	CorrelationID string

	// Token is populated ONLY when returning the [ErrTokenFileWriteFailed]
	// error (recovery path: the caller prints the token to stderr with a
	// rotation warning). On the happy path the field is empty — the token
	// lives in the file and isn't duplicated in Result to avoid an
	// accidental log leak.
	Token string
}

// Init performs bootstrap of the first Archon.
//
// Sequence:
//  1. Validate Config (minimum: ArchonAID, TTLBootstrap, Pool,
//     SigningKeyRef, IssuerFactory, AuditWriter).
//  2. Read signing-key from Vault KV (mount/path from SigningKeyRef).
//  3. Build the JWT issuer.
//  4. BEGIN tx → `pg_advisory_xact_lock(AdvisoryLockID)` →
//     `CountNonSystem(operators)`; >0 → rollback + [ErrAlreadyInitialized].
//  5. Insert operator (created_by_aid=NULL).
//  6. Issue JWT.
//  7. COMMIT.
//  8. Audit event `operator.created` (after COMMIT — otherwise we could
//     record an audit for a "phantom" insert that got rolled back).
//  9. Save the JWT to the credentialOutput file (mode 0400).
//
// The audit is written AFTER COMMIT: an audit-writer failure must not
// roll back a successful insert (the Archon is already the source of
// truth in the DB). If the audit write fails, Init returns an error, but
// the DB state remains consistent.
func Init(ctx context.Context, cfg Config) (*Result, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	path, err := keepervault.ParseRef(cfg.SigningKeyRef)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: signing_key_ref: %w", err)
	}
	// path is passed in logical form (`<mount>/<rel>`); Client strips
	// the prefix itself. ReadKV also tolerates a relative form.
	kv, err := cfg.VaultClient.ReadKV(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: read vault %q: %w", path, err)
	}
	signingKey, err := extractSigningKey(kv)
	if err != nil {
		return nil, err
	}
	issuer, err := cfg.IssuerFactory(signingKey)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: build jwt issuer: %w", err)
	}

	displayName := cfg.DisplayName
	if displayName == "" {
		displayName = cfg.ArchonAID
	}

	// The transaction holds the advisory lock for the entire duration up
	// to COMMIT. `pg_advisory_xact_lock` is released automatically on
	// COMMIT or ROLLBACK — defer Rollback is correct here.
	tx, err := cfg.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: begin tx: %w", err)
	}
	// Calling rollback after a successful Commit is a no-op (pgx returns
	// ErrTxClosed), so we discard the error.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, AdvisoryLockID); err != nil {
		return nil, fmt.Errorf("bootstrap: acquire advisory lock: %w", err)
	}

	// Non-system only: archon-system is an FK anchor, not a real Archon (ADR-013 amendment 2026-07-01).
	n, err := operator.CountNonSystem(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: count operators: %w", err)
	}
	if n > 0 {
		return nil, ErrAlreadyInitialized
	}

	op := &operator.Operator{
		AID:         cfg.ArchonAID,
		DisplayName: displayName,
		AuthMethod:  operator.AuthMethodJWT,
		CreatedVia:  operator.CreatedViaBootstrap,
		// CreatedByAID = nil (first bootstrap Archon, ADR-013/014).
		// The bootstrap invariant (exactly one) is enforced by the
		// `operators_first_archon_idx` index on created_via='bootstrap'
		// (migration 085).
		// CreatedAt zero → DEFAULT NOW() in the DB.
	}
	if err := operator.Insert(ctx, tx, op); err != nil {
		return nil, fmt.Errorf("bootstrap: insert operator: %w", err)
	}

	// Fix for BUG-1 (ADR-028(c)): the membership row (cluster-admin, <aid>)
	// is written to rbac_role_operators in the SAME advisory-lock
	// transaction as the operator INSERT. The cluster-admin role already
	// exists from seed migration 027 (E1). Without this row the enforcer
	// (which resolves membership from the DB) would find no role for the
	// first Archon — the JWT claim `roles` is NOT authoritative for
	// membership. granted_by_aid = NULL — bootstrap membership has no
	// initiator.
	if err := rbac.GrantOperator(ctx, tx, BootstrapRoleClusterAdmin, cfg.ArchonAID, nil); err != nil {
		return nil, fmt.Errorf("bootstrap: grant cluster-admin membership: %w", err)
	}

	token, err := issuer.Issue(
		cfg.ArchonAID,
		[]string{BootstrapRoleClusterAdmin},
		cfg.TTLBootstrap,
		true, // bootstrapInitial
	)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: issue jwt: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap: commit tx: %w", err)
	}

	// The audit event is written after COMMIT. ArchonAID on the event is
	// empty (NULL in the DB), per ADR-014(e): the first Archon is the
	// subject itself, while `archon_aid` is the initiator; bootstrap is
	// initiated by "nobody" (keeper_internal).
	correlationID := audit.NewULID()
	ev := &audit.Event{
		AuditID:       audit.NewULID(),
		EventType:     audit.EventOperatorCreated,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: correlationID,
		Payload: map[string]any{
			"bootstrap_initial": true,
			"aid":               cfg.ArchonAID,
			"display_name":      displayName,
			"auth_method":       string(operator.AuthMethodJWT),
		},
	}
	if err := cfg.AuditWriter.Write(ctx, ev); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuditWriteFailed, err)
	}

	credPath := cfg.CredentialOutput
	if credPath == "" {
		// Defensive guard (belt-and-suspenders): the AID is embedded into
		// the file name bootstrap-<aid>.token. The new charset (ADR-014
		// amendment) already excludes `/`/`\`, but before it lands in a
		// path we re-check ValidAID plus an explicit path-traversal
		// filter. Insert/audit are already committed — we return the
		// token in Result for recovery (the caller will print it to
		// stderr).
		if !operator.ValidAID(cfg.ArchonAID) || !safePathComponent(cfg.ArchonAID) {
			return &Result{
				AuditID:       ev.AuditID,
				CorrelationID: correlationID,
				Token:         token,
			}, fmt.Errorf("%w: ArchonAID %q unsafe for credential path", ErrTokenFileWriteFailed, cfg.ArchonAID)
		}
		credPath = defaultCredentialPath(cfg.ArchonAID)
	}
	if err := ensureCredentialDir(credPath); err != nil {
		// Directory not created — the operator's source of truth is in
		// the DB, the audit is written, but the file was not saved.
		// Trigger the recovery path: return the token in Result +
		// ErrTokenFileWriteFailed.
		return &Result{
			CredentialPath: credPath,
			AuditID:        ev.AuditID,
			CorrelationID:  correlationID,
			Token:          token,
		}, fmt.Errorf("%w: %w", ErrTokenFileWriteFailed, err)
	}
	if err := writeTokenFile(credPath, token); err != nil {
		// See ErrTokenFileWriteFailed: insert + audit are already
		// committed, the file is lost. Return the token in Result so
		// the caller can print it to stderr with a rotation warning.
		return &Result{
			CredentialPath: credPath,
			AuditID:        ev.AuditID,
			CorrelationID:  correlationID,
			Token:          token,
		}, fmt.Errorf("%w: %w", ErrTokenFileWriteFailed, err)
	}

	return &Result{
		CredentialPath: credPath,
		AuditID:        ev.AuditID,
		CorrelationID:  correlationID,
	}, nil
}

func validateConfig(cfg Config) error {
	if !operator.ValidAID(cfg.ArchonAID) {
		return fmt.Errorf("bootstrap: invalid ArchonAID %q (must match %s)", cfg.ArchonAID, operator.AIDPattern)
	}
	if cfg.TTLBootstrap <= 0 {
		return fmt.Errorf("bootstrap: TTLBootstrap must be positive, got %s", cfg.TTLBootstrap)
	}
	if cfg.Pool == nil {
		return errors.New("bootstrap: Pool is nil")
	}
	if cfg.VaultClient == nil {
		return errors.New("bootstrap: VaultClient is nil")
	}
	if cfg.IssuerFactory == nil {
		return errors.New("bootstrap: IssuerFactory is nil")
	}
	if cfg.AuditWriter == nil {
		return errors.New("bootstrap: AuditWriter is nil")
	}
	if cfg.SigningKeyRef == "" {
		return errors.New("bootstrap: SigningKeyRef is empty (auth.jwt.signing_key_ref)")
	}
	return nil
}

// extractSigningKey extracts the `signing_key` field from the Vault KV
// payload and base64-decodes it. Behavior:
//
//   - `string` value, valid base64 → []byte;
//   - `string` value, NOT base64 → used as raw bytes (fallback so the
//     dev scenario `vault kv put ... signing_key=raw-32-bytes` also
//     works);
//   - `[]byte` value → used as-is;
//   - missing or empty → [ErrSigningKeyMissing].
//
// The minimum length (>= 32 bytes for HS256) is already validated by
// jwt.NewIssuer.
func extractSigningKey(kv map[string]any) ([]byte, error) {
	raw, ok := kv[vaultSigningKeyField]
	if !ok {
		return nil, ErrSigningKeyMissing
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil, ErrSigningKeyMissing
		}
		if decoded, err := base64.StdEncoding.DecodeString(v); err == nil {
			return decoded, nil
		}
		return []byte(v), nil
	case []byte:
		if len(v) == 0 {
			return nil, ErrSigningKeyMissing
		}
		return v, nil
	default:
		return nil, fmt.Errorf("bootstrap: signing_key has unsupported type %T (want string or []byte)", raw)
	}
}

// writeTokenFile creates/overwrites the path file with 0400 permissions
// and writes token + a trailing `\n` (for compatibility with tools like
// `cat | jwt decode` that expect line-terminated input).
//
// If the file already exists, it is removed first — it cannot be opened
// O_WRONLY after a previous write (mode 0400 = read-only owner).
// os.Remove ignores ErrNotExist; on permission-denied (e.g. a /tmp/ file
// owned by another user) it returns a clear error message.
func writeTokenFile(path, token string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, credentialFileMode)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // writeTokenFile's error comes from Sync/Write below

	// `O_CREATE` applies mode only on creation; with a stale umask the
	// resulting permissions could end up as 0400 & ~umask. An explicit
	// Chmod keeps the 0400 invariant regardless of the process umask.
	if err := f.Chmod(credentialFileMode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

// safePathComponent is the last barrier before embedding an AID into a
// file name: it rejects path separators and `..`. Duplicates the
// guarantees of ValidAID (the new charset already excludes
// `/`/`\`/leading `.`), but does not rely on it in case the AID format
// is extended later. Returns false for an empty string.
func safePathComponent(s string) bool {
	if s == "" || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == '\\' {
			return false
		}
	}
	return true
}

// defaultCredentialPath returns the default path for the JWT file.
//
// Priority (review M0.5c: moving away from a predictable world-readable
// `/tmp`):
//  1. `os.UserCacheDir()` → `<cache>/keeper/bootstrap-<aid>.token`
//     (Linux = `~/.cache/keeper/...`, macOS = `~/Library/Caches/keeper/...`).
//  2. Fallback `/var/lib/keeper/bootstrap-<aid>.token` — for a systemd
//     service without `HOME` (User cache unavailable).
//
// The parent directory is created by [ensureCredentialDir] with
// `mode 0700` if it doesn't already exist. The AID is part of the file
// name so that a repeat init with a different AID doesn't silently
// overwrite the old file.
func defaultCredentialPath(aid string) string {
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "keeper", "bootstrap-"+aid+".token")
	}
	return filepath.Join("/var/lib/keeper", "bootstrap-"+aid+".token")
}

// ensureCredentialDir creates the parent directory of `path` with
// `mode 0700` if it doesn't already exist. It does not chmod an existing
// directory (the operator may have a custom mount / permissions). A
// non-directory at that path is an error.
func ensureCredentialDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("credential dir %q exists but is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("stat %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	return nil
}
