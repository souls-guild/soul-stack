// The `role` object — one user-defined MongoDB role, reconciled through
// createRole / updateRole / dropRole against a live mongod.
//
// This is the object where an honest structural diff is actually possible, and it
// is the reason the object exists rather than being folded into `user`. `rolesInfo`
// with showPrivileges returns the whole grant — the privileges and the inherited
// roles — as a structure, so a role that already carries what was declared is a
// real no-op (changed=false) and one that drifted is a real update, both decided by
// comparison rather than by the plugin remembering what it did last time.
//
// The comparison is done on a CANONICAL form ([mongoPrivilege.key]): actions sorted
// and de-duplicated, resources reduced to one spelling, the whole list sorted. Mongo
// returns privileges in an order of its own and may return an action list in another
// — comparing the raw documents would report a change on every apply, and a step
// that always says changed=true is one an operator learns to ignore.
//
// `updateRole` REPLACES the grant rather than adding to it, which is what makes
// `present` a converge and not an accumulation: whatever a role picked up out of
// band goes away, and the declared grant is what remains.
package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/protobuf/types/known/structpb"
)

// The role names mongod ships. createRole refuses them, so declaring one is refused
// HERE too rather than mid-run (NIM-786) — and refused again in Apply from the live
// `isBuiltin` flag, which is the answer that cannot drift as the server adds names.
//
// The split is by SCOPE, and it matters in the over-refusing direction. The first
// set exists in every database; the second exists only in `admin`, so `root` in
// `appdb` is a perfectly legal user-defined name that mongod would create — and
// refusing it here would refuse input Apply accepts, which is the mirror of the
// NIM-786 defect and just as much a defect.
var builtinRolesEveryDB = map[string]bool{
	"read": true, "readWrite": true, "dbAdmin": true, "dbOwner": true, "userAdmin": true,
}

var builtinRolesAdminOnly = map[string]bool{
	"clusterAdmin": true, "clusterManager": true, "clusterMonitor": true, "hostManager": true,
	"backup": true, "restore": true,
	"readAnyDatabase": true, "readWriteAnyDatabase": true,
	"userAdminAnyDatabase": true, "dbAdminAnyDatabase": true,
	"root": true, "__system": true,
}

// isBuiltinRole reports whether mongod already ships this name IN THIS DATABASE.
func isBuiltinRole(name, db string) bool {
	if builtinRolesEveryDB[name] {
		return true
	}
	return db == adminDB && builtinRolesAdminOnly[name]
}

// privResource is the thing a privilege is granted ON. Mongo spells it three ways
// and they are mutually exclusive: a database (optionally narrowed to a
// collection), the cluster itself, or every resource there is.
type privResource struct {
	db          string
	collection  string
	cluster     bool
	anyResource bool
}

// key is the canonical spelling of a resource, used both to compare and to report.
//
// The two names are QUOTED rather than concatenated with separators. A database
// name may contain a comma on Unix and a collection name very nearly anything, so
// `db=a` + `collection=b,collection=` and `db=a,collection=b` + `collection=`
// would render the same string — two different grants comparing equal, which is
// precisely the collision this file's canonical-form argument claims cannot happen.
func (r privResource) key() string {
	switch {
	case r.anyResource:
		return "anyResource"
	case r.cluster:
		return "cluster"
	default:
		return "db=" + strconv.Quote(r.db) + ",collection=" + strconv.Quote(r.collection)
	}
}

// doc renders the resource as mongod's createRole contract expects it.
func (r privResource) doc() bson.D {
	switch {
	case r.anyResource:
		return bson.D{{Key: "anyResource", Value: true}}
	case r.cluster:
		return bson.D{{Key: "cluster", Value: true}}
	default:
		return bson.D{{Key: "db", Value: r.db}, {Key: "collection", Value: r.collection}}
	}
}

// mongoPrivilege is one {resource, actions} grant. actions are kept sorted and
// de-duplicated from the moment they are parsed or read, so every comparison below
// is a string comparison and no caller has to remember to normalize.
type mongoPrivilege struct {
	resource privResource
	actions  []string
}

func (p mongoPrivilege) key() string {
	acts := make([]string, 0, len(p.actions))
	for _, a := range p.actions {
		acts = append(acts, strconv.Quote(a))
	}
	return p.resource.key() + "|" + strings.Join(acts, ",")
}

func (p mongoPrivilege) doc() bson.D {
	acts := make(bson.A, 0, len(p.actions))
	for _, a := range p.actions {
		acts = append(acts, a)
	}
	return bson.D{{Key: "resource", Value: p.resource.doc()}, {Key: "actions", Value: acts}}
}

// grant is everything a role confers: its own privileges plus the roles it
// inherits. Comparing two grants is comparing two sorted key lists.
type grant struct {
	privileges []mongoPrivilege
	roles      []mongoRole
}

// sameAs reports whether two grants confer the same thing. Both sides are
// canonical by construction, so this is the whole of the diff.
func (g grant) sameAs(other grant) bool {
	if len(g.privileges) != len(other.privileges) || len(g.roles) != len(other.roles) {
		return false
	}
	for i := range g.privileges {
		if g.privileges[i].key() != other.privileges[i].key() {
			return false
		}
	}
	for i := range g.roles {
		if g.roles[i].role != other.roles[i].role || g.roles[i].db != other.roles[i].db {
			return false
		}
	}
	return true
}

// canonical sorts a grant into the one order two of them can be compared in.
func (g grant) canonical() grant {
	sort.Slice(g.privileges, func(i, j int) bool { return g.privileges[i].key() < g.privileges[j].key() })
	sort.Slice(g.roles, func(i, j int) bool {
		if g.roles[i].db != g.roles[j].db {
			return g.roles[i].db < g.roles[j].db
		}
		return g.roles[i].role < g.roles[j].role
	})
	return g
}

// roleDatabase is the DB the role lives in — its namespace, since a role name is
// only unique within one. admin is the home of cluster-wide roles and the default.
func roleDatabase(f map[string]*structpb.Value) string {
	if db := stringOrEmpty(f["database"]); db != "" {
		return db
	}
	return adminDB
}

// --- parsing the declared grant ---

// parseGrant reads params.privileges and params.roles into a canonical grant.
//
// A grant that confers NOTHING is refused, here and in Validate both, for the
// reason `user.present` refuses a user with no roles: a role that grants nothing is
// not a state anyone means to declare, and accepting it would make a typo in the
// key name look like success.
func parseGrant(f map[string]*structpb.Value, roleDB string) (grant, error) {
	privs, err := parsePrivileges(f["privileges"], roleDB)
	if err != nil {
		return grant{}, err
	}
	inherited, err := parseRoleRefs(f["roles"], roleDB, "params.roles")
	if err != nil {
		return grant{}, err
	}
	if len(privs) == 0 && len(inherited) == 0 {
		return grant{}, fmt.Errorf(
			"params.privileges and params.roles are both empty: a role that confers nothing is not a state to declare")
	}
	return grant{privileges: privs, roles: inherited}.canonical(), nil
}

// parsePrivileges reads the declared privilege list. Each entry names ONE resource
// and the actions granted on it.
func parsePrivileges(v *structpb.Value, roleDB string) ([]mongoPrivilege, error) {
	items := listField(v)
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]mongoPrivilege, 0, len(items))
	seen := make(map[string]int, len(items))
	for i, it := range items {
		addr := fmt.Sprintf("params.privileges[%d]", i)
		spec := structField(it)
		if spec == nil {
			return nil, fmt.Errorf("%s: must be an object {resource, actions}", addr)
		}
		res, err := parsePrivResource(spec, addr, roleDB)
		if err != nil {
			return nil, err
		}
		// ★ ONE ENTRY PER RESOURCE. mongod MERGES two grants on the same resource
		// into one before storing them, so `[{events, [find]}, {events, [insert]}]`
		// comes back from rolesInfo as a single `{events, [find, insert]}` — two
		// declared against one live, never equal, `updateRole` on every apply
		// forever. Refusing the duplicate is the same rule [parseIndexKeys] applies
		// to a field named twice in one key.
		if first, dup := seen[res.key()]; dup {
			return nil, fmt.Errorf(
				"%s: resource %s is already granted by params.privileges[%d] — mongod MERGES two grants on one "+
					"resource into one, so a duplicate can never compare equal to what it reads back; "+
					"put the actions of a resource in a single entry", addr, res.key(), first)
		}
		seen[res.key()] = i

		actions, err := parseActions(spec, addr)
		if err != nil {
			return nil, err
		}
		out = append(out, mongoPrivilege{resource: res, actions: actions})
	}
	return out, nil
}

// parsePrivResource reads one resource spec. The three spellings are mutually
// exclusive and naming two of them is refused rather than resolved by precedence:
// `cluster` and `anyResource` grant very different things, and picking one for the
// author would be picking how much access they meant to give.
func parsePrivResource(spec map[string]*structpb.Value, addr, roleDB string) (privResource, error) {
	res, err := mapField(spec, "resource", addr+".resource")
	if err != nil {
		return privResource{}, err
	}
	if len(res) == 0 {
		return privResource{}, fmt.Errorf("%s.resource: must be a map — {db, collection}, {cluster: true} or {any_resource: true}", addr)
	}

	cluster, err := boolField(res, "cluster", addr+".resource.cluster", false)
	if err != nil {
		return privResource{}, err
	}
	anyRes, err := boolField(res, "any_resource", addr+".resource.any_resource", false)
	if err != nil {
		return privResource{}, err
	}
	db, err := stringField(res, "db", addr+".resource.db", "")
	if err != nil {
		return privResource{}, err
	}
	coll, err := stringField(res, "collection", addr+".resource.collection", "")
	if err != nil {
		return privResource{}, err
	}

	named := 0
	for _, on := range []bool{cluster, anyRes, db != "" || coll != ""} {
		if on {
			named++
		}
	}
	switch {
	case named == 0:
		return privResource{}, fmt.Errorf("%s.resource: names nothing — set db (and optionally collection), cluster: true, or any_resource: true", addr)
	case named > 1:
		return privResource{}, fmt.Errorf("%s.resource: db/collection, cluster and any_resource are mutually exclusive — name exactly one", addr)
	case cluster:
		return privResource{cluster: true}, nil
	case anyRes:
		return privResource{anyResource: true}, nil
	}

	// A resource that names only a collection is scoped to the role's own DB —
	// the same inheritance a role reference has, and mongod stores it expanded.
	if db == "" {
		db = roleDB
	}
	return privResource{db: db, collection: coll}, nil
}

// parseActions reads the action list of one privilege, sorted and de-duplicated so
// the comparison downstream is exact.
func parseActions(spec map[string]*structpb.Value, addr string) ([]string, error) {
	items, err := listFieldOf(spec, "actions", addr+".actions")
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%s.actions: must be a non-empty list of action names (a privilege granting no action is not one)", addr)
	}
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for i, it := range items {
		s, ok := stringValue(it)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%s.actions[%d]: must be a non-empty action name", addr, i)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// parseRoleRefs reads a list of {role, db} references. Unlike [parseRoles] (the
// user object's, which refuses an empty list because a user without roles is
// meaningless) an empty list is legal here: a role may confer privileges alone.
func parseRoleRefs(v *structpb.Value, defaultDB, addr string) ([]mongoRole, error) {
	items := listField(v)
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]mongoRole, 0, len(items))
	seen := make(map[string]int, len(items))
	for i, it := range items {
		rf := structField(it)
		if rf == nil {
			return nil, fmt.Errorf("%s[%d]: must be an object {role, db}", addr, i)
		}
		name, err := stringField(rf, "role", fmt.Sprintf("%s[%d].role", addr, i), "")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%s[%d].role: must be a non-empty role name", addr, i)
		}
		db, err := stringField(rf, "db", fmt.Sprintf("%s[%d].db", addr, i), "")
		if err != nil {
			return nil, err
		}
		if db == "" {
			db = defaultDB
		}
		// The same rule the two sibling parsers apply — a field named twice in one
		// index key, a resource granted twice — for the same reason: the server
		// normalizes the list it stores, so a duplicate can never compare equal to
		// what comes back and `updateRole` would run on every apply forever.
		ref := strconv.Quote(name) + "@" + strconv.Quote(db)
		if first, dup := seen[ref]; dup {
			return nil, fmt.Errorf("%s[%d]: role %q in %q is already inherited by %s[%d]", addr, i, name, db, addr, first)
		}
		seen[ref] = i
		out = append(out, mongoRole{role: name, db: db})
	}
	return out, nil
}

// --- reading the live grant ---

// liveRole is what rolesInfo says about the role: whether it exists, whether it is
// one of mongod's own, and what it currently confers.
type liveRole struct {
	exists    bool
	isBuiltin bool
	grant     grant
}

// readRole asks rolesInfo for the role WITH its privileges. A role that is not
// there comes back as an empty array, not as an error — that is the idempotency
// probe, and it is the same shape [userExists] uses.
func readRole(ctx context.Context, conn mongoConn, db, name string) (liveRole, error) {
	raw, err := conn.RunCommand(ctx, db, bson.D{
		{Key: "rolesInfo", Value: name},
		{Key: "showPrivileges", Value: true},
	})
	if err != nil {
		return liveRole{}, fmt.Errorf("rolesInfo: %w", err)
	}
	// A reply this cannot READ is an error, not "the role is not there". Decoding
	// it as absence makes `role.absent` report a cheerful no-op — `present: false`
	// in Output — about a role that still grants everything it did, and a scenario
	// gating teardown on that field walks straight past it. An EMPTY array is the
	// genuine "not found"; anything unparseable is a failure.
	val, lookupErr := raw.LookupErr("roles")
	if lookupErr != nil {
		return liveRole{}, fmt.Errorf("rolesInfo: reply carries no roles array")
	}
	arr, ok := val.ArrayOK()
	if !ok {
		return liveRole{}, fmt.Errorf("rolesInfo: roles is not an array")
	}
	vals, err := arr.Values()
	if err != nil {
		return liveRole{}, fmt.Errorf("rolesInfo: unreadable roles array: %w", err)
	}
	if len(vals) == 0 {
		return liveRole{}, nil
	}
	doc, ok := vals[0].DocumentOK()
	if !ok {
		return liveRole{}, fmt.Errorf("rolesInfo: a role entry is not a document")
	}

	out := liveRole{exists: true}
	if b, err := doc.LookupErr("isBuiltin"); err == nil {
		out.isBuiltin, _ = b.BooleanOK()
	}
	out.grant = grant{
		privileges: livePrivileges(doc),
		roles:      liveRoleRefs(doc),
	}.canonical()
	return out, nil
}

// livePrivileges reads the privileges rolesInfo returned into the same canonical
// form the declared side is parsed into.
func livePrivileges(doc bson.Raw) []mongoPrivilege {
	val, err := doc.LookupErr("privileges")
	if err != nil {
		return nil
	}
	arr, ok := val.ArrayOK()
	if !ok {
		return nil
	}
	vals, err := arr.Values()
	if err != nil {
		return nil
	}
	out := make([]mongoPrivilege, 0, len(vals))
	for _, v := range vals {
		pd, ok := v.DocumentOK()
		if !ok {
			continue
		}
		p := mongoPrivilege{}
		if rv, err := pd.LookupErr("resource"); err == nil {
			if rd, ok := rv.DocumentOK(); ok {
				p.resource = liveResource(rd)
			}
		}
		if av, err := pd.LookupErr("actions"); err == nil {
			p.actions = liveStringArray(av)
			sort.Strings(p.actions)
		}
		out = append(out, p)
	}
	return out
}

// liveResource reads one resource document back into [privResource].
func liveResource(doc bson.Raw) privResource {
	r := privResource{}
	if v, err := doc.LookupErr("anyResource"); err == nil {
		r.anyResource, _ = v.BooleanOK()
	}
	if v, err := doc.LookupErr("cluster"); err == nil {
		r.cluster, _ = v.BooleanOK()
	}
	if v, err := doc.LookupErr("db"); err == nil {
		r.db, _ = v.StringValueOK()
	}
	if v, err := doc.LookupErr("collection"); err == nil {
		r.collection, _ = v.StringValueOK()
	}
	return r
}

// liveRoleRefs reads the inherited-roles array.
func liveRoleRefs(doc bson.Raw) []mongoRole {
	val, err := doc.LookupErr("roles")
	if err != nil {
		return nil
	}
	arr, ok := val.ArrayOK()
	if !ok {
		return nil
	}
	vals, err := arr.Values()
	if err != nil {
		return nil
	}
	out := make([]mongoRole, 0, len(vals))
	for _, v := range vals {
		rd, ok := v.DocumentOK()
		if !ok {
			continue
		}
		ref := mongoRole{}
		if rv, err := rd.LookupErr("role"); err == nil {
			ref.role, _ = rv.StringValueOK()
		}
		if dv, err := rd.LookupErr("db"); err == nil {
			ref.db, _ = dv.StringValueOK()
		}
		out = append(out, ref)
	}
	return out
}

// liveStringArray reads a bson array of strings, skipping anything that is not one.
func liveStringArray(v bson.RawValue) []string {
	arr, ok := v.ArrayOK()
	if !ok {
		return nil
	}
	vals, err := arr.Values()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(vals))
	for _, e := range vals {
		if s, ok := e.StringValueOK(); ok {
			out = append(out, s)
		}
	}
	return out
}

// grantArgs renders a grant as the two arguments createRole and updateRole share.
func grantArgs(g grant) (bson.A, bson.A) {
	privs := make(bson.A, 0, len(g.privileges))
	for _, p := range g.privileges {
		privs = append(privs, p.doc())
	}
	return privs, rolesToBSON(g.roles)
}

// --- Apply ---

// applyRolePresent reconciles the role to the declared grant: absent -> createRole,
// drifted -> updateRole, identical -> no-op. The decision comes from comparing what
// rolesInfo returned with what params declare, so a repeat apply on a converged
// role reports changed=false because it IS unchanged, not because the plugin
// remembers having run.
func (m *MongoModule) applyRolePresent(ctx context.Context, stream eventStream, conn mongoConn, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)
	name := stringOrEmpty(f["name"])
	db := roleDatabase(f)

	want, err := parseGrant(f, db)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}

	live, err := readRole(ctx, conn, db, name)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	// The live answer outranks the static list in validateRolePresent: mongod's own
	// set of built-in names grows, and this one cannot go stale.
	if live.isBuiltin {
		return sendFailure(stream, fmt.Sprintf(
			"role %q in %q is a BUILT-IN role: mongod does not allow redefining one, pick another name", name, db))
	}

	privs, inherited := grantArgs(want)

	if !live.exists {
		cmd := bson.D{{Key: "createRole", Value: name}, {Key: "privileges", Value: privs}, {Key: "roles", Value: inherited}}
		if _, err := conn.RunCommand(ctx, db, cmd); err != nil {
			return sendFailure(stream, "createRole: "+redactError(err, secrets...))
		}
		return sendOutcome(stream, true, fmt.Sprintf("role %q created in %q", name, db), roleOutput(db, name, want, true))
	}

	if live.grant.sameAs(want) {
		return sendOutcome(stream, false, fmt.Sprintf("role %q already grants exactly this (no-op)", name),
			roleOutput(db, name, want, true))
	}

	// updateRole REPLACES both lists, which is what makes this a converge: a
	// privilege the role picked up out of band is gone afterwards.
	cmd := bson.D{{Key: "updateRole", Value: name}, {Key: "privileges", Value: privs}, {Key: "roles", Value: inherited}}
	if _, err := conn.RunCommand(ctx, db, cmd); err != nil {
		return sendFailure(stream, "updateRole: "+redactError(err, secrets...))
	}
	return sendOutcome(stream, true, fmt.Sprintf("role %q updated in %q", name, db), roleOutput(db, name, want, true))
}

// applyRoleAbsent drops the role. Idempotent through the same rolesInfo probe: a
// role the instance does not have is a no-op.
//
// A built-in name is refused rather than attempted, for the reason `present`
// refuses it — but here it matters more: dropRole against a built-in is an attempt
// to remove part of the server's own authorization model.
func (m *MongoModule) applyRoleAbsent(ctx context.Context, stream eventStream, conn mongoConn, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)
	name := stringOrEmpty(f["name"])
	db := roleDatabase(f)

	live, err := readRole(ctx, conn, db, name)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}
	if live.isBuiltin {
		return sendFailure(stream, fmt.Sprintf(
			"role %q in %q is a BUILT-IN role and is part of mongod's own authorization model: it cannot be dropped", name, db))
	}
	if !live.exists {
		return sendOutcome(stream, false, fmt.Sprintf("role %q already absent in %q", name, db), map[string]any{
			"database": db, "name": name, "present": false,
		})
	}
	if _, err := conn.RunCommand(ctx, db, bson.D{{Key: "dropRole", Value: name}}); err != nil {
		return sendFailure(stream, "dropRole: "+redactError(err, secrets...))
	}
	return sendOutcome(stream, true, fmt.Sprintf("role %q dropped in %q", name, db), map[string]any{
		"database": db, "name": name, "present": false,
	})
}

// roleOutput is what a scenario can read back off a reconciled role. The counts are
// of the DECLARED grant, which after a successful apply is also the live one.
func roleOutput(db, name string, g grant, present bool) map[string]any {
	return map[string]any{
		"database":   db,
		"name":       name,
		"present":    present,
		"privileges": int64(len(g.privileges)),
		"roles":      int64(len(g.roles)),
	}
}

// --- Validate ---

// validateRolePresent refuses everything about a declared grant that Apply would
// refuse: the subject, the built-in names mongod ships, and every malformed
// privilege — [parseGrant] is the same function Apply calls, so the two cannot
// disagree about what a well-formed grant is (NIM-786).
func validateRolePresent(f map[string]*structpb.Value) []string {
	errs := validateRoleSubject(f)
	if _, err := parseGrant(f, roleDatabase(f)); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}

// validateRoleAbsent — the subject alone. What the role currently grants does not
// matter to a drop, and whether it exists is a live fact.
func validateRoleAbsent(f map[string]*structpb.Value) []string {
	return validateRoleSubject(f)
}

// validateRoleSubject is what both actions require: somewhere to connect to, the
// role this step is about, and a name mongod would accept as a user-defined one.
func validateRoleSubject(f map[string]*structpb.Value) []string {
	errs := validateAddr(f)
	errs = append(errs, requireString(f, "name")...)

	if name, db := stringOrEmpty(f["name"]), roleDatabase(f); isBuiltinRole(name, db) {
		errs = append(errs, fmt.Sprintf(
			"params.name: %q is a BUILT-IN mongod role in %q — this object manages user-defined roles only", name, db))
	}
	return errs
}
