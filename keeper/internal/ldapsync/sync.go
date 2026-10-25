package ldapsync

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	keeperldap "github.com/souls-guild/soul-stack/keeper/internal/auth/ldap"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// dropAfterFailedRuns is "LDAP unreachable keeps the roles for one more
// interval": the first pass that cannot read an operator's groups keeps them,
// the second in a row — an interval later, since a pass is due only once the
// previous one is an interval old — drops them.
const dropAfterFailedRuns = 2

// Reason says why a reconciliation changed roles (audit payload, metric label).
type Reason string

const (
	ReasonGroups      Reason = "groups"
	ReasonNotFound    Reason = "not_found"
	ReasonUnreachable Reason = "unreachable"
)

// Result is what a pass ended with (metric label).
type Result string

const (
	// ResultIdle — no pass was due; not counted.
	ResultIdle Result = "idle"
	// ResultOK — the directory answered for every operator and every answer
	// was applied.
	ResultOK Result = "ok"
	// ResultPartial — some operators could not be read or reconciled, or a
	// reconciliation was refused to keep the admin set.
	ResultPartial Result = "partial"
	// ResultUnreachable — the directory could not be opened at all.
	ResultUnreachable Result = "unreachable"
	// ResultGuarded — the answers would have stripped every operator holding
	// group roles; nothing was changed for the operators answered.
	ResultGuarded Result = "guarded"
	// ResultError — the pass could not read its own state.
	ResultError Result = "error"
)

// Directory reads operators' groups through the service account.
type Directory interface {
	Groups(aid string) ([]string, error)
	Close() error
}

// DirectoryOpener opens one Directory per pass.
type DirectoryOpener interface {
	OpenDirectory(ctx context.Context) (Directory, error)
}

// Reconciler is the login's role reconciliation ([auth.DBMapper]).
type Reconciler interface {
	RolesForGroups(groups []string) []string
	ReconcileRoles(ctx context.Context, aid string, want []string) (auth.RoleChange, error)
	ManagedRolesOf(ctx context.Context, aid string) ([]string, error)
	ExistingRoles(ctx context.Context, names []string) (map[string]bool, error)
}

// Store is the sync's persistent state ([PGStore]).
type Store interface {
	LoadSettings(ctx context.Context) (Settings, bool, error)
	DueOperators(ctx context.Context, interval time.Duration) ([]DueOperator, error)
	Record(ctx context.Context, r Record) error
}

// Invalidator refreshes the RBAC snapshot on this instance and signals the
// others (`rbac:invalidate`), as a role mutation through the API does.
type Invalidator interface {
	Invalidate(ctx context.Context)
}

// Config wires a [Syncer].
type Config struct {
	Store     Store
	Directory DirectoryOpener
	Roles     Reconciler
	Audit     audit.Writer
	// Invalidator is called once after a pass that changed any role, so a
	// dropped role stops working at once rather than at the next TTL poll of
	// the snapshot. nil → the TTL poll alone.
	Invalidator Invalidator
	Metrics     *Metrics
	Logger      *slog.Logger
}

// Syncer runs passes. It holds no state between them: everything a pass needs
// to know about earlier ones is in the [Store], so a new leader carries on
// where the old one stopped.
type Syncer struct {
	cfg Config
	// dirty — a role changed in the pass under way; reset by Run.
	dirty bool
}

// NewSyncer validates the wiring.
func NewSyncer(cfg Config) (*Syncer, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("ldapsync: Store is required")
	case cfg.Directory == nil:
		return nil, errors.New("ldapsync: Directory is required")
	case cfg.Roles == nil:
		return nil, errors.New("ldapsync: Roles is required")
	case cfg.Audit == nil:
		return nil, errors.New("ldapsync: Audit is required")
	case cfg.Logger == nil:
		return nil, errors.New("ldapsync: Logger is required")
	}
	return &Syncer{cfg: cfg}, nil
}

type lookup struct {
	groups []string
	err    error
}

// answered — the directory said something about this operator: its groups,
// or that it holds no entry for it.
func (l lookup) answered() bool { return l.err == nil || errors.Is(l.err, keeperldap.ErrUserNotFound) }

// Run performs a pass if one is due.
func (s *Syncer) Run(ctx context.Context) Result {
	settings, _, err := s.cfg.Store.LoadSettings(ctx)
	if err != nil {
		// The sync is the protection, so a broken row must not stop it: run on
		// the default and say so loudly. PUT validates, so only a hand edit of
		// keeper_settings gets here.
		s.cfg.Logger.Error("ldapsync: settings unreadable, running on the default interval",
			slog.String("key", SettingsKey), slog.Any("error", err))
		settings = DefaultSettings()
	}
	due, err := s.cfg.Store.DueOperators(ctx, settings.Interval)
	if err != nil {
		s.cfg.Logger.Error("ldapsync: cannot read the operators due", slog.Any("error", err))
		return s.finish(ResultError)
	}
	if len(due) == 0 {
		return ResultIdle
	}

	lookups := make([]lookup, len(due))
	dir, openErr := s.cfg.Directory.OpenDirectory(ctx)
	if openErr != nil {
		s.cfg.Logger.Warn("ldapsync: directory unreachable",
			slog.Int("operators", len(due)), slog.Any("error", openErr))
		for i := range lookups {
			lookups[i].err = openErr
		}
	} else {
		for i, op := range due {
			lookups[i].groups, lookups[i].err = dir.Groups(op.AID)
		}
		_ = dir.Close()
	}

	guarded := s.strippingEveryone(ctx, due, lookups)
	if guarded {
		s.cfg.Logger.Error("ldapsync: the directory's answers would strip every operator holding group roles — reading it as a broken lookup, no roles changed",
			slog.Int("operators", len(due)),
			slog.String("remedy", "check auth.ldap.aid_attr, user_filter, group_filter and the service account's right to read group membership"))
	}

	result := ResultOK
	switch {
	case guarded:
		result = ResultGuarded
	case openErr != nil:
		result = ResultUnreachable
	}
	s.dirty = false
	for i, op := range due {
		if guarded && lookups[i].answered() {
			s.guard(ctx, op)
			continue
		}
		if !s.apply(ctx, op, lookups[i]) && result == ResultOK {
			result = ResultPartial
		}
	}
	if s.dirty && s.cfg.Invalidator != nil {
		s.cfg.Invalidator.Invalidate(ctx)
	}
	return s.finish(result)
}

// strippingEveryone reports whether the answers would take every group role
// from every operator holding one — two at least. A service account not
// allowed to read membership, or a user_filter whose widened form matches
// nothing, looks exactly like that while logins keep working, so obeying would
// strip everyone, the next logins would restore them and the next pass strip
// them again; causes a login shares (a wrong aid_attr or group_filter) are
// held the same way until someone fixes the config. Operators holding no group
// role have nothing to lose and do not count; operators the directory did not
// answer for take the unreachable path as usual.
//
// "Reconciled to nothing" is judged after roles missing from the registry are
// left out, as the reconciliation will leave them out; if that registry check
// fails, every mapped role is taken as existing. An operator whose current
// roles cannot be read is taken as a holder — so a failing read can hold a
// real departure for one more pass, which is cheaper than letting it switch
// the guard off.
func (s *Syncer) strippingEveryone(ctx context.Context, due []DueOperator, lookups []lookup) bool {
	var wanted []string
	for _, l := range lookups {
		if l.err == nil {
			wanted = append(wanted, s.cfg.Roles.RolesForGroups(l.groups)...)
		}
	}
	exists, existsErr := s.cfg.Roles.ExistingRoles(ctx, wanted)
	if existsErr != nil {
		s.cfg.Logger.Warn("ldapsync: cannot check mapped roles against the registry, judging the guard without it", slog.Any("error", existsErr))
	}

	holders, stripped := 0, 0
	for i, op := range due {
		l := lookups[i]
		if !l.answered() {
			continue
		}
		held, err := s.cfg.Roles.ManagedRolesOf(ctx, op.AID)
		if err != nil {
			s.cfg.Logger.Warn("ldapsync: cannot read an operator's roles, counting it as a holder for the guard",
				slog.String("aid", op.AID), slog.Any("error", err))
		} else if len(held) == 0 {
			continue
		}
		holders++
		if l.err != nil || !keepsAny(s.cfg.Roles.RolesForGroups(l.groups), exists, existsErr == nil) {
			stripped++
		}
	}
	return holders >= 2 && stripped == holders
}

// keepsAny reports whether want names a role the reconciliation will keep.
func keepsAny(want []string, exists map[string]bool, checked bool) bool {
	for _, r := range want {
		if !checked || exists[r] {
			return true
		}
	}
	return false
}

// guard records an operator the guard kept. failed_runs is left as it was:
// this is not an outage, so it does not count toward dropping the roles.
func (s *Syncer) guard(ctx context.Context, op DueOperator) {
	s.cfg.Metrics.outcome(OutcomeGuarded)
	s.record(ctx, Record{AID: op.AID, Outcome: OutcomeGuarded, FailedRuns: op.FailedRuns,
		Error: "the pass would have stripped every operator holding group roles"})
}

// apply acts on one lookup and reports whether the directory answered and the
// answer was applied.
func (s *Syncer) apply(ctx context.Context, op DueOperator, l lookup) bool {
	switch {
	case l.err == nil:
		return s.reconcile(ctx, op, s.cfg.Roles.RolesForGroups(l.groups), ReasonGroups, l.groups)
	case errors.Is(l.err, keeperldap.ErrUserNotFound):
		return s.reconcile(ctx, op, nil, ReasonNotFound, nil)
	}

	failed := op.FailedRuns + 1
	rec := Record{AID: op.AID, Outcome: OutcomeUnreachable, FailedRuns: failed, Error: l.err.Error()}
	if failed >= dropAfterFailedRuns {
		change, err := s.cfg.Roles.ReconcileRoles(ctx, op.AID, nil)
		switch {
		case errors.Is(err, auth.ErrReconcileWouldLockOutCluster):
			s.cfg.Metrics.lockoutRefused()
			rec.Outcome = OutcomeLockoutRefused
			rec.Error = l.err.Error() + "; " + err.Error()
		case err != nil:
			s.cfg.Logger.Error("ldapsync: dropping the group roles of an operator LDAP could not answer for failed",
				slog.String("aid", op.AID), slog.Any("error", err))
			rec.Error = l.err.Error() + "; " + err.Error()
		default:
			s.changed(ctx, op.AID, change, ReasonUnreachable, nil)
		}
	}
	s.cfg.Metrics.outcome(rec.Outcome)
	s.record(ctx, rec)
	return false
}

// reconcile applies an answer from the directory.
func (s *Syncer) reconcile(ctx context.Context, op DueOperator, want []string, reason Reason, groups []string) bool {
	outcome := OutcomeSynced
	if reason == ReasonNotFound {
		outcome = OutcomeNotFound
	}
	change, err := s.cfg.Roles.ReconcileRoles(ctx, op.AID, want)
	switch {
	case errors.Is(err, auth.ErrReconcileWouldLockOutCluster):
		// Not applied, so not an `ok` pass: the refusal wants a human, and the
		// staleness alert on the last `ok` pass is how it gets one.
		s.cfg.Metrics.lockoutRefused()
		s.cfg.Metrics.outcome(OutcomeLockoutRefused)
		s.record(ctx, Record{AID: op.AID, Outcome: OutcomeLockoutRefused, Confirmed: true, Error: err.Error()})
		return false
	case err != nil:
		s.cfg.Logger.Error("ldapsync: reconcile failed",
			slog.String("aid", op.AID), slog.String("reason", string(reason)), slog.Any("error", err))
		s.cfg.Metrics.outcome(OutcomeReconcileFailed)
		s.record(ctx, Record{AID: op.AID, Outcome: OutcomeReconcileFailed, Confirmed: true, Error: err.Error()})
		return false
	}
	s.changed(ctx, op.AID, change, reason, groups)
	rec := Record{AID: op.AID, Outcome: outcome, Confirmed: true}
	if len(change.Missing) > 0 {
		s.cfg.Logger.Warn("ldapsync: group_role_map names roles missing from the registry; skipped",
			slog.String("aid", op.AID), slog.Any("missing", change.Missing))
		rec.Error = "skipped roles missing from the registry: " + strings.Join(change.Missing, ", ")
	}
	s.cfg.Metrics.outcome(outcome)
	s.record(ctx, rec)
	return true
}

// changed audits and counts a reconciliation that did something.
func (s *Syncer) changed(ctx context.Context, aid string, change auth.RoleChange, reason Reason, groups []string) {
	if change.Empty() {
		return
	}
	s.dirty = true
	s.cfg.Metrics.roleChanges(change, reason)
	s.cfg.Logger.Info("ldapsync: roles reconciled",
		slog.String("aid", aid), slog.String("reason", string(reason)),
		slog.Any("granted", change.Granted), slog.Any("revoked", change.Revoked))
	payload := map[string]any{
		"aid":     aid,
		"reason":  string(reason),
		"granted": nonNil(change.Granted),
		"revoked": nonNil(change.Revoked),
	}
	if reason == ReasonGroups {
		payload["groups"] = nonNil(groups)
	}
	ev := &audit.Event{
		AuditID:   audit.NewULID(),
		EventType: audit.EventOperatorRolesSynced,
		Source:    audit.SourceKeeperInternal,
		Payload:   payload,
	}
	if err := s.cfg.Audit.Write(ctx, ev); err != nil {
		// The roles already changed; RBAC is the source of truth.
		s.cfg.Logger.Error("ldapsync: audit write failed (roles changed, audit lost)",
			slog.String("aid", aid), slog.Any("error", err))
	}
}

func (s *Syncer) record(ctx context.Context, r Record) {
	if err := s.cfg.Store.Record(ctx, r); err != nil {
		s.cfg.Logger.Error("ldapsync: cannot record the attempt", slog.String("aid", r.AID), slog.Any("error", err))
	}
}

func (s *Syncer) finish(r Result) Result {
	s.cfg.Metrics.run(r)
	return r
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
