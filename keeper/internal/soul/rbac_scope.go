package soul

import "context"

// PermissionChecker — the one scope-aware RBAC call the per-host gates need.
// Satisfied by *rbac.Enforcer and *rbac.Holder, and structurally identical to
// api/middleware.PermissionChecker; declared here so that the packages which
// only need to OR a permission over a host's contexts (handlers, api, conductor,
// mcp) do not each grow their own copy of the loop.
type PermissionChecker interface {
	Check(aid, resource, action string, context map[string]string) error
}

// HostContexts expands a host's scope into the per-candidate RBAC context set
// for an OR-check ([middleware.RequirePermissionMulti] over REST, the same loop
// everywhere else).
//
// `host` is in EVERY context: the SID comes from the path, the tool argument or
// the console frame, and is known whether or not the row could be read. Each
// Coven label of the host adds a context `{host, coven=<label>}` — a host may
// carry several (ADR-008), and a single flat context can hold only one value per
// dimension ([rbac.Permission.Matches]), so the OR over the set is what makes
// `on coven=<one of them>` match.
//
// No labels → the single `{host}` context, which is the honest answer in both
// cases that produce it: the host really carries no coven, or the row could not
// be read at all. A `coven=` condition then fails closed (a dimension absent
// from the context does not satisfy a condition on it), and `host=`/bare/`*`
// grants keep working — which is the pre-NIM-588 behaviour of every call, now
// confined to the case where the coven genuinely is unknown.
func HostContexts(sid string, covens []string) []map[string]string {
	if sid == "" {
		return nil
	}
	if len(covens) == 0 {
		return []map[string]string{{"host": sid}}
	}
	out := make([]map[string]string, 0, len(covens))
	for _, c := range covens {
		out = append(out, map[string]string{"host": sid, "coven": c})
	}
	return out
}

// HostContextsBySID reads the host's Coven labels through db and builds the
// context set. A nil db, a missing row or a Postgres failure all yield the
// `{host}` context alone — see [HostContexts] for why that, and not nil.
func HostContextsBySID(ctx context.Context, db ExecQueryRower, sid string) []map[string]string {
	if db == nil {
		return HostContexts(sid, nil)
	}
	covens, err := CovenBySID(ctx, db, sid)
	if err != nil {
		return HostContexts(sid, nil)
	}
	return HostContexts(sid, covens)
}

// HostContextsBySIDs is [HostContextsBySID] over a batch, in one round-trip.
// Every input SID is a key of the result; hosts whose row is missing (or all of
// them, if the read failed outright) get the `{host}` context alone, so a
// `coven=`-scoped grant fails closed per host rather than for the whole batch.
func HostContextsBySIDs(ctx context.Context, db ExecQueryRower, sids []string) map[string][]map[string]string {
	byHost := map[string][]string{}
	if db != nil {
		if got, err := CovenBySIDs(ctx, db, sids); err == nil {
			byHost = got
		}
	}
	out := make(map[string][]map[string]string, len(sids))
	for _, sid := range sids {
		out[sid] = HostContexts(sid, byHost[sid])
	}
	return out
}

// AllowAnyContext ORs one permission over a context set: nil as soon as any one
// context is granted, otherwise the LAST denial (its class — permission-denied
// vs revoked — is what the caller maps to a status).
//
// An empty set → a single attempt with a nil context: bare/`*` pass, scoped ones
// do not. Same fail-closed rule as [middleware.RequirePermissionMulti], so the
// middleware and the in-handler gates cannot drift apart on the case where the
// context could not be built.
func AllowAnyContext(check PermissionChecker, aid, resource, action string, contexts []map[string]string) error {
	if len(contexts) == 0 {
		contexts = []map[string]string{nil}
	}
	var lastErr error
	for _, c := range contexts {
		err := check.Check(aid, resource, action, c)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}
