// The `role` object's implementation — CREATE / ALTER / DROP ROLE.
//
// Cassandra has ROLES, not users: one concept covers both the login a service
// authenticates as and the group it inherits privileges from, and `CREATE USER` is a
// deprecated alias for it. The object is named after the thing.
//
// ★ No schema-agreement wait here, and that is not an omission. A role is a ROW in
// `system_auth.roles`, replicated by that keyspace's replication factor like any
// other data — it is not a schema change and does not propagate through the gossip
// path keyspace and table DDL do (schema.go). Waiting for schema agreement after
// CREATE ROLE would wait for something that already held.
//
// ★ The PASSWORD OF AN EXISTING ROLE IS NEITHER READ NOR WRITTEN. Cassandra stores
// it as a salted hash, so it cannot be compared against the declared one, and a state
// that re-applied it on every run would report `changed` forever — a state that is
// never converged. So `present` sets a password only when it CREATES the role, and
// reconciles `login` and `superuser` — the two attributes it can actually read back —
// on one that already exists. Rotating a password is a day-2 operation this artifact
// does not do; `command.run` with an `ALTER ROLE` is the escape hatch, and the
// description says so rather than leaving an operator to discover it.
package main

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

const roleQuery = `SELECT can_login, is_superuser FROM system_auth.roles WHERE role = ?`

// roleState is the live role, limited to the attributes that can be read back and
// therefore reconciled.
type roleState struct {
	canLogin    bool
	isSuperuser bool
}

// readRole reads the live role. found=false means no such role, which is an answer
// and not an error.
func readRole(ctx context.Context, s cassSession, name string) (roleState, bool, error) {
	rows, err := s.Query(ctx, roleQuery, name)
	if err != nil {
		return roleState{}, false, fmt.Errorf("read system_auth.roles: %w", err)
	}
	if len(rows) == 0 {
		return roleState{}, false, nil
	}
	return roleState{
		canLogin:    rowBool(rows[0], "can_login"),
		isSuperuser: rowBool(rows[0], "is_superuser"),
	}, true, nil
}

// applyRolePresent creates a role, or reconciles the attributes of one that exists.
func (m *CassandraModule) applyRolePresent(ctx context.Context, stream eventStream, s cassSession, params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	rolePassword := stringOrEmpty(f["role_password"])

	// Everything Validate refuses about this action, refused again here: a runner
	// need not call Validate, and an asymmetry between the two phases is a different
	// contract depending on who invoked the plugin (NIM-786, in both directions).
	if errs := checkRolePresent(f); len(errs) > 0 {
		return sendFailure(stream, strings.Join(errs, "; "))
	}
	name := stringOrEmpty(f["name"])
	login := boolOrDefault(f["login"], true)
	superuser := boolOrDefault(f["superuser"], false)

	live, found, err := readRole(ctx, s, name)
	if err != nil {
		return sendFailure(stream, redactError(err, password, rolePassword))
	}

	if !found {
		// ★ The password is a QUOTED LITERAL, not a bind marker, and that is forced
		// by the driver: gocql prepares — and therefore binds — only DML, so a `?` in
		// a CREATE ROLE reaches the server with nothing bound to it and the statement
		// is a syntax error (cql.go names the driver source). Writing it as a literal
		// is the only thing that works.
		//
		// What that costs, stated plainly: the password is inside the statement text,
		// so a cluster with audit or slow-query logging enabled can record it. That is
		// inherent to Cassandra role management — cqlsh and every other driver do the
		// same — and it is bounded on this side: the statement is never put into an
		// event, a message or an error, and every error out of this action is passed
		// through redactError with the password as a secret.
		stmt := fmt.Sprintf("CREATE ROLE %s WITH LOGIN = %t AND SUPERUSER = %t", name, login, superuser)
		if rolePassword != "" {
			literal, err := quoteLiteral("params.role_password", rolePassword)
			if err != nil {
				return sendFailure(stream, err.Error())
			}
			stmt += " AND PASSWORD = " + literal
		}
		if err := s.Exec(ctx, stmt); err != nil {
			return sendFailure(stream, "CREATE ROLE: "+redactError(err, password, rolePassword))
		}
		return sendOutcome(stream, true, fmt.Sprintf("role %s created", name), roleOutput(name, login, superuser))
	}

	if live.canLogin == login && live.isSuperuser == superuser {
		return sendOutcome(stream, false, fmt.Sprintf("role %s already matches (no-op; its password is not compared)", name), roleOutput(name, login, superuser))
	}

	// The declared password is deliberately NOT applied on this path — see the
	// file comment. Reconciling only what was read keeps `changed` honest.
	stmt := fmt.Sprintf("ALTER ROLE %s WITH LOGIN = %t AND SUPERUSER = %t", name, login, superuser)
	if err := s.Exec(ctx, stmt); err != nil {
		return sendFailure(stream, "ALTER ROLE: "+redactError(err, password, rolePassword))
	}
	return sendOutcome(stream, true,
		fmt.Sprintf("role %s altered: login %t -> %t, superuser %t -> %t (its password is not managed here)",
			name, live.canLogin, login, live.isSuperuser, superuser),
		roleOutput(name, login, superuser))
}

// roleOutput is the one shape every outcome of `present` reports. It carries no
// password and no hash.
func roleOutput(name string, login, superuser bool) map[string]any {
	return map[string]any{
		"role":      name,
		"login":     login,
		"superuser": superuser,
	}
}

// applyRoleAbsent drops a role. Idempotent: one that is not there is a no-op.
func (m *CassandraModule) applyRoleAbsent(ctx context.Context, stream eventStream, s cassSession, params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])

	name := stringOrEmpty(f["name"])
	if err := checkIdentifier("params.name", name); err != nil {
		return sendFailure(stream, err.Error())
	}

	if _, found, err := readRole(ctx, s, name); err != nil {
		return sendFailure(stream, redactError(err, password))
	} else if !found {
		return sendOutcome(stream, false, fmt.Sprintf("role %s is already absent (no-op)", name), map[string]any{"role": name})
	}

	if err := s.Exec(ctx, "DROP ROLE "+name); err != nil {
		return sendFailure(stream, "DROP ROLE: "+redactError(err, password))
	}
	return sendOutcome(stream, true, fmt.Sprintf("role %s dropped", name), map[string]any{"role": name})
}

// checkRolePresent is everything `role.present` refuses about its own params, in one
// place so Validate and Apply cannot answer differently.
func checkRolePresent(f map[string]*structpb.Value) []string {
	var errs []string
	if err := checkIdentifier("params.name", stringOrEmpty(f["name"])); err != nil {
		errs = append(errs, err.Error())
	}
	// A role that cannot log in has no use for a password, and accepting the pair
	// would leave an author believing a credential exists that does not.
	if !boolOrDefault(f["login"], true) && stringOrEmpty(f["role_password"]) != "" {
		errs = append(errs, "params.role_password: set together with login=false — a role that cannot log in has no password")
	}
	if rolePassword := stringOrEmpty(f["role_password"]); rolePassword != "" {
		if _, err := quoteLiteral("params.role_password", rolePassword); err != nil {
			errs = append(errs, err.Error())
		}
	}
	return errs
}

func validateRolePresent(f map[string]*structpb.Value) []string {
	return append(validateConnect(f), checkRolePresent(f)...)
}

func validateRoleAbsent(f map[string]*structpb.Value) []string {
	errs := validateConnect(f)
	if err := checkIdentifier("params.name", stringOrEmpty(f["name"])); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}
