// Package bootstrap implements `keeper init` logic (ADR-013).
//
// Init, under a PG advisory lock, checks that the operators registry is
// empty, inserts the first Archon (`created_by_aid: NULL`), issues a JWT
// (TTL `auth.jwt.ttl_bootstrap`, claim `bootstrap_initial: true`, role
// `cluster-admin`), writes an `operator.created` audit event (source
// `keeper_internal`, `archon_aid: NULL`) and hands the token over. A
// repeat call on a non-empty registry returns [ErrAlreadyInitialized].
//
// The token goes to one of three destinations, all through [Config]:
// a regular file with `mode 0400` (the default), an already-open fd
// supplied as [Config.CredentialWriter], or a stream sitting at the
// given path — a character device, a fifo, `/dev/stdout`. Only the
// first carries a permission guarantee; [Result.CredentialIsStream]
// says which one happened.
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
	"io"
	"os"
	"path/filepath"
	"syscall"
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
	//
	// When CredentialWriter is set this is not opened at all and serves
	// only as the label in [Result.CredentialPath].
	CredentialOutput string

	// CredentialWriter, when non-nil, receives the token instead of any
	// file: no path is opened, no directory is created and no mode is
	// enforced. `keeper/cmd/keeper` sets it to os.Stdout for
	// `--credential-out=-`, which in a distroless image is the only way
	// to get the token out of the container at all (NIM-420) — there is
	// no shell and no `cat` to read a file back with.
	//
	// The writer must not be buffered, or the caller must flush it:
	// bootstrap writes once and does not close what it did not open.
	CredentialWriter io.Writer
}

// Result is the return value of a successful Init. Used by the caller
// for the final completion message on stderr; the token in Result is NOT
// logged (exception: ErrTokenFileWriteFailed recovery, see the Token
// field).
type Result struct {
	// CredentialPath is where the token was actually written (after
	// fallback). With [Config.CredentialWriter] set there is no path and
	// this carries whatever label the caller put in
	// [Config.CredentialOutput].
	CredentialPath string

	// CredentialIsStream reports that the token went somewhere no
	// permission guarantee applies to — an fd handed in through
	// [Config.CredentialWriter], or a character device / fifo reached at
	// CredentialPath. When Init returned no error, false means, and only
	// means, that the token is in a regular file with `mode 0400`. On an
	// error return the token reached nothing and this field describes
	// only which destination was attempted.
	//
	// The caller uses this to warn: a token on a stream is readable by
	// whoever is on the other end of it, and if that end is a container's
	// stdout it has just entered the cluster's log pipeline.
	CredentialIsStream bool

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

	// An fd the caller already holds open (`--credential-out=-`). There
	// is no path to resolve, no directory to create and no mode to
	// enforce, so the whole file dance below is skipped.
	if cfg.CredentialWriter != nil {
		if err := writeTokenStream(cfg.CredentialWriter, token); err != nil {
			return &Result{
				CredentialPath:     credPath,
				CredentialIsStream: true,
				AuditID:            ev.AuditID,
				CorrelationID:      correlationID,
				Token:              token,
			}, fmt.Errorf("%w: %w", ErrTokenFileWriteFailed, err)
		}
		return &Result{
			CredentialPath:     credPath,
			CredentialIsStream: true,
			AuditID:            ev.AuditID,
			CorrelationID:      correlationID,
		}, nil
	}

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
	isStream, err := writeTokenFile(credPath, token)
	if err != nil {
		// See ErrTokenFileWriteFailed: insert + audit are already
		// committed, the file is lost. Return the token in Result so
		// the caller can print it to stderr with a rotation warning.
		return &Result{
			CredentialPath:     credPath,
			CredentialIsStream: isStream,
			AuditID:            ev.AuditID,
			CorrelationID:      correlationID,
			Token:              token,
		}, fmt.Errorf("%w: %w", ErrTokenFileWriteFailed, err)
	}

	return &Result{
		CredentialPath:     credPath,
		CredentialIsStream: isStream,
		AuditID:            ev.AuditID,
		CorrelationID:      correlationID,
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

// writeTokenFile writes token + a trailing `\n` to path (line-terminated
// for the benefit of `cat | jwt decode`-style pipelines), and reports
// whether the destination turned out to be a stream rather than a
// regular file.
//
// Two shapes, picked by what is at the path:
//
//   - nothing, or a regular file → [writeTokenRegularFile], mode 0400;
//   - anything else → [writeTokenInPlace], no mode at all.
//
// The test is os.Lstat and deliberately NOT os.Stat. `/dev/stdout` is a
// symlink to `/proc/self/fd/1`, which is itself a symlink to whatever fd
// 1 really is: with stdout redirected to a file, Stat follows the chain
// and answers "regular file". The regular-file branch starts by
// unlinking its target, so a Stat here would have this function delete
// the operator's `/dev/stdout` — as root it would succeed.
//
// Lstat classifies the *name*, which is all it can do; whether the name
// leads to an actual stream is settled by [writeTokenInPlace] against
// the opened fd.
func writeTokenFile(path, token string) (isStream bool, err error) {
	st, lerr := os.Lstat(path)
	switch {
	case lerr == nil && !st.Mode().IsRegular():
		return true, writeTokenInPlace(path, st.Mode(), token)
	case lerr != nil && !os.IsNotExist(lerr):
		return false, fmt.Errorf("classify credential path: %w", lerr)
	}
	return false, writeTokenRegularFile(path, token)
}

// writeTokenRegularFile creates/overwrites path with 0400 permissions.
//
// An existing file is removed first: after a previous write it is 0400,
// and its own owner cannot reopen a read-only file O_WRONLY. os.Remove
// ignores ErrNotExist; on permission-denied (e.g. a /tmp/ file owned by
// another user) it returns a clear error message.
func writeTokenRegularFile(path, token string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, credentialFileMode)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // the reported error comes from Chmod/Write/Sync below

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

// writeTokenInPlace writes the token to a destination that is already a
// stream — a character device or a fifo, reached directly or through a
// symlink; in practice `--credential-out=/dev/stdout` (NIM-420).
//
// Nothing here may alter the destination itself. No unlink (removing a
// device node is the bug this fixes, and on a symlink it would swap the
// operator's link for a plain file), no Chmod (there is no mode to
// enforce on a stream, and through a symlink it would repermission a
// file we do not own), no Sync (fsync on a pipe or character device
// fails with EINVAL, and a stream has no dirty pages anyway).
//
// Because none of those guards apply, the open has to establish what it
// is really writing to. Lstat only classified the name:
//
//   - O_NONBLOCK, so a fifo with no reader fails with ENXIO instead of
//     blocking forever. This runs after the Archon is committed, and
//     `signal.NotifyContext` has already taken SIGINT off the default
//     handler — a hang here is not even interruptible, and leaves a
//     cluster that is initialized but whose only token reached nobody.
//   - an fstat of the opened fd, admitting a character device or a fifo
//     and refusing everything else. An allowlist rather than a denylist,
//     because the type that got through the denylist was the destructive
//     one: a block device opens cleanly and takes the write at offset
//     zero, so a root `keeper init` with `--credential-out` pointing at
//     /dev/sda — a typo, or a link planted where the credential was
//     going — laid the JWT over the partition table. A directory and a
//     socket never reach here at all; the kernel answers EISDIR and
//     ENXIO at the open.
//
// The allowlist narrows the blast radius; it does not close the class,
// and should not be described as if it did. Every character device is
// admitted, and some of those are as bad as the block device: writing
// to /dev/kmsg puts the JWT in the kernel ring buffer and from there in
// the journal, /dev/mem is admitted too (both measured on this kernel),
// and merely opening and closing /dev/watchdog reboots the machine —
// the open happens before any check can run, so no fstat-based rule can
// prevent it. Refusing devices by number would be worse than the
// disease. The real boundary is that the path form trusts its target as
// well as its directory; --credential-out=- trusts neither.
//
// A regular file is refused by its own branch. Not as a symlink guard —
// there isn't one, and claiming otherwise misleads whoever reads this
// next. Following symlinks is load-bearing here (/dev/stdout is one),
// and a planted fifo takes delivery of the token whether it is reached
// through a symlink or planted at the credential path directly (both
// measured). O_NOFOLLOW would not help, because the bare fifo is the
// same attack. The path form is unusable in a directory somebody else
// can write to, full stop; that is a property of the feature, recorded
// in docs/operations/bootstrap-rbac.md, not something this branch fixes.
//
// What the branch actually protects is the guarantee the path form
// advertises. --credential-out=<path> promises mode 0400, and that can
// only be kept on a file we created ourselves — not by chmod'ing one we
// found, for the reason given at the top of this comment: through a
// symlink that repermissions a file we do not own. A regular file
// already sitting there carries whatever mode it already has, so
// writing a cluster-admin JWT into it drops the promise *silently*, and
// that adverb is the whole distinction. The rule is not "a path gets a
// path's guarantees or an error" — /dev/stdout is a path, gets no 0400,
// and is allowed. It is three-valued: the guarantee, a downgrade the
// operator is told about (the stderr warning and credential_is_stream:
// true), or an error. A stream announces itself. An existing regular
// file is the one destination whose result is indistinguishable from
// the path form working — a file on disk holding the token, exactly as
// advertised, wearing somebody else's mode.
//
// Note what the refusal is NOT about, because the plausible-sounding
// version invites a fix: it is not about leftover bytes after the
// token. A tail does survive `>>` and `1<>` (measured — the shell
// truncates for neither, and the reopened fd writes from offset zero),
// so an earlier claim here that redirection disposes of the tail held
// only for plain `>`. It changes nothing either way: O_TRUNC would not
// make such a file 0400. --credential-out=- serves the redirect,
// resolves no path at all, and — unlike a path — promises no mode,
// which is exactly why it is the honest form for one.
//
// nameMode is what os.Lstat saw at the name, and is used for one thing:
// telling the operator which kind of ENXIO they hit. It has no say in
// whether the write happens — only the fstat below does.
func writeTokenInPlace(path string, nameMode os.FileMode, token string) error {
	// The wrappers deliberately do not repeat the path: the *PathError
	// underneath already carries it, and "open X: open X: ..." tells an
	// operator less than one mention plus the reason we were opening it
	// that way.
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		// ENXIO covers two unrelated situations and only one of them is
		// fixable, so they are not given the same sentence: a fifo says
		// it when nobody is reading, and a socket says it because open(2)
		// cannot open a socket at all.
		if errors.Is(err, syscall.ENXIO) && nameMode&os.ModeNamedPipe != 0 {
			return fmt.Errorf("nothing is reading it: %w — start the reader "+
				"first, or use --credential-out=- and pipe", err)
		}
		return fmt.Errorf("not usable as a stream: %w", err)
	}
	defer f.Close() //nolint:errcheck // the reported error comes from Write below

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("classify the opened stream: %w", err)
	}
	if err := streamTypeError(path, st.Mode()); err != nil {
		return err
	}

	// O_NONBLOCK was needed only to survive the open, and usually the
	// runtime already covers the rest: the poller parks the goroutine on
	// EAGAIN, so Write waits for a full pipe to drain even though the
	// descriptor is still non-blocking (Fd() clears O_NONBLOCK only when
	// the runtime set it, not when we passed it to open). Measured on a
	// fifo, dropping this line changes nothing — which is exactly why it
	// has no test. What it covers is registration *failing*: epoll
	// refuses some devices, and then our own O_NONBLOCK reaches a raw
	// write, where a full buffer is EAGAIN rather than a wait. One fcntl
	// removes the difference between the two cases.
	if err := syscall.SetNonblock(int(f.Fd()), false); err != nil {
		return fmt.Errorf("restore blocking mode on %s: %w", path, err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// streamTypeError says why the fd [writeTokenInPlace] opened may not be
// written to, or nil if it may.
//
// It is split out because the branch it guards cannot be tested against
// the thing it exists for. Creating a device node needs CAP_MKNOD, and a
// test that pointed the writer at a real /dev/sda to prove it is refused
// would destroy a disk the first time it regressed. A pure function over
// a mode can be handed `os.ModeDevice` and asked.
func streamTypeError(path string, mode os.FileMode) error {
	switch {
	case mode&(os.ModeCharDevice|os.ModeNamedPipe) != 0:
		// The two things a token can stream into. The fifo case is
		// `--credential-out=/dev/stdout` under a pipe: reopening
		// /proc/self/fd/1 yields a second fd to the same pipe. Not
		// `--credential-out=-`, which an earlier version of this comment
		// named here and which never reaches this function at all — it
		// sets Config.CredentialWriter, and Run returns from
		// writeTokenStream well before writeTokenFile is called.
		return nil
	case mode.IsRegular():
		return fmt.Errorf("%s leads to a regular file, not a stream: "+
			"name that file directly, or use --credential-out=- and redirect", path)
	case mode&os.ModeDevice != 0:
		// Char devices matched above, so this is a block device: it opens
		// cleanly and takes the write at offset zero.
		return fmt.Errorf("%s leads to a block device, not a stream: refusing to "+
			"write a credential over it", path)
	default:
		return fmt.Errorf("%s leads to neither a stream nor a file (mode %s): "+
			"refusing to write a credential there", path, mode.Type())
	}
}

// writeTokenStream writes the token to an fd the caller already holds
// open ([Config.CredentialWriter], i.e. `--credential-out=-`).
//
// Byte-identical to what the file paths produce, trailing `\n` included:
// `keeper init --credential-out=- > archon.jwt` has to yield the same
// file as `keeper init --credential-out=archon.jwt`, modulo its mode.
func writeTokenStream(w io.Writer, token string) error {
	if _, err := io.WriteString(w, token+"\n"); err != nil {
		return fmt.Errorf("write token to stream: %w", err)
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
