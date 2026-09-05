// The `user` object of the mongo plugin: createUser/dropUser against live mongod
// ENTIRELY through go-mongo-driver. MongoDB users live in admin.system.users
// (imperative), NOT in a config file (unlike redis users.acl), so this is a verb
// pair, not file rendering.
//
// `present` and `absent` are two ACTIONS (address level 3, obj_user.go), where they
// used to be one state reading `params.state`. The two paths were never symmetric —
// only `present` may fall back to the localhost-exception below — so the split
// costs nothing and makes the asymmetry visible in the address.
//
// LOCALHOST-EXCEPTION BOOTSTRAP (mongo mechanics, analogous to redis default_admin):
// mongod with security.authorization: enabled allows connection WITHOUT auth ONLY
// through loopback (localhost) and ONLY while admin DB has no users. The first
// admin (default_admin) is created this way: auth connection is not possible yet
// (no user exists), so plugin falls back to no-auth localhost connection. As soon
// as the first user is created, exception closes and further connections use auth.
//
// Fallback mechanics (inside plugin, not in render: render passes addr+username+
// password, plugin decides auth path from actual live state, parallel with redis
// plugin deciding by INFO/CONFIG GET):
//  1. present + username/password set -> try connection WITH AUTH;
//  2. connection/usersInfo fails with auth error (Unauthorized/AuthenticationFailed)
//     -> expected for FIRST admin (admin DB empty, no auth yet) -> fallback to
//     NO-AUTH connection (localhost-exception works if addr is loopback);
//  3. createUser of first admin succeeds over no-auth localhost connection.
//
// For non-first users (admin already exists), auth connection succeeds -> no fallback.
//
// Idempotency: usersInfo(name) before operation. present + user exists -> no-op
// (changed=false; password/roles change is day-2 update, outside PILOT).
// present + absent -> createUser (changed=true). absent + exists -> dropUser
// (changed=true). absent + absent -> no-op.
//
// SECURITY CRITICAL (ADR-010): params.password NEVER reaches events/errors.
// Password goes ONLY into createUser document (pwd) and connection credentials;
// errors are sanitized by redactError.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/protobuf/types/known/structpb"
)

// mongoRole is one user role ({role, db}). Empty db -> role in user's own DB
// (createUser runs in database context, role without db inherits it).
type mongoRole struct {
	role string
	db   string
}

// userDatabase is the DB the user lives in — the login/roles context of
// createUser/dropUser. admin is mongo's home for system accounts and the default
// both actions share.
func userDatabase(f map[string]*structpb.Value) string {
	if db := stringOrEmpty(f["database"]); db != "" {
		return db
	}
	return "admin"
}

// applyUserPresent is createUser with localhost-exception bootstrap. It opens the
// connection ITSELF (with auth -> fallback no-auth for the first admin), so it does
// not go through the shared connect path in [object.Apply].
func (m *MongoModule) applyUserPresent(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	f := params.GetFields()
	name := stringOrEmpty(f["name"])
	database := userDatabase(f)
	// pwd of the CREATED user is separate from connection password (admin). If
	// user_password is set, use it; otherwise fallback to password (bootstrap case
	// where admin creates ITSELF with the same password as connection). Separation
	// is needed for operator users: connect with admin password, createUser with
	// new user's password.
	newUserPwd := stringOrEmpty(f["user_password"])
	if newUserPwd == "" {
		newUserPwd = stringOrEmpty(f["password"])
	}

	cfg, err := parseConnConfig(params)
	if err != nil {
		return sendFailure(stream, err.Error())
	}

	// Connection with localhost-exception fallback: this path allows the no-auth
	// bootstrap of the first admin.
	conn, usedLocalhost, err := m.openUserConn(ctx, cfg, true)
	if err != nil {
		return sendFailure(stream, "connect: "+redactError(err, cfg.password, cfg.tls.keyPEM))
	}
	defer func() { _ = conn.Close(ctx) }()

	exists, err := userExists(ctx, conn, database, name)
	if err != nil {
		return sendFailure(stream, "usersInfo: "+redactError(err, newUserPwd, cfg.password))
	}
	if exists {
		// User already exists - no-op (password/roles change is day-2 update, outside PILOT).
		return sendOutcome(stream, false, fmt.Sprintf("user %q already present", name), map[string]any{
			"present":         true,
			"changed":         false,
			"used_localhost":  usedLocalhost,
			"bootstrap_admin": usedLocalhost,
		})
	}

	roles, err := parseRoles(f["roles"], database)
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	if err := createUser(ctx, conn, database, name, newUserPwd, roles); err != nil {
		return sendFailure(stream, "createUser: "+redactError(err, newUserPwd, cfg.password))
	}
	return sendOutcome(stream, true, fmt.Sprintf("user %q created", name), map[string]any{
		"present":         true,
		"changed":         true,
		"used_localhost":  usedLocalhost,
		"bootstrap_admin": usedLocalhost,
	})
}

// applyUserAbsent is dropUser. It opens the connection itself for the same reason
// present does, but with the bootstrap fallback DISABLED: removing a user requires
// privileges, so an auth failure here is a failure and not a first-admin case.
//
// It redacts ONE secret where the present half redacts two, and that is the split
// rather than an omission: `user_password` is the password of the user being CREATED,
// this action does not read it, and since it is not declared on `absent` (obj_user.go)
// a task carrying it is refused as module.unknown_param before Apply is reached. There
// is no second value here to keep out of an error string.
func (m *MongoModule) applyUserAbsent(ctx context.Context, stream eventStream, params *structpb.Struct) error {
	f := params.GetFields()
	name := stringOrEmpty(f["name"])
	database := userDatabase(f)

	cfg, err := parseConnConfig(params)
	if err != nil {
		return sendFailure(stream, err.Error())
	}

	conn, _, err := m.openUserConn(ctx, cfg, false)
	if err != nil {
		return sendFailure(stream, "connect: "+redactError(err, cfg.password, cfg.tls.keyPEM))
	}
	defer func() { _ = conn.Close(ctx) }()

	exists, err := userExists(ctx, conn, database, name)
	if err != nil {
		return sendFailure(stream, "usersInfo: "+redactError(err, cfg.password))
	}
	if !exists {
		return sendOutcome(stream, false, fmt.Sprintf("user %q already absent", name), map[string]any{
			"present": false,
			"changed": false,
		})
	}
	if err := dropUser(ctx, conn, database, name); err != nil {
		return sendFailure(stream, "dropUser: "+redactError(err, cfg.password))
	}
	return sendOutcome(stream, true, fmt.Sprintf("user %q dropped", name), map[string]any{
		"present": false,
		"changed": true,
	})
}

// openUserConn opens connection for user-state with localhost-exception fallback.
// Returns (conn, usedLocalhost, err): usedLocalhost=true when no-auth bootstrap
// path fired (first admin). Algorithm:
//   - auth set -> try connection WITH AUTH + cheap usersInfo ping (checks that
//     auth actually works, not just TCP connection);
//   - allowBootstrap AND auth connection fails with auth error -> fallback to
//     NO-AUTH connection (localhost-exception works on loopback + empty admin DB);
//   - auth not set -> immediately use no-auth connection (localhost-exception expected).
func (m *MongoModule) openUserConn(ctx context.Context, cfg connConfig, allowBootstrap bool) (mongoConn, bool, error) {
	// No credentials -> straight to no-auth (operator relies on localhost-exception).
	if cfg.username == "" && cfg.password == "" {
		conn, err := m.openConn(ctx, connConfig{addr: cfg.addr, authDB: cfg.authDB, tls: cfg.tls})
		return conn, true, err
	}

	authConn, authErr := m.openConn(ctx, cfg)
	if authErr == nil {
		// TCP connection opened, but authorization enabled can reject auth lazily
		// (on first command). Cheap probe usersInfo confirms auth actually works;
		// auth error -> fallback (bootstrap case).
		if _, probeErr := authConn.RunCommand(ctx, cfg.authDB, bson.D{{Key: "usersInfo", Value: 1}}); probeErr == nil {
			return authConn, false, nil
		} else if allowBootstrap && isAuthError(probeErr) {
			_ = authConn.Close(ctx)
			noAuth, err := m.openConn(ctx, connConfig{addr: cfg.addr, authDB: cfg.authDB, tls: cfg.tls})
			return noAuth, true, err
		} else {
			// Not an auth error (or bootstrap disabled) - connection is valid, return as is
			// (subsequent operation will return the real error).
			return authConn, false, nil
		}
	}

	// TCP/auth connection did not open. If this is an auth error and bootstrap is
	// allowed, try no-auth localhost path.
	if allowBootstrap && isAuthError(authErr) {
		noAuth, err := m.openConn(ctx, connConfig{addr: cfg.addr, authDB: cfg.authDB, tls: cfg.tls})
		return noAuth, true, err
	}
	return nil, false, authErr
}

// isAuthError recognizes mongo authorization error (for localhost-exception
// fallback). mongo returns codeName Unauthorized (13) / AuthenticationFailed (18)
// on auth attempt against empty admin DB or with wrong credentials. Check typed
// mongo.CommandError by code, with text fallback (for non-command connection paths).
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		// 13 Unauthorized, 18 AuthenticationFailed.
		if cmdErr.Code == 13 || cmdErr.Code == 18 {
			return true
		}
		name := cmdErr.Name
		if name == "Unauthorized" || name == "AuthenticationFailed" {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "requires authentication") ||
		strings.Contains(msg, "not authorized")
}

// userExists checks user presence via usersInfo. Returns true when response has
// non-empty users array.
func userExists(ctx context.Context, conn mongoConn, db, name string) (bool, error) {
	raw, err := conn.RunCommand(ctx, db, bson.D{{Key: "usersInfo", Value: name}})
	if err != nil {
		return false, err
	}
	users, lookupErr := raw.LookupErr("users")
	if lookupErr != nil {
		return false, nil
	}
	arr, ok := users.ArrayOK()
	if !ok {
		return false, nil
	}
	vals, err := arr.Values()
	if err != nil {
		return false, nil
	}
	return len(vals) > 0, nil
}

// createUser executes createUser command with roles and password. pwd goes into
// bson document (mongo createUser contract); it does not reach events/logs.
func createUser(ctx context.Context, conn mongoConn, db, name, pwd string, roles []mongoRole) error {
	cmd := bson.D{
		{Key: "createUser", Value: name},
		{Key: "pwd", Value: pwd},
		{Key: "roles", Value: rolesToBSON(roles)},
	}
	_, err := conn.RunCommand(ctx, db, cmd)
	return err
}

// dropUser executes dropUser command.
func dropUser(ctx context.Context, conn mongoConn, db, name string) error {
	_, err := conn.RunCommand(ctx, db, bson.D{{Key: "dropUser", Value: name}})
	return err
}

// rolesToBSON converts roles into bson array of createUser contract: each role is
// {role, db}. Role without db inherits user's DB (createUser context), but mongo
// requires explicit db in role document -> substitute user DB when db is empty.
func rolesToBSON(roles []mongoRole) bson.A {
	arr := bson.A{}
	for _, r := range roles {
		arr = append(arr, bson.D{
			{Key: "role", Value: r.role},
			{Key: "db", Value: r.db},
		})
	}
	return arr
}

// parseRoles parses params.roles: array of {role, db}. Empty db inherits user's DB
// (userDB). Empty array on present -> error (user without roles is meaningless).
func parseRoles(v *structpb.Value, userDB string) ([]mongoRole, error) {
	items := listField(v)
	if len(items) == 0 {
		return nil, errors.New("params.roles: must be a non-empty list of {role, db} for present")
	}
	out := make([]mongoRole, 0, len(items))
	for _, it := range items {
		rf := structField(it)
		if rf == nil {
			return nil, fmt.Errorf("params.roles: each item must be an object {role, db}")
		}
		role := stringOrEmpty(rf["role"])
		if strings.TrimSpace(role) == "" {
			return nil, fmt.Errorf("params.roles: each item requires a non-empty role")
		}
		db := stringOrEmpty(rf["db"])
		if db == "" {
			db = userDB
		}
		out = append(out, mongoRole{role: role, db: db})
	}
	return out, nil
}

// validateUserPresent performs static checks for user.present: addr + name are
// required. roles is checked in Apply, where the live user is known — an existing
// user is a no-op and needs none.
func validateUserPresent(f map[string]*structpb.Value) []string {
	return validateUserSubject(f)
}

// validateUserAbsent performs static checks for user.absent: addr + name.
func validateUserAbsent(f map[string]*structpb.Value) []string {
	return validateUserSubject(f)
}

// validateUserSubject is what both actions require: somewhere to connect to, and
// the user this step is about.
func validateUserSubject(f map[string]*structpb.Value) []string {
	var errs []string
	errs = append(errs, validateAddr(f)...)
	errs = append(errs, requireString(f, "name")...)
	return errs
}
