package operator

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/argon2"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Local passwords (NIM-1051, migration 130). The table is read and written only
// in this file, and no operator read joins it. A hash leaves the file only
// inside [PasswordCheck], in an unexported field its String/LogValue redact.

// MaxPasswordLength — the longest password accepted, in characters. The
// bounds of the configurable minimum live with `auth.local` in shared/config.
const MaxPasswordLength = config.LocalMaxPasswordLength

var (
	// ErrLocalPasswordsDisabled — this Keeper has no `auth.local` block, so it
	// neither sets nor checks local passwords.
	ErrLocalPasswordsDisabled = errors.New("operator: local passwords are not configured (auth.local)")
	// ErrNotLocalOperator — the operator does not log in with a local password:
	// its auth_method is not `local`. A password is never given to it.
	ErrNotLocalOperator = errors.New("operator: operator does not log in with a local password")
	// ErrWeakPassword — the new password does not meet [PasswordPolicy].
	ErrWeakPassword = errors.New("operator: password does not meet the policy")
	// ErrPasswordMismatch — the password presented is not the operator's.
	ErrPasswordMismatch = errors.New("operator: password does not match")
	// ErrLocalOperatorNoToken — a local operator is created without a token, so
	// a `ttl` or `label` for one does not apply.
	ErrLocalOperatorNoToken = errors.New("operator: a local operator is created without a token; ttl and label do not apply")
)

// NormalizeLoginAID is the AID a password login form's input names: trimmed
// and lowercased, the only case an AID has. The login rate limit keys its
// per-AID counter by the same form, so "Alice" and "alice" share one lockout.
func NormalizeLoginAID(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// PasswordPolicy is what a new local password must satisfy. MinLength counts
// characters, not bytes. There are no composition rules (NIST SP 800-63B).
type PasswordPolicy struct {
	MinLength int
}

// Check returns [ErrWeakPassword] (wrapped with the reason) when password may
// not be set for aid.
func (p PasswordPolicy) Check(aid, password string) error {
	if !utf8.ValidString(password) {
		return fmt.Errorf("%w: must be valid UTF-8", ErrWeakPassword)
	}
	n := utf8.RuneCountInString(password)
	if n < p.MinLength {
		return fmt.Errorf("%w: must be at least %d characters", ErrWeakPassword, p.MinLength)
	}
	if n > MaxPasswordLength {
		return fmt.Errorf("%w: must be at most %d characters", ErrWeakPassword, MaxPasswordLength)
	}
	for _, r := range password {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: must not contain control characters", ErrWeakPassword)
		}
	}
	if strings.EqualFold(password, aid) {
		return fmt.Errorf("%w: must differ from the AID", ErrWeakPassword)
	}
	return nil
}

// argon2id parameters of a new hash: RFC 9106 §4, the second recommended
// option (64 MiB, 3 passes, 4 lanes). Verification reads them back from the
// stored string, so raising them later needs no migration.
type argonParams struct {
	time    uint32
	memory  uint32 // KiB
	threads uint8
	keyLen  uint32
	saltLen int
}

var newHashParams = argonParams{time: 3, memory: 64 * 1024, threads: 4, keyLen: 32, saltLen: 16}

// hashSlots bounds concurrent argon2 computations, and with them the memory a
// burst of logins can take: 4 × 64 MiB at the current parameters, 4 × the
// stored bound (1 GiB) for a planted row. The login rate limit is per IP and
// per AID; a spread of both would otherwise allocate without bound.
var hashSlots = make(chan struct{}, 4)

func withHashSlot(ctx context.Context, fn func()) error {
	select {
	case hashSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-hashSlots }()
	fn()
	return nil
}

// hashPassword returns the PHC-encoded argon2id hash of password with a fresh
// salt.
func hashPassword(ctx context.Context, password string) (string, error) {
	p := newHashParams
	salt := make([]byte, p.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("operator: password salt: %w", err)
	}
	var key []byte
	if err := withHashSlot(ctx, func() {
		key = argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, p.keyLen)
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Bounds on parameters read back from a stored hash: a row is database
// content, and a planted `m=` of gigabytes would turn every login into an
// allocation of that size.
const (
	maxStoredArgonMemory = 1024 * 1024 // KiB, 1 GiB
	maxStoredArgonTime   = 16
)

// verifyPassword reports whether password produces the encoded hash. A hash it
// cannot parse is an error, not a mismatch.
func verifyPassword(ctx context.Context, encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("operator: stored password hash is not argon2id")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("operator: stored password hash has unsupported version %q", parts[2])
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return false, fmt.Errorf("operator: stored password hash parameters %q: %w", parts[3], err)
	}
	if p.memory == 0 || p.memory > maxStoredArgonMemory || p.time == 0 || p.time > maxStoredArgonTime || p.threads == 0 {
		return false, fmt.Errorf("operator: stored password hash parameters %q out of range", parts[3])
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, errors.New("operator: stored password hash salt is malformed")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, errors.New("operator: stored password hash key is malformed")
	}
	var got []byte
	if err := withHashSlot(ctx, func() {
		got = argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(want)))
	}); err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// decoyHash is verified against when there is no hash to verify — an unknown
// AID, an operator of another method — so a refusal costs the same time
// whichever the reason, and the answer's latency does not say which AIDs have
// a password.
var (
	decoyOnce sync.Once
	decoyHash string
	decoyErr  error
)

func spendDecoyVerify(ctx context.Context, password string) {
	decoyOnce.Do(func() {
		seed := make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			decoyErr = err
			return
		}
		decoyHash, decoyErr = hashPassword(context.Background(), base64.RawStdEncoding.EncodeToString(seed))
	})
	if decoyErr != nil {
		return
	}
	_, _ = verifyPassword(ctx, decoyHash, password)
}

const selectPasswordLoginSQL = `
SELECT o.auth_method, o.revoked_at IS NOT NULL, p.hash
FROM operators o
LEFT JOIN operator_passwords p ON p.aid = o.aid
WHERE o.aid = $1
`

// PasswordCheck is a password that verified: the operator and the hash it
// matched. It carries the hash only inside this package — what a login mints
// and what a change writes are bound to the password that was checked, so a
// reset committed in between refuses them instead of being undone.
type PasswordCheck struct {
	aid  string
	hash string
}

// AID is the operator the password verified for.
func (c PasswordCheck) AID() string { return c.aid }

// String, GoString and LogValue keep the hash out of any %v, %#v or slog line
// a PasswordCheck reaches.
func (c PasswordCheck) String() string       { return "PasswordCheck{" + c.aid + "}" }
func (c PasswordCheck) GoString() string     { return c.String() }
func (c PasswordCheck) LogValue() slog.Value { return slog.StringValue(c.String()) }

// VerifyLocalPassword checks password against aid's stored hash.
//
// The order is the anti-takeover and anti-oracle contract (ADR-058(d), NIM-1051):
//   - an unknown AID → [ErrOperatorNotFound], an operator whose method is not
//     `local` (or that has no hash) → [ErrNotLocalOperator]; both after a decoy
//     verification, so neither answers faster than a wrong password;
//   - a wrong password → [ErrPasswordMismatch];
//   - only a matching password reveals that the operator is revoked
//     ([ErrOperatorAlreadyRevoked]).
func VerifyLocalPassword(ctx context.Context, db ExecQueryRower, aid, password string) (PasswordCheck, error) {
	var (
		method  string
		revoked bool
		hash    *string
	)
	err := db.QueryRow(ctx, selectPasswordLoginSQL, aid).Scan(&method, &revoked, &hash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		spendDecoyVerify(ctx, password)
		return PasswordCheck{}, ErrOperatorNotFound
	case err != nil:
		return PasswordCheck{}, fmt.Errorf("operator: read password: %w", err)
	}
	if AuthMethod(method) != AuthMethodLocal || hash == nil {
		spendDecoyVerify(ctx, password)
		return PasswordCheck{}, ErrNotLocalOperator
	}
	ok, err := verifyPassword(ctx, *hash, password)
	if err != nil {
		return PasswordCheck{}, err
	}
	if !ok {
		return PasswordCheck{}, ErrPasswordMismatch
	}
	if revoked {
		return PasswordCheck{}, ErrOperatorAlreadyRevoked
	}
	return PasswordCheck{aid: aid, hash: *hash}, nil
}

// TxBeginner opens the transaction a password write or a password session is
// minted in. `*pgxpool.Pool` satisfies it.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// stillCurrent re-reads the hash of check's operator under lock in tx and
// reports whether it is still the one that verified. FOR SHARE waits for a
// reset or change that holds the row, then sees what it committed.
func stillCurrent(ctx context.Context, tx pgx.Tx, check PasswordCheck, lock string) (bool, error) {
	var hash string
	err := tx.QueryRow(ctx, `SELECT hash FROM operator_passwords WHERE aid = $1 FOR `+lock, check.aid).Scan(&hash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("operator: re-read password: %w", err)
	}
	return hash == check.hash, nil
}

// MintPasswordSession mints the session a password login earned (kind
// `session`, through [MintJWT]) in one transaction with a share lock on the
// operator's password row. The verification and the mint are not atomic — the
// hash is slow on purpose — so a reset can land between them: one committed
// since the check refuses the mint ([ErrPasswordMismatch]); one still running
// waits for this transaction and then revokes the session it minted.
func MintPasswordSession(ctx context.Context, pool TxBeginner, issuer JWTIssuer, check PasswordCheck, in MintInput) (MintResult, error) {
	if check.aid == "" || in.AID != check.aid {
		return MintResult{}, errors.New("operator: mint password session: no password check for this AID")
	}
	in.Kind = JWTKindSession
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MintResult{}, fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := stillCurrent(ctx, tx, check, "SHARE")
	if err != nil {
		return MintResult{}, err
	}
	if !current {
		return MintResult{}, ErrPasswordMismatch
	}
	res, err := MintJWT(ctx, tx, issuer, in)
	if err != nil {
		return MintResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MintResult{}, fmt.Errorf("operator: commit tx: %w", err)
	}
	return res, nil
}

const upsertPasswordSQL = `
INSERT INTO operator_passwords (aid, hash, set_at, set_by_aid)
VALUES ($1, $2, NOW(), NULLIF($3, ''))
ON CONFLICT (aid) DO UPDATE
SET hash = EXCLUDED.hash, set_at = EXCLUDED.set_at, set_by_aid = EXCLUDED.set_by_aid
`

// storePasswordHash writes aid's hash. The composite foreign key of migration
// 130 refuses an operator whose method is not `local`; that refusal comes back
// as the bare [ErrNotLocalOperator] — its text reaches a 422 detail, the
// driver's must not.
func storePasswordHash(ctx context.Context, db ExecQueryRower, aid, hash, byAID string) error {
	_, err := db.Exec(ctx, upsertPasswordSQL, aid, hash, byAID)
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation && pgErr.ConstraintName == "operator_passwords_operator_fkey" {
		return ErrNotLocalOperator
	}
	return fmt.Errorf("operator: store password: %w", err)
}

// SetInitialPassword hashes and stores the password of a local operator just
// inserted in db's transaction — `keeper init --password-stdin`, which has no
// initiator (set_by_aid NULL). The caller checks the policy.
func SetInitialPassword(ctx context.Context, db ExecQueryRower, aid, password string) error {
	hash, err := hashPassword(ctx, password)
	if err != nil {
		return err
	}
	return storePasswordHash(ctx, db, aid, hash, "")
}

const revokeTokensOfKindsSQL = `
UPDATE operator_jwts
SET revoked_at = NOW(), revoked_by_aid = NULLIF($2, '')
WHERE aid = $1 AND kind = ANY($4) AND revoked_at IS NULL
  AND expires_at > NOW() - INTERVAL '5 minutes' AND jwt_id <> $3
`

// Kinds a password replacement revokes. A change by the operator itself ends
// its other web sessions; a reset by an administrator — account recovery —
// also ends the personal tokens (NIM-1052): whoever held the old password could
// have issued one to itself, and it would outlive the reset by up to
// `auth.jwt.ttl_max`. `issued` and `bootstrap` tokens were handed out by an
// administrator or `keeper init`, not earned by the password, and stay.
var (
	changeRevokesKinds = []JWTKind{JWTKindSession}
	resetRevokesKinds  = []JWTKind{JWTKindSession, JWTKindPersonal}
)

// RevokeTokensOfKinds revokes aid's tokens of the given kinds that may still verify
// somewhere, except keepJWTID when it is non-empty — the session the caller is
// using. "May still verify" is the RBAC snapshot's own window (expired less
// than five minutes ago, for the clock gap between the database and a
// verifying instance). Returns how many were revoked. The caller invalidates
// the RBAC snapshot after the write.
func RevokeTokensOfKinds(ctx context.Context, db ExecQueryRower, aid, keepJWTID, byAID string, kinds []JWTKind) (int64, error) {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	tag, err := db.Exec(ctx, revokeTokensOfKindsSQL, aid, byAID, keepJWTID, names)
	if err != nil {
		return 0, fmt.Errorf("operator: revoke tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}
