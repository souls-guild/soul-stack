package operator

// The owner of a machine operator (ADR-014 amendment NIM-1057): one person or
// one Synod, who issues and revokes the operator's tokens without
// operator.issue-token. Besides [Insert], which writes the owner a create
// already validated, this file is the only writer of operators.owner_aid /
// operators.owner_synod, and the only place that decides who owns what.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

var (
	// ErrOwnerNotAllowed — the operator is not a machine operator
	// ([Operator.IsMachine]), and only those have an owner.
	ErrOwnerNotAllowed = errors.New("operator: only an operator with auth_method jwt created through the Operator API has an owner")
	// ErrInvalidOwner — the owner named is not one an operator may have; the
	// wrapped message says why.
	ErrInvalidOwner = errors.New("operator: invalid owner")
	// ErrOwnerLacksPermissions — the caller owns the operator but does not hold
	// every right the operator holds, so it may not mint the operator's token.
	ErrOwnerLacksPermissions = errors.New("operator: the owner does not hold every permission of the operator it owns")
	// ErrOwnedJWTFromPersonal — the request that asks for an owned operator's
	// token is made with a personal token (or a jti no row of the caller
	// accounts for), as [ErrPersonalJWTFromPersonal] refuses for /v1/me/jwts.
	ErrOwnedJWTFromPersonal = errors.New("operator: a personal token cannot issue tokens of an operator you own; sign in or use another token")
)

// Owner — the owner of a machine operator: exactly one of AID and Synod, or
// neither for "no owner".
type Owner struct {
	AID   string
	Synod string
}

// IsZero — no owner.
func (o Owner) IsZero() bool { return o.AID == "" && o.Synod == "" }

// OwnerOf reads op's owner.
func OwnerOf(op *Operator) Owner {
	var o Owner
	if op.OwnerAID != nil {
		o.AID = *op.OwnerAID
	}
	if op.OwnerSynod != nil {
		o.Synod = *op.OwnerSynod
	}
	return o
}

// columns — the owner as the two nullable columns.
func (o Owner) columns() (aid, synod *string) {
	if o.AID != "" {
		v := o.AID
		aid = &v
	}
	if o.Synod != "" {
		v := o.Synod
		synod = &v
	}
	return aid, synod
}

const (
	selectOwnerCandidateSQL = `SELECT auth_method, revoked_at IS NOT NULL FROM operators WHERE aid = $1 FOR KEY SHARE`
	selectOwnerSynodSQL     = `SELECT name FROM synods WHERE name = $1 FOR KEY SHARE`
)

// personMethods — the auth methods of an Archon that signs in itself, the only
// kind that owns: directly ([validateOwner]) or as a member of the owning Synod
// ([personSQL]). A machine operator in that Synod owns nothing through it, or
// one machine operator could mint another's tokens.
var personMethods = []AuthMethod{AuthMethodLocal, AuthMethodLDAP, AuthMethodOIDC}

func isPerson(m AuthMethod) bool {
	for _, p := range personMethods {
		if m == p {
			return true
		}
	}
	return false
}

// personSQL — [personMethods] as an SQL list.
var personSQL = func() string {
	quoted := make([]string, len(personMethods))
	for i, m := range personMethods {
		quoted[i] = "'" + string(m) + "'"
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}()

// validateOwner checks that owner may own the operator aid: one field set, an
// active person ([personMethods]) other than aid itself, or an existing Synod.
// The owner's row is share-locked before the caller locks aid's row, so an
// operator revoke — which locks every administrator row and then its target —
// meets them in its own order. Not always: when aid and the owner are both
// administrators, the revoke's scan can take aid's row first, and Postgres
// aborts one of the two transactions (40P01, a 500 to retry); a rare collision
// of two administrative writes, left at that. A Synod delete in flight is
// waited out, so a Synod that is gone answers 422 here rather than a
// foreign-key failure at the write.
func validateOwner(ctx context.Context, db ExecQueryRower, aid string, owner Owner) error {
	switch {
	case owner.AID != "" && owner.Synod != "":
		return fmt.Errorf("%w: set owner_aid or owner_synod, not both", ErrInvalidOwner)
	case owner.AID != "":
		if !ValidAID(owner.AID) {
			return fmt.Errorf("%w: owner_aid must match %s", ErrInvalidOwner, AIDPattern)
		}
		if owner.AID == aid {
			return fmt.Errorf("%w: an operator cannot own itself", ErrInvalidOwner)
		}
		var (
			method  string
			revoked bool
		)
		switch err := db.QueryRow(ctx, selectOwnerCandidateSQL, owner.AID).Scan(&method, &revoked); {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: operator %q not found", ErrInvalidOwner, owner.AID)
		case err != nil:
			return fmt.Errorf("operator: read owner %q: %w", owner.AID, err)
		case revoked:
			return fmt.Errorf("%w: operator %q is revoked", ErrInvalidOwner, owner.AID)
		}
		if !isPerson(AuthMethod(method)) {
			return fmt.Errorf("%w: operator %q signs in with a token issued to it; an owner is a person who signs in (local, ldap or oidc)", ErrInvalidOwner, owner.AID)
		}
	case owner.Synod != "":
		var name string
		switch err := db.QueryRow(ctx, selectOwnerSynodSQL, owner.Synod).Scan(&name); {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: synod %q not found", ErrInvalidOwner, owner.Synod)
		case err != nil:
			return fmt.Errorf("operator: read owner synod %q: %w", owner.Synod, err)
		}
	}
	return nil
}

// SetOwnerInput — parameters for [Service.SetOwner]. A zero Owner clears it.
type SetOwnerInput struct {
	AID   string
	Owner Owner
}

// SetOwnerResult — the owner written and the one it replaced.
type SetOwnerResult struct {
	AID      string
	Owner    Owner
	Previous Owner
}

const (
	lockOperatorForOwnerSQL = `SELECT auth_method, created_via, revoked_at IS NOT NULL, owner_aid, owner_synod FROM operators WHERE aid = $1 FOR NO KEY UPDATE`
	setOwnerSQL             = `UPDATE operators SET owner_aid = $2, owner_synod = $3 WHERE aid = $1`
)

// SetOwner sets or clears the owner of a machine operator. The operator's token
// lock ([lockOperatorTokens]) is taken first, so an owner's token issue in
// flight either finishes under the old owner or starts under the new one. A
// revoked operator's owner can be cleared but not set: a Synod that owns one
// cannot be deleted until it is cleared. Returns [ErrOperatorNotFound],
// [ErrOperatorAlreadyRevoked], [ErrOwnerNotAllowed] or [ErrInvalidOwner].
func (s *Service) SetOwner(ctx context.Context, in SetOwnerInput) (SetOwnerResult, error) {
	if !ValidAID(in.AID) {
		return SetOwnerResult{}, fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return SetOwnerResult{}, fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOperatorTokens(ctx, tx, in.AID); err != nil {
		return SetOwnerResult{}, err
	}
	// The target's own refusals come before the owner's: an unknown, revoked or
	// non-machine operator answers that, whatever owner the request names. Read
	// unlocked here; its row is locked after the owner's (the lock order above).
	target, err := SelectByAID(ctx, tx, in.AID)
	if err != nil {
		return SetOwnerResult{}, err
	}
	if target.IsRevoked() && !in.Owner.IsZero() {
		return SetOwnerResult{}, ErrOperatorAlreadyRevoked
	}
	if !target.IsMachine() {
		return SetOwnerResult{}, ErrOwnerNotAllowed
	}
	if !in.Owner.IsZero() {
		if err := validateOwner(ctx, tx, in.AID, in.Owner); err != nil {
			return SetOwnerResult{}, err
		}
	}

	var (
		op      Operator
		method  string
		revoked bool
	)
	switch err := tx.QueryRow(ctx, lockOperatorForOwnerSQL, in.AID).Scan(&method, &op.CreatedVia, &revoked, &op.OwnerAID, &op.OwnerSynod); {
	case errors.Is(err, pgx.ErrNoRows):
		return SetOwnerResult{}, ErrOperatorNotFound
	case err != nil:
		return SetOwnerResult{}, fmt.Errorf("operator: lock operator: %w", err)
	case revoked && !in.Owner.IsZero():
		return SetOwnerResult{}, ErrOperatorAlreadyRevoked
	}
	op.AuthMethod = AuthMethod(method)
	if !op.IsMachine() {
		return SetOwnerResult{}, ErrOwnerNotAllowed
	}

	aid, synod := in.Owner.columns()
	if _, err := tx.Exec(ctx, setOwnerSQL, in.AID, aid, synod); err != nil {
		return SetOwnerResult{}, fmt.Errorf("operator: set owner: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SetOwnerResult{}, fmt.Errorf("operator: commit tx: %w", err)
	}
	return SetOwnerResult{AID: in.AID, Owner: in.Owner, Previous: OwnerOf(&op)}, nil
}

var ownedOperatorsWhere = `
WHERE revoked_at IS NULL
  AND (owner_aid = $1
       OR owner_synod IN (SELECT so.synod_name FROM synod_operators so JOIN operators m ON m.aid = so.aid
                          WHERE so.aid = $1 AND m.auth_method IN ` + personSQL + `))`

// ListOwned returns a page of the active operators callerAID owns, directly or
// through a Synod it is a member of (as a person, [personSQL]), by AID.
func (s *Service) ListOwned(ctx context.Context, callerAID string, offset, limit int) ([]*Operator, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`+ownedOperatorsWhere, callerAID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("operator: count owned: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+operatorColumns+` FROM operators`+ownedOperatorsWhere+`
ORDER BY aid LIMIT $2 OFFSET $3`, callerAID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("operator: list owned: %w", err)
	}
	defer rows.Close()
	out := make([]*Operator, 0, limit)
	for rows.Next() {
		op, err := scanOperator(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("operator: list owned scan: %w", err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("operator: list owned iter: %w", err)
	}
	return out, total, nil
}

// isSynodOwnerSQL reads the membership unlocked: a member removed while its
// issue is in flight may still mint that one token, as it could have a moment
// earlier. Locking the row would put this path into the self-lockout probe's
// lock order on the administrators' rows, for a window of one statement.
var isSynodOwnerSQL = `SELECT EXISTS (SELECT 1 FROM synod_operators so JOIN operators m ON m.aid = so.aid
WHERE so.synod_name = $1 AND so.aid = $2 AND m.auth_method IN ` + personSQL + `)`

// ownedOperator reads aid for callerAID: [ErrOperatorNotFound] both when there
// is no such operator and when the caller does not own it, so the owner routes
// tell a stranger nothing about operators it does not own.
func ownedOperator(ctx context.Context, db ExecQueryRower, callerAID, aid string) (*Operator, error) {
	if !ValidAID(aid) {
		return nil, fmt.Errorf("operator: invalid AID %q (must match %s)", aid, AIDPattern)
	}
	op, err := SelectByAID(ctx, db, aid)
	if err != nil {
		return nil, err
	}
	switch owner := OwnerOf(op); {
	case owner.AID != "":
		if owner.AID != callerAID {
			return nil, ErrOperatorNotFound
		}
	case owner.Synod != "":
		var member bool
		if err := db.QueryRow(ctx, isSynodOwnerSQL, owner.Synod, callerAID).Scan(&member); err != nil {
			return nil, fmt.Errorf("operator: read synod membership: %w", err)
		}
		if !member {
			return nil, ErrOperatorNotFound
		}
	default:
		return nil, ErrOperatorNotFound
	}
	return op, nil
}

// IssueOwnedJWTInput — parameters for [Service.IssueOwnedJWT]. CallerAID owns
// AID; CallerJWTID is the `jti` of the token the request is made with.
type IssueOwnedJWTInput struct {
	AID         string
	CallerAID   string
	CallerJWTID string
	// TTL — 0 → `auth.jwt.ttl_default`; otherwise within [MinJWTTTL, `auth.jwt.ttl_max`].
	TTL    time.Duration
	Label  string
	Client ClientMeta
}

// IssueOwnedJWT mints a token (kind `issued`) of an operator for its owner.
// In the transaction that mints: the operator's token lock, then ownership,
// then the token the request is made with ([ErrOwnedJWTFromPersonal],
// [ErrCallerJWTRevoked]), then the coverage of the operator's rights by the
// caller's ([ErrOwnerLacksPermissions]) — owning an operator never lets its
// owner act with a right it does not hold.
func (s *Service) IssueOwnedJWT(ctx context.Context, in IssueOwnedJWTInput) (*IssueTokenResult, error) {
	ttl, err := s.resolveTTL(in.TTL)
	if err != nil {
		return nil, err
	}
	if !ValidJWTLabel(in.Label) {
		return nil, ErrInvalidJWTLabel
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOperatorTokens(ctx, tx, in.AID); err != nil {
		return nil, err
	}
	op, err := ownedOperator(ctx, tx, in.CallerAID, in.AID)
	if err != nil {
		return nil, err
	}
	if op.IsRevoked() {
		return nil, ErrOperatorAlreadyRevoked
	}
	if _, _, err := checkCallerToken(ctx, tx, in.CallerAID, in.CallerJWTID); err != nil {
		if errors.Is(err, ErrPersonalJWTFromPersonal) {
			return nil, ErrOwnedJWTFromPersonal
		}
		return nil, err
	}
	if err := rbac.AssertCovers(ctx, tx, in.CallerAID, in.AID); err != nil {
		if errors.Is(err, rbac.ErrPermissionNotHeld) {
			return nil, fmt.Errorf("%w: %w", ErrOwnerLacksPermissions, err)
		}
		return nil, err
	}

	minted, err := MintJWT(ctx, tx, s.issuer, MintInput{
		AID:          in.AID,
		Roles:        s.rbac.RolesOf(in.AID),
		TTL:          ttl,
		Kind:         JWTKindIssued,
		Label:        in.Label,
		CreatedByAID: in.CallerAID,
		Client:       in.Client,
	})
	if err != nil {
		return nil, fmt.Errorf("operator: issue owned JWT failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operator: commit tx: %w", err)
	}
	return &IssueTokenResult{
		AID:       in.AID,
		JWT:       minted.JWT,
		JWTID:     minted.JWTID,
		TTL:       ttl,
		ExpiresAt: minted.ExpiresAt,
	}, nil
}

// ListOwnedJWTs is [Service.ListJWTs] for an operator callerAID owns.
func (s *Service) ListOwnedJWTs(ctx context.Context, callerAID, aid string, f JWTFilter, offset, limit int) ([]JWTRecord, int, error) {
	if _, err := ownedOperator(ctx, s.pool, callerAID, aid); err != nil {
		return nil, 0, err
	}
	return ListJWTs(ctx, s.pool, aid, f, offset, limit)
}

// RevokeOwnedJWT is [Service.RevokeJWT] for an operator callerAID owns. No
// coverage check: revoking only takes access away.
func (s *Service) RevokeOwnedJWT(ctx context.Context, callerAID, aid, jwtID string) (JWTRecord, error) {
	if !ValidJWTID(jwtID) {
		return JWTRecord{}, ErrJWTNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return JWTRecord{}, fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOperatorTokens(ctx, tx, aid); err != nil {
		return JWTRecord{}, err
	}
	if _, err := ownedOperator(ctx, tx, callerAID, aid); err != nil {
		return JWTRecord{}, err
	}
	rec, err := RevokeJWT(ctx, tx, aid, jwtID, callerAID)
	if err != nil {
		return JWTRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return JWTRecord{}, fmt.Errorf("operator: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return rec, nil
}
