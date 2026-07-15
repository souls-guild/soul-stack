package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ProvisioningGate — narrow surface for checking the provisioning_allowed_methods
// policy (ADR-058 Part B). Implemented by *serviceregistry.Holder; declared as
// an interface so the auth package doesn't pull in serviceregistry and can be
// tested with a fake gate. ProvisioningMethodAllowed("ldap") answers whether
// CREATING a federated operator (auto-provision) is allowed. A nil gate is
// treated by the caller as "allow" (back-compat: policy not configured).
type ProvisioningGate interface {
	ProvisioningMethodAllowed(method string) bool
}

// Txer — narrow transaction factory (subset of pgxpool.Pool) needed for
// atomic role reconciliation. *pgxpool.Pool satisfies it automatically.
// Declared as an interface so the auth package doesn't pull pgxpool into its
// dependencies.
type Txer interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// MapperConfig — [DBMapper] dependencies.
type MapperConfig struct {
	// Method — this mapper's federated method (ADR-058(a)): operator.AuthMethodLDAP
	// or operator.AuthMethodOIDC. Determines which `auth_method`/`created_via`
	// gets written to the row on auto-provision and which method is checked
	// against ProvisioningGate. One [DBMapper] serves one method (the LDAP
	// mapper and OIDC mapper are constructed separately by the daemon with the
	// same logic). Empty value → provisioning is rejected ([ErrAuthFailed],
	// defense-in-depth: a mapper without an explicit method must not silently
	// create an operator of unknown origin).
	Method operator.AuthMethod

	// GroupRoleMap — external group → RBAC roles (config auth.{ldap,oidc}.group_role_map).
	// Role source for a federated operator (ADR-058(d), decision #2: roles from groups).
	GroupRoleMap map[string][]string

	// DB — read/write surface for the operators + rbac_role_operators registry.
	// A real pgxpool.Pool satisfies the interface.
	DB operator.ExecQueryRower

	// Tx — transaction factory for atomic role reconciliation (HIGH-1,
	// ADR-058(d)): granting new roles + scoped-revoking departed ones run in
	// ONE transaction, otherwise a failure between grant and revoke would
	// leave membership inconsistent. A real *pgxpool.Pool satisfies it
	// (BeginTx).
	//
	// nil → falls back to a non-transactional grant-only path over DB
	// (back-compat for unit tests with a fake DB lacking BeginTx): revoke
	// reconciliation is then skipped, but the existing "grant from groups"
	// guard still holds. The daemon always sets Tx (d.pool), so reconciliation
	// is atomic in production.
	Tx Txer

	// Audit — where to write `operator.provisioned` (login is written by the endpoint).
	Audit audit.Writer

	// ProvisioningGate — the provisioning_allowed_methods policy (ADR-058
	// Part B): gates ONLY the provision branch (auto-creating a new federated
	// operator). nil → gate disabled (policy not configured, back-compat). An
	// existing operator (the err==nil case in Map) does NOT go through the
	// gate — it logs in regardless of policy.
	ProvisioningGate ProvisioningGate

	// Logger — debug trace (no secrets).
	Logger *slog.Logger
}

// DBMapper maps an external identity (LDAP or OIDC) onto operators(aid) +
// roles (ADR-058(d)). Implements [Mapper]. The logic is identical for both
// methods; only cfg.Method tells them apart (written to auth_method/created_via,
// checked against ProvisioningGate).
//
// Stage 1 decisions (ADR-058):
//   - provisioning — auto-provision by groups (decision #1): the first login
//     creates the operator IF its groups intersect group_role_map; outside
//     the mapped groups — reject ([ErrNoRoleMapping]), no operator is created;
//   - role source — external groups (decision #2): for both new and existing
//     operators, roles are computed from group_role_map, not from the
//     registry. Membership is synced into rbac_role_operators (RBAC's
//     authority is the table, not the JWT claim, ADR-028(c));
//   - revoked invariant: federated login for a revoked operator is forbidden
//     ([ErrOperatorRevoked]).
//
// DBMapper writes the `operator.provisioned` audit event (the row-creation
// fact); the endpoint writes `operator.login` (one event per successful
// login) — kept separate so the login event isn't duplicated.
type DBMapper struct {
	cfg MapperConfig
	// managedRoles — union of values(group_role_map): the domain of roles this
	// federated mapper OWNS (HIGH-1, ADR-058(d)). Revoke reconciliation
	// touches ONLY roles from this set — roles granted via Synod/manually/by
	// any other path outside group_role_map are never revoked. Computed once
	// in NewMapper.
	managedRoles map[string]struct{}
}

// NewMapper constructs a DBMapper.
func NewMapper(cfg MapperConfig) *DBMapper {
	return &DBMapper{cfg: cfg, managedRoles: managedRoleSet(cfg.GroupRoleMap)}
}

// managedRoleSet collects the set of all roles mentioned in group_role_map's
// values — the domain federated reconciliation owns (HIGH-1).
func managedRoleSet(grm map[string][]string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, roles := range grm {
		for _, role := range roles {
			set[role] = struct{}{}
		}
	}
	return set
}

// Map implements [Mapper]: ext → MappedOperator or a sentinel error.
//
// AID comes from ext.AID (Authenticator derives it from cfg.AIDAttr, default
// `uid`, see ldap.Authenticator). An invalid AID → [ErrAuthFailed]
// (anti-oracle: the cause doesn't leak outward).
func (m *DBMapper) Map(ctx context.Context, ext ExternalIdentity) (MappedOperator, error) {
	if m.cfg.Method == "" {
		// Defense-in-depth: a mapper without an explicit method must not
		// silently create an operator of unknown origin. The daemon always
		// sets Method.
		return MappedOperator{}, ErrAuthFailed
	}
	aid := ext.AID
	if !operator.ValidAID(aid) {
		if m.cfg.Logger != nil {
			m.cfg.Logger.Debug("auth/mapper: derived AID failed validation",
				slog.String("aid", aid))
		}
		return MappedOperator{}, ErrAuthFailed
	}

	roles := m.rolesForGroups(ext.Groups)
	if len(roles) == 0 {
		// Outside the group mapping — reject, no operator is created (ADR-058(d)).
		return MappedOperator{}, ErrNoRoleMapping
	}

	op, err := operator.SelectByAID(ctx, m.cfg.DB, aid)
	switch {
	case err == nil:
		if op.IsRevoked() {
			return MappedOperator{}, ErrOperatorRevoked
		}
		// CRIT-1 (account takeover, ADR-058(d) revocation invariant
		// hardened): the federated path serves ONLY operators created by
		// THIS SAME federated method. If the existing operator lives under
		// a different auth_method (bootstrap/system `jwt`, mTLS, OR another
		// federated method), reject — otherwise anyone controlling the
		// external IdP could mint themselves a valid derived AID that
		// collides with a privileged operator's AID (e.g. the bootstrap
		// cluster-admin) and take over their session. ErrAuthFailed
		// (anti-oracle: a bare 401 outward — we don't reveal that the AID
		// exists under a different method). Bootstrap (`auth_method=jwt`)
		// and `archon-system`/system operators are thereby protected
		// automatically: their auth_method ∉ {ldap,oidc}, so the federated
		// mapper never accepts them.
		if op.AuthMethod != m.cfg.Method {
			if m.cfg.Logger != nil {
				m.cfg.Logger.Warn("auth/mapper: federated login rejected — AID belongs to a different auth_method",
					slog.String("aid", aid),
					slog.String("mapper_method", string(m.cfg.Method)))
			}
			return MappedOperator{}, ErrAuthFailed
		}
		// Existing active operator of the same method: roles come from
		// groups (role source = groups, decision #2). Membership is
		// reconciled (grant new + scoped-revoke departed, HIGH-1) so an
		// external group change is reflected in RBAC on the next login.
		if err := m.reconcileRoles(ctx, aid, roles); err != nil {
			return MappedOperator{}, err
		}
		return MappedOperator{AID: aid, Roles: roles, Provisioned: false}, nil

	case errors.Is(err, operator.ErrOperatorNotFound):
		// Auto-provision (decision #1): user is in a mapped group → create the operator.
		return m.provision(ctx, aid, ext, roles)

	default:
		return MappedOperator{}, fmt.Errorf("auth/mapper: select operator: %w", err)
	}
}

// provision creates a new federated operator (auth_method=cfg.Method) +
// membership + the `operator.provisioned` audit event.
//
// created_via=string(cfg.Method) (ldap|oidc), created_by_aid=NULL (ADR-058(d)):
// federated login is initiated by the external IdP, there is no initiating
// operator. NULL created_by_aid is now legal for non-bootstrap rows — the
// bootstrap invariant moved to created_via='bootstrap' (migration 085), so a
// separate reserved-AID marker is no longer needed. The source is attributed
// by created_via itself.
func (m *DBMapper) provision(ctx context.Context, aid string, ext ExternalIdentity, roles []string) (MappedOperator, error) {
	method := string(m.cfg.Method)
	// provisioning_allowed_methods policy gate (ADR-058 Part B): ONLY on
	// creation. BEFORE Insert — the operator must not appear when the method
	// is forbidden. gate==nil → skip (policy not configured, back-compat).
	if m.cfg.ProvisioningGate != nil && !m.cfg.ProvisioningGate.ProvisioningMethodAllowed(method) {
		return MappedOperator{}, ErrProvisioningDisabled
	}

	displayName := ext.Username
	if displayName == "" {
		displayName = aid
	}
	// created_via is a string in the same domain as auth_method (ldap|oidc);
	// the CreatedVia enum is a string alias, values match (ADR-058(d)).
	op := &operator.Operator{
		AID:          aid,
		DisplayName:  displayName,
		AuthMethod:   m.cfg.Method,
		CreatedByAID: nil,
		CreatedVia:   method,
		Metadata:     map[string]any{"federated_source": method},
	}
	if err := operator.Insert(ctx, m.cfg.DB, op); err != nil {
		return MappedOperator{}, fmt.Errorf("auth/mapper: provision insert: %w", err)
	}
	// Freshly created operator: no roles yet, nothing to revoke-reconcile —
	// grant-only over DB (the same transactionality isn't needed here,
	// federated provisioning is rare).
	if err := m.grantRoles(ctx, m.cfg.DB, aid, roles); err != nil {
		return MappedOperator{}, err
	}

	// `operator.provisioned` audit (no secrets: password/bind creds never end
	// up in ext, groups aren't secret).
	ev := &audit.Event{
		AuditID:   audit.NewULID(),
		EventType: audit.EventOperatorProvisioned,
		Source:    audit.SourceAPI,
		ArchonAID: aid,
		Payload: map[string]any{
			"aid":          aid,
			"auth_method":  method,
			"display_name": displayName,
			"roles":        roles,
			"groups":       ext.Groups,
		},
	}
	if err := m.cfg.Audit.Write(ctx, ev); err != nil {
		// Operator already created; audit lost. Don't fail the login
		// (operator is the source of truth), but log for manual reconciliation.
		if m.cfg.Logger != nil {
			m.cfg.Logger.Error("auth/mapper: provision audit write failed (operator created, audit lost)",
				slog.String("aid", aid), slog.Any("error", err))
		}
	}
	return MappedOperator{AID: aid, Roles: roles, Provisioned: true}, nil
}

// reconcileRoles brings the operator's DIRECT membership (rbac_role_operators)
// to the `want` set (roles from the user's current groups) — grant new +
// scoped-revoke departed (HIGH-1, implements ADR-058(d) "roles = external
// groups").
//
// Scope of revoke (CRITICAL): ONLY roles from the domain this mapper owns
// are revoked (m.managedRoles = union of values(group_role_map)). Roles
// granted via Synod / manually / any other path OUTSIDE group_role_map are
// left untouched — federated reconciliation owns only its own domain.
//
// Algorithm: revoke = (current direct membership ∩ managedRoles) \ want;
// grant = want \ current. Both mutations run in ONE transaction (Tx
// factory): a failure between grant and revoke must not leave membership
// inconsistent.
//
// Tx==nil (unit test with a fake DB lacking BeginTx) → falls back to
// grant-only over DB (back-compat): revoke is skipped, but grant from groups
// still holds. The daemon always sets Tx, so reconciliation is atomic and
// revokes in production.
func (m *DBMapper) reconcileRoles(ctx context.Context, aid string, want []string) error {
	for _, role := range want {
		if !rbac.ValidRoleName(role) {
			return fmt.Errorf("auth/mapper: invalid role name %q in group_role_map", role)
		}
	}

	if m.cfg.Tx == nil {
		// Back-compat without a transaction: only grant (revoke requires reading
		// current membership + atomicity with grant — not guaranteed without Tx).
		return m.grantRoles(ctx, m.cfg.DB, aid, want)
	}

	tx, err := m.cfg.Tx.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("auth/mapper: begin reconcile tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	current, err := rbac.DirectRolesOf(ctx, tx, aid)
	if err != nil {
		return err
	}

	wantSet := make(map[string]struct{}, len(want))
	for _, r := range want {
		wantSet[r] = struct{}{}
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, r := range current {
		currentSet[r] = struct{}{}
	}

	// Revoke: current roles in the managed domain that are no longer in want.
	for _, role := range current {
		if _, managed := m.managedRoles[role]; !managed {
			continue // outside the group_role_map domain — not our zone (Synod/manual)
		}
		if _, keep := wantSet[role]; keep {
			continue
		}
		if err := rbac.RevokeOperator(ctx, tx, role, aid); err != nil {
			// The pairing may already be gone (race with a manual revoke) — that's fine, don't fail.
			if errors.Is(err, rbac.ErrRoleOperatorNotFound) {
				continue
			}
			return fmt.Errorf("auth/mapper: reconcile revoke role %q: %w", role, err)
		}
	}

	// Grant: roles from want that aren't present yet (idempotent, but skip existing).
	for _, role := range want {
		if _, have := currentSet[role]; have {
			continue
		}
		if err := rbac.GrantOperator(ctx, tx, role, aid, nil); err != nil {
			return fmt.Errorf("auth/mapper: reconcile grant role %q: %w", role, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth/mapper: commit reconcile tx: %w", err)
	}
	return nil
}

// grantRoles idempotently grants each role over db (pool OR tx).
// granted_by_aid = nil (federated membership has no initiating operator).
// Used by the provision path (fresh operator) and reconcileRoles's Tx==nil
// fallback. An invalid role name is a configuration error.
func (m *DBMapper) grantRoles(ctx context.Context, db operator.ExecQueryRower, aid string, roles []string) error {
	for _, role := range roles {
		if !rbac.ValidRoleName(role) {
			return fmt.Errorf("auth/mapper: invalid role name %q in group_role_map", role)
		}
		if err := rbac.GrantOperator(ctx, db, role, aid, nil); err != nil {
			return fmt.Errorf("auth/mapper: grant role %q: %w", role, err)
		}
	}
	return nil
}

// rolesForGroups intersects the user's groups with group_role_map and
// collects a deduplicated, stably sorted set of roles.
func (m *DBMapper) rolesForGroups(groups []string) []string {
	if len(m.cfg.GroupRoleMap) == 0 || len(groups) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for _, g := range groups {
		for _, role := range m.cfg.GroupRoleMap[g] {
			seen[role] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for role := range seen {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

// compile-time assertion: *DBMapper implements Mapper.
var _ Mapper = (*DBMapper)(nil)
