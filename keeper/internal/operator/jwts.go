package operator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// JWTKind says where an accounted JWT came from (`operator_jwts.kind`,
// migration 127, ADR-014 amendment NIM-1045).
type JWTKind string

const (
	// JWTKindBootstrap — the first Archon's token from `keeper init`.
	JWTKindBootstrap JWTKind = "bootstrap"
	// JWTKindIssued — minted for an operator by `POST /v1/operators` or
	// `issue-token` (HTTP or MCP).
	JWTKindIssued JWTKind = "issued"
	// JWTKindSession — the cookie of an LDAP/OIDC login. The short Bearer the
	// cookie is exchanged for carries the same `jti` and has no row of its own,
	// so revoking the session refuses both.
	JWTKindSession JWTKind = "session"
)

// MaxJWTLabelLen bounds `operator_jwts.label` (SQL CHECK `operator_jwts_label_len`).
const MaxJWTLabelLen = 200

// maxClientMetaLen bounds the stored User-Agent and IP. A client controls both;
// the column is evidence for a human reading a list, not a log of arbitrary input.
const maxClientMetaLen = 512

var (
	// ErrInvalidJWTLabel — the label is longer than [MaxJWTLabelLen] or holds a
	// control character.
	ErrInvalidJWTLabel = fmt.Errorf("operator: jwt label must be at most %d characters without control characters", MaxJWTLabelLen)
	// ErrJWTNotFound — no `operator_jwts` row with this id belongs to this AID.
	// A row of another operator answers the same, so the revoke route does not
	// confirm that some other operator's token id exists.
	ErrJWTNotFound = errors.New("operator: jwt not found")
	// ErrJWTAlreadyRevoked — the row already carries `revoked_at`.
	ErrJWTAlreadyRevoked = errors.New("operator: jwt already revoked")
)

var jwtIDRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// ValidJWTID reports whether id has the form `operator_jwts.jwt_id` stores (a
// ULID; SQL CHECK `operator_jwts_id_format`).
func ValidJWTID(id string) bool { return jwtIDRe.MatchString(id) }

// ValidJWTLabel reports whether label may be stored: valid UTF-8, at most
// [MaxJWTLabelLen] characters, no control characters (a NUL would fail the
// insert as a 500, a newline would forge a line in the operator's list).
func ValidJWTLabel(label string) bool {
	if !utf8.ValidString(label) || utf8.RuneCountInString(label) > MaxJWTLabelLen {
		return false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// JWTRecord is one `operator_jwts` row. It never holds the token itself.
type JWTRecord struct {
	JWTID             string
	AID               string
	Kind              JWTKind
	Label             string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	CreatedByAID      *string
	CreatedIP         string
	CreatedUserAgent  string
	LastUsedAt        *time.Time
	LastUsedIP        string
	LastUsedUserAgent string
	RevokedAt         *time.Time
	RevokedByAID      *string
}

// ClientMeta is what the request that mints or uses a token said about itself.
// Both fields are untrusted and only stored for a human to read.
type ClientMeta struct {
	IP        string
	UserAgent string
}

// JWTIssuer signs a token under a given id. `*jwt.Issuer` satisfies it.
type JWTIssuer interface {
	Issue(id, aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error)
}

// MintInput — what [MintJWT] needs to account for and sign one token.
type MintInput struct {
	AID              string
	Roles            []string
	TTL              time.Duration
	Kind             JWTKind
	Label            string
	CreatedByAID     string // "" → NULL (bootstrap, a login has no initiator but the subject)
	BootstrapInitial bool
	Client           ClientMeta
}

// MintResult — the signed token and the row it is accounted under.
type MintResult struct {
	JWT       string
	JWTID     string
	ExpiresAt time.Time
}

const insertJWTSQL = `
INSERT INTO operator_jwts (jwt_id, aid, kind, label, created_at, expires_at, created_by_aid, created_ip, created_user_agent)
VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''))
`

// MintJWT writes the `operator_jwts` row and then signs the token under its id.
// The row comes first so that no token exists that a row cannot revoke: a
// signing failure leaves an unused row that expires, never the reverse.
//
// db may be a transaction (`keeper init` mints inside the transaction that
// creates the first Archon) or the pool.
func MintJWT(ctx context.Context, db ExecQueryRower, issuer JWTIssuer, in MintInput) (MintResult, error) {
	if in.TTL <= 0 {
		return MintResult{}, fmt.Errorf("operator: mint jwt: ttl must be positive, got %s", in.TTL)
	}
	switch in.Kind {
	case JWTKindBootstrap, JWTKindIssued, JWTKindSession:
	default:
		return MintResult{}, fmt.Errorf("operator: mint jwt: unknown kind %q", in.Kind)
	}
	if !ValidJWTLabel(in.Label) {
		return MintResult{}, ErrInvalidJWTLabel
	}

	id := audit.NewULID()
	now := time.Now().UTC()
	expiresAt := now.Add(in.TTL)
	if _, err := db.Exec(ctx, insertJWTSQL,
		id, in.AID, string(in.Kind), in.Label, now, expiresAt, in.CreatedByAID,
		clip(in.Client.IP), clip(in.Client.UserAgent),
	); err != nil {
		return MintResult{}, fmt.Errorf("operator: record jwt: %w", err)
	}
	token, err := issuer.Issue(id, in.AID, in.Roles, in.TTL, in.BootstrapInitial)
	if err != nil {
		return MintResult{}, fmt.Errorf("operator: sign jwt: %w", err)
	}
	return MintResult{JWT: token, JWTID: id, ExpiresAt: expiresAt}, nil
}

const listJWTsSQL = `
SELECT jwt_id, aid, kind, COALESCE(label, ''), created_at, expires_at, created_by_aid,
       COALESCE(created_ip, ''), COALESCE(created_user_agent, ''),
       last_used_at, COALESCE(last_used_ip, ''), COALESCE(last_used_user_agent, ''),
       revoked_at, revoked_by_aid
FROM operator_jwts
WHERE aid = $1
ORDER BY created_at DESC, jwt_id DESC
LIMIT $2 OFFSET $3
`

const countJWTsSQL = `SELECT COUNT(*) FROM operator_jwts WHERE aid = $1`

// ListJWTs returns a page of the AID's accounted tokens, newest first, and the
// total. Expired rows are included until the Reaper purges them: a list that
// hid them would hide what a leaked token was doing until yesterday.
func ListJWTs(ctx context.Context, db ExecQueryRower, aid string, offset, limit int) ([]JWTRecord, int, error) {
	var total int
	if err := db.QueryRow(ctx, countJWTsSQL, aid).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("operator: count jwts: %w", err)
	}
	rows, err := db.Query(ctx, listJWTsSQL, aid, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("operator: list jwts: %w", err)
	}
	defer rows.Close()
	out := make([]JWTRecord, 0)
	for rows.Next() {
		rec, err := scanJWT(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("operator: iter jwts: %w", err)
	}
	return out, total, nil
}

func scanJWT(row pgx.Row) (JWTRecord, error) {
	var (
		rec  JWTRecord
		kind string
	)
	if err := row.Scan(&rec.JWTID, &rec.AID, &kind, &rec.Label, &rec.CreatedAt, &rec.ExpiresAt,
		&rec.CreatedByAID, &rec.CreatedIP, &rec.CreatedUserAgent,
		&rec.LastUsedAt, &rec.LastUsedIP, &rec.LastUsedUserAgent,
		&rec.RevokedAt, &rec.RevokedByAID,
	); err != nil {
		return JWTRecord{}, fmt.Errorf("operator: scan jwt: %w", err)
	}
	rec.Kind = JWTKind(kind)
	return rec, nil
}

const revokeJWTSQL = `
UPDATE operator_jwts
SET revoked_at = NOW(), revoked_by_aid = $3
WHERE jwt_id = $1 AND aid = $2 AND revoked_at IS NULL
RETURNING jwt_id, aid, kind, COALESCE(label, ''), created_at, expires_at, created_by_aid,
          COALESCE(created_ip, ''), COALESCE(created_user_agent, ''),
          last_used_at, COALESCE(last_used_ip, ''), COALESCE(last_used_user_agent, ''),
          revoked_at, revoked_by_aid
`

const probeJWTSQL = `SELECT revoked_at IS NOT NULL FROM operator_jwts WHERE jwt_id = $1 AND aid = $2`

// RevokeJWT sets `revoked_at` on one token of aid. It does not invalidate the
// RBAC snapshot — the caller does that after the write ([Service.RevokeJWT]).
func RevokeJWT(ctx context.Context, db ExecQueryRower, aid, jwtID, byAID string) (JWTRecord, error) {
	rec, err := scanJWT(db.QueryRow(ctx, revokeJWTSQL, jwtID, aid, byAID))
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return JWTRecord{}, fmt.Errorf("operator: revoke jwt: %w", err)
	}
	var revoked bool
	switch perr := db.QueryRow(ctx, probeJWTSQL, jwtID, aid).Scan(&revoked); {
	case errors.Is(perr, pgx.ErrNoRows):
		return JWTRecord{}, ErrJWTNotFound
	case perr != nil:
		return JWTRecord{}, fmt.Errorf("operator: revoke jwt probe: %w", perr)
	case revoked:
		return JWTRecord{}, ErrJWTAlreadyRevoked
	}
	return JWTRecord{}, fmt.Errorf("operator: revoke jwt: 0 rows affected, but %q is active (concurrent revoke?)", jwtID)
}

const touchJWTSQL = `
UPDATE operator_jwts
SET last_used_at = $2, last_used_ip = NULLIF($3, ''), last_used_user_agent = NULLIF($4, '')
WHERE jwt_id = $1 AND (last_used_at IS NULL OR last_used_at < $2)
`

// TouchJWT records that the token was used. The WHERE keeps a delayed write
// from one Keeper instance from moving the timestamp backwards over another's.
func TouchJWT(ctx context.Context, db ExecQueryRower, jwtID string, at time.Time, client ClientMeta) error {
	if _, err := db.Exec(ctx, touchJWTSQL, jwtID, at.UTC(), clip(client.IP), clip(client.UserAgent)); err != nil {
		return fmt.Errorf("operator: touch jwt: %w", err)
	}
	return nil
}

func clip(s string) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\x00", "")
	if len(s) <= maxClientMetaLen {
		return s
	}
	cut := maxClientMetaLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
