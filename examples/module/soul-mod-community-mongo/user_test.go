package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// authError is mongo.CommandError with Unauthorized code (13), as mongod returns
// for auth attempt against EMPTY admin DB (first admin not created yet).
func authError() error {
	return mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "command usersInfo requires authentication"}
}

// dualModule builds MongoModule that distinguishes auth connection and no-auth
// connection by username presence in cfg: authConn is returned when username!="",
// noAuthConn when empty. This models localhost-exception fallback (plugin reopens
// connection WITHOUT auth on auth-fail). Returns last connection cfg in cfg field
// of each fakeConn.
func dualModule(authConn, noAuthConn *fakeConn) *MongoModule {
	return &MongoModule{
		connect: func(_ context.Context, cfg connConfig) (mongoConn, error) {
			if cfg.username != "" || cfg.password != "" {
				authConn.cfg = cfg
				return authConn, nil
			}
			noAuthConn.cfg = cfg
			return noAuthConn, nil
		},
	}
}

// TestApplyUser_LocalhostExceptionBootstrap_FirstAdmin is the key mongo test.
// admin DB is empty: auth connection opens, but usersInfo probe fails Unauthorized
// (localhost-exception has not been crossed yet). Plugin falls back to NO-AUTH
// connection and creates first admin over it (createUser). Checks:
//   - createUser called (on no-auth connection);
//   - used_localhost/bootstrap_admin = true;
//   - changed=true, present=true;
//   - password did not leak into events.
func TestApplyUser_LocalhostExceptionBootstrap_FirstAdmin(t *testing.T) {
	// auth connection: usersInfo probe fails Unauthorized (admin empty -> auth does not work).
	authConn := &fakeConn{cmdErrByName: map[string]error{"usersInfo": authError()}}
	// no-auth connection (localhost-exception): usersInfo -> 0 users (absent), createUser ok.
	noAuthConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}

	m := dualModule(authConn, noAuthConn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "default_admin",
			"database": "admin",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "root", "db": "admin"}},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("expected successful creation of first admin, got %+v", fin)
	}
	if !fin.Changed {
		t.Error("expected changed=true (first admin created)")
	}
	if !fin.GetOutput().GetFields()["used_localhost"].GetBoolValue() {
		t.Error("used_localhost=false, expected true (localhost-exception fallback fired)")
	}
	if !fin.GetOutput().GetFields()["bootstrap_admin"].GetBoolValue() {
		t.Error("bootstrap_admin=false, expected true")
	}
	// createUser executed on NO-AUTH connection (localhost-exception), not auth.
	if !hasCommand(noAuthConn.calls, "createUser") {
		t.Errorf("createUser not executed on no-auth connection, got %v", noAuthConn.calls)
	}
	// Created user name.
	if v, ok := commandValue(noAuthConn.calls, "createUser"); !ok || v != "default_admin" {
		t.Errorf("createUser name=%v, expected default_admin", v)
	}
	assertEventsNoSecret(t, stream)
	if !noAuthConn.closed {
		t.Error("no-auth connection not closed")
	}
}

// TestApplyUser_AuthWorks_NoBootstrap - admin ALREADY exists: auth connection works
// (usersInfo probe ok), fallback is not needed. Create second user via auth connection.
// used_localhost=false.
func TestApplyUser_AuthWorks_NoBootstrap(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{
		// usersInfo is called twice: probe (openUserConn) and exists-check. Both
		// return contextually meaningful response: SUCCESS (no error) matters for
		// probe; for exists, requested user is ABSENT (0). usersRaw(0) fits both:
		// probe checks only absence of error.
		"usersInfo": usersRaw(0),
	}}
	noAuthConn := &fakeConn{}

	m := dualModule(authConn, noAuthConn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("expected success, got %+v", fin)
	}
	if fin.GetOutput().GetFields()["used_localhost"].GetBoolValue() {
		t.Error("used_localhost=true, expected false (auth works, bootstrap not needed)")
	}
	// createUser on AUTH connection, no-auth not touched.
	if !hasCommand(authConn.calls, "createUser") {
		t.Errorf("createUser not executed on auth connection, got %v", authConn.calls)
	}
	if len(noAuthConn.calls) != 0 {
		t.Errorf("no-auth connection must not be used (auth works), got %v", noAuthConn.calls)
	}
}

// TestApplyUser_PresentIdempotent_NoOp - user already exists -> no-op (changed=false),
// createUser is NOT called (idempotency; password/roles change is day-2).
func TestApplyUser_PresentIdempotent_NoOp(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{
		"usersInfo": usersRaw(1), // user exists (probe ok and exists=true)
	}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("expected success, got %+v", fin)
	}
	if fin.Changed {
		t.Error("present-idempotent: user already exists -> changed=false")
	}
	if hasCommand(authConn.calls, "createUser") {
		t.Error("createUser called for existing user - idempotency violated")
	}
}

// TestApplyUser_AbsentDropsExisting - state=absent, user exists -> dropUser, changed=true.
func TestApplyUser_AbsentDropsExisting(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{
		"usersInfo": usersRaw(1),
	}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "absent",
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("expected success, got %+v", fin)
	}
	if !fin.Changed {
		t.Error("absent + user exists -> changed=true (dropUser)")
	}
	if !hasCommand(authConn.calls, "dropUser") {
		t.Errorf("expected dropUser, got %v", authConn.calls)
	}
}

// TestApplyUser_AbsentIdempotent_NoOp - state=absent, user absent -> no-op.
func TestApplyUser_AbsentIdempotent_NoOp(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{
		"usersInfo": usersRaw(0),
	}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "ghost",
			"database": "appdb",
			"state":    "absent",
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("expected success, got %+v", fin)
	}
	if fin.Changed {
		t.Error("absent-idempotent: user absent -> changed=false")
	}
	if hasCommand(authConn.calls, "dropUser") {
		t.Error("dropUser called for absent user - idempotency violated")
	}
}

// TestApplyUser_AbsentDoesNotBootstrap - state=absent + auth fails Unauthorized ->
// do NOT perform no-auth fallback (user removal is not a bootstrap case). Return error.
func TestApplyUser_AbsentDoesNotBootstrap(t *testing.T) {
	authConn := &fakeConn{cmdErrByName: map[string]error{"usersInfo": authError()}}
	noAuthConn := &fakeConn{}
	m := dualModule(authConn, noAuthConn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "absent",
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("expected failed=true (absent does not use localhost-exception fallback), got %+v", fin)
	}
	if len(noAuthConn.calls) != 0 {
		t.Errorf("no-auth connection must not be used on absent path, got %v", noAuthConn.calls)
	}
}

// TestApplyUser_NoCredentials_DirectLocalhost - credentials not set -> straight
// to no-auth (operator relies on localhost-exception). used_localhost=true.
func TestApplyUser_NoCredentials_DirectLocalhost(t *testing.T) {
	noAuthConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}
	authConn := &fakeConn{}
	m := dualModule(authConn, noAuthConn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"name":     "default_admin",
			"database": "admin",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "root", "db": "admin"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("expected success, got %+v", fin)
	}
	if !fin.GetOutput().GetFields()["used_localhost"].GetBoolValue() {
		t.Error("used_localhost=false, expected direct no-auth path without credentials")
	}
	if len(authConn.calls) != 0 {
		t.Errorf("auth connection must not be used without credentials, got %v", authConn.calls)
	}
}

// TestApplyUser_PresentRejectsEmptyRoles - present + empty roles -> failed (user
// without roles is meaningless; validation is in Apply and depends on state present).
func TestApplyUser_PresentRejectsEmptyRoles(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "norolls",
			"database": "appdb",
			"state":    "present",
		}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Failed {
		t.Fatalf("expected failed=true for present without roles, got %+v", fin)
	}
}

// TestApplyUser_CreateUserErrorDoesNotLeakPassword - createUser error text
// echoes the password and is sanitized by redactError (security invariant
// ADR-010). The password legitimately goes into pwd in the createUser document
// on the wire, but NOT into events/errors.
func TestApplyUser_CreateUserErrorDoesNotLeakPassword(t *testing.T) {
	authConn := &fakeConn{
		rawByName:    map[string]bson.Raw{"usersInfo": usersRaw(0)},
		cmdErrByName: map[string]error{"createUser": errors.New("write error for pwd " + secretPass)},
	}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("expected failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyUser_PasswordDoesNotLeakOutsideCreateUser - password goes ONLY into
// createUser pwd, and NOT into usersInfo/other commands on the wire (security,
// defense-in-depth).
func TestApplyUser_PasswordDoesNotLeakOutsideCreateUser(t *testing.T) {
	authConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
			"name":     "appuser",
			"database": "appdb",
			"state":    "present",
			"roles":    []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	if commandCarriesSecretOutsideCreateUser(authConn.calls, secretPass) {
		t.Error("password leaked into command arguments outside createUser")
	}
}

// TestApplyUser_UserPasswordSeparateFromAdminPassword - password separation:
// connect uses the admin password (password), while createUser pwd comes from
// user_password (password of the user being created). For operator users these
// are different secrets. Verify createUser.pwd == user_password,
// connection.password == admin-password, and neither leaks into events.
func TestApplyUser_UserPasswordSeparateFromAdminPassword(t *testing.T) {
	const adminPass = "admin-connect-pass-4b1c"
	const userPass = "new-user-pwd-a7f2"
	authConn := &fakeConn{rawByName: map[string]bson.Raw{"usersInfo": usersRaw(0)}}
	m := dualModule(authConn, &fakeConn{})
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "user",
		Params: mustStruct(t, map[string]any{
			"addr":          "127.0.0.1:27017",
			"username":      "default_admin",
			"password":      adminPass,
			"user_password": userPass,
			"name":          "appuser",
			"database":      "appdb",
			"state":         "present",
			"roles":         []any{map[string]any{"role": "readWrite", "db": "appdb"}},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("expected success, got %+v", fin)
	}
	// Connect with the admin password.
	if authConn.cfg.password != adminPass {
		t.Errorf("connection.password=%q, expected admin password %q", authConn.cfg.password, adminPass)
	}
	// createUser.pwd is the password of the CREATED user, not admin.
	if pwd, ok := commandField(authConn.calls, "createUser", "pwd"); !ok || pwd != userPass {
		t.Errorf("createUser.pwd=%v, expected user_password %q (password separation)", pwd, userPass)
	}
	// Neither admin password nor user password appears in events.
	for _, e := range stream.sent {
		if strings.Contains(e.String(), adminPass) || strings.Contains(e.String(), userPass) {
			t.Errorf("password leaked into event: %q", e.String())
		}
	}
}
