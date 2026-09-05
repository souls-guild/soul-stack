// Guards on the `role` object. The one worth the file is the no-op: this is the
// object of the artifact that converges on a real structural diff, so a live grant
// that MEANS the same thing as the declared one must come back changed=false even
// when mongod returns it in another order — otherwise every apply reports a change
// and an operator learns to ignore the report.
package main

import (
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
)

// roleReply is a rolesInfo answer carrying one role.
func roleReply(t *testing.T, role bson.D) bson.Raw {
	t.Helper()
	return rawDoc(t, bson.D{{Key: "roles", Value: bson.A{role}}, {Key: "ok", Value: int32(1)}})
}

// noRoleReply is a rolesInfo answer for a role that is not there.
func noRoleReply(t *testing.T) bson.Raw {
	t.Helper()
	return rawDoc(t, bson.D{{Key: "roles", Value: bson.A{}}, {Key: "ok", Value: int32(1)}})
}

func roleApply(t *testing.T, m *MongoModule, state string, params map[string]any) *applyStream {
	t.Helper()
	stream := &applyStream{}
	if err := m.role().Apply(&pluginv1.ApplyRequest{State: state, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	return stream
}

// declaredGrant is the grant every test below declares, written once.
func declaredGrant() map[string]any {
	return map[string]any{
		"addr":     "127.0.0.1:27017",
		"password": secretPass,
		"name":     "app_writer",
		"database": "appdb",
		"privileges": []any{
			map[string]any{
				"resource": map[string]any{"db": "appdb", "collection": "events"},
				"actions":  []any{"insert", "find"},
			},
			map[string]any{
				"resource": map[string]any{"cluster": true},
				"actions":  []any{"replSetGetStatus"},
			},
		},
		"roles": []any{map[string]any{"role": "read", "db": "appdb"}},
	}
}

func TestRolePresent_CreatesWhenAbsent(t *testing.T) {
	conn := &fakeConn{rawByName: map[string]bson.Raw{"rolesInfo": noRoleReply(t)}}
	m := newModule(conn)

	stream := roleApply(t, m, "present", declaredGrant())

	if changed, _ := outcome(t, stream); !changed {
		t.Error("creating a role must report changed=true")
	}
	if n := countCommand(conn.calls, "createRole"); n != 1 {
		t.Errorf("createRole sent %d times, want 1", n)
	}
	if n := countCommand(conn.calls, "updateRole"); n != 0 {
		t.Errorf("updateRole sent %d times at a role that did not exist", n)
	}
	assertEventsNoSecret(t, stream)
	if commandCarriesSecretOutsideCreateUser(conn.calls, secretPass) {
		t.Error("the connection password reached a command argument")
	}
}

// TestRolePresent_NoOpWhenGrantMatches is the point of the object: the live grant
// comes back with its privileges in the OTHER order and each action list shuffled,
// and it must still read as converged. Comparing the raw documents would report a
// change here, on every single apply.
func TestRolePresent_NoOpWhenGrantMatches(t *testing.T) {
	live := bson.D{
		{Key: "role", Value: "app_writer"},
		{Key: "db", Value: "appdb"},
		{Key: "isBuiltin", Value: false},
		{Key: "privileges", Value: bson.A{
			bson.D{
				{Key: "resource", Value: bson.D{{Key: "cluster", Value: true}}},
				{Key: "actions", Value: bson.A{"replSetGetStatus"}},
			},
			bson.D{
				{Key: "resource", Value: bson.D{{Key: "db", Value: "appdb"}, {Key: "collection", Value: "events"}}},
				{Key: "actions", Value: bson.A{"find", "insert"}},
			},
		}},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}},
	}
	conn := &fakeConn{rawByName: map[string]bson.Raw{"rolesInfo": roleReply(t, live)}}
	m := newModule(conn)

	stream := roleApply(t, m, "present", declaredGrant())

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a grant that already matches must be a no-op, got %q", msg)
	}
	if n := countCommand(conn.calls, "createRole") + countCommand(conn.calls, "updateRole"); n != 0 {
		t.Errorf("a no-op wrote %d time(s)", n)
	}
}

// TestRolePresent_UpdatesOnDrift — a live grant missing an action is a real change,
// and updateRole (which REPLACES the grant) is what applies it.
func TestRolePresent_UpdatesOnDrift(t *testing.T) {
	live := bson.D{
		{Key: "role", Value: "app_writer"},
		{Key: "isBuiltin", Value: false},
		{Key: "privileges", Value: bson.A{
			bson.D{
				{Key: "resource", Value: bson.D{{Key: "db", Value: "appdb"}, {Key: "collection", Value: "events"}}},
				{Key: "actions", Value: bson.A{"find"}}, // insert was revoked out of band
			},
			bson.D{
				{Key: "resource", Value: bson.D{{Key: "cluster", Value: true}}},
				{Key: "actions", Value: bson.A{"replSetGetStatus"}},
			},
		}},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: "appdb"}}}},
	}
	conn := &fakeConn{rawByName: map[string]bson.Raw{"rolesInfo": roleReply(t, live)}}
	m := newModule(conn)

	stream := roleApply(t, m, "present", declaredGrant())

	if changed, _ := outcome(t, stream); !changed {
		t.Error("a drifted grant must report changed=true")
	}
	if n := countCommand(conn.calls, "updateRole"); n != 1 {
		t.Fatalf("updateRole sent %d times, want 1", n)
	}
	if n := countCommand(conn.calls, "createRole"); n != 0 {
		t.Errorf("createRole sent %d times at a role that exists", n)
	}
	// The command carries the WHOLE declared grant, because updateRole replaces.
	privs, ok := commandField(conn.calls, "updateRole", "privileges")
	if !ok {
		t.Fatal("updateRole carried no privileges")
	}
	if arr, _ := privs.(bson.A); len(arr) != 2 {
		t.Errorf("updateRole carried %d privileges, want the 2 declared", len(arr))
	}
}

// TestRolePresent_BuiltinIsRefusedFromLiveAnswer — the static list in Validate can
// go stale as mongod adds names; the live `isBuiltin` flag cannot. Both refuse.
func TestRolePresent_BuiltinIsRefusedFromLiveAnswer(t *testing.T) {
	live := bson.D{{Key: "role", Value: "custom_looking"}, {Key: "isBuiltin", Value: true}}
	conn := &fakeConn{rawByName: map[string]bson.Raw{"rolesInfo": roleReply(t, live)}}
	m := newModule(conn)

	params := declaredGrant()
	params["name"] = "custom_looking"
	stream := roleApply(t, m, "present", params)

	if msg := failureMessage(t, stream); !strings.Contains(msg, "BUILT-IN") {
		t.Errorf("expected a refusal naming the built-in, got %q", msg)
	}
	if n := countCommand(conn.calls, "createRole") + countCommand(conn.calls, "updateRole"); n != 0 {
		t.Errorf("a refused step still wrote %d time(s)", n)
	}
}

func TestRoleAbsent_DropsAndIsIdempotent(t *testing.T) {
	t.Run("drops an existing role", func(t *testing.T) {
		live := bson.D{{Key: "role", Value: "app_writer"}, {Key: "isBuiltin", Value: false}}
		conn := &fakeConn{rawByName: map[string]bson.Raw{"rolesInfo": roleReply(t, live)}}
		stream := roleApply(t, newModule(conn), "absent", map[string]any{
			"addr": "127.0.0.1:27017", "name": "app_writer", "database": "appdb",
		})
		if changed, _ := outcome(t, stream); !changed {
			t.Error("dropping an existing role must report changed=true")
		}
		if n := countCommand(conn.calls, "dropRole"); n != 1 {
			t.Errorf("dropRole sent %d times, want 1", n)
		}
	})

	t.Run("a role that is not there is a no-op", func(t *testing.T) {
		conn := &fakeConn{rawByName: map[string]bson.Raw{"rolesInfo": noRoleReply(t)}}
		stream := roleApply(t, newModule(conn), "absent", map[string]any{
			"addr": "127.0.0.1:27017", "name": "app_writer", "database": "appdb",
		})
		if changed, msg := outcome(t, stream); changed {
			t.Errorf("expected a no-op, got %q", msg)
		}
		if n := countCommand(conn.calls, "dropRole"); n != 0 {
			t.Errorf("a no-op sent dropRole %d times", n)
		}
	})
}

// TestValidateRole_RefusesWhatApplyWould — [parseGrant] is the same function both
// call, so a grant Validate passes is one Apply can build (NIM-786).
func TestValidateRole_RefusesWhatApplyWould(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]any
		wantErr string
	}{
		{
			"a built-in name",
			map[string]any{"addr": "127.0.0.1:27017", "name": "root",
				"privileges": []any{map[string]any{"resource": map[string]any{"cluster": true}, "actions": []any{"x"}}}},
			"BUILT-IN",
		},
		{
			"a grant that confers nothing",
			map[string]any{"addr": "127.0.0.1:27017", "name": "empty_role"},
			"confers nothing",
		},
		{
			"a resource naming two things at once",
			map[string]any{"addr": "127.0.0.1:27017", "name": "r", "privileges": []any{
				map[string]any{"resource": map[string]any{"cluster": true, "any_resource": true}, "actions": []any{"x"}}}},
			"mutually exclusive",
		},
		{
			"a resource naming nothing",
			map[string]any{"addr": "127.0.0.1:27017", "name": "r", "privileges": []any{
				map[string]any{"resource": map[string]any{}, "actions": []any{"x"}}}},
			"must be a map",
		},
		{
			"a privilege granting no action",
			map[string]any{"addr": "127.0.0.1:27017", "name": "r", "privileges": []any{
				map[string]any{"resource": map[string]any{"cluster": true}, "actions": []any{}}}},
			"non-empty list",
		},
		{
			"an inherited role with no name",
			map[string]any{"addr": "127.0.0.1:27017", "name": "r", "roles": []any{map[string]any{"db": "appdb"}}},
			"non-empty role name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateRolePresent(mustFields(t, tc.params))
			if len(errs) == 0 {
				t.Fatalf("expected a refusal mentioning %q, got none", tc.wantErr)
			}
			if !strings.Contains(strings.Join(errs, "; "), tc.wantErr) {
				t.Errorf("refusal %v does not mention %q", errs, tc.wantErr)
			}
		})
	}
}

func TestValidateRolePresent_HappyPath(t *testing.T) {
	if errs := validateRolePresent(mustFields(t, declaredGrant())); len(errs) != 0 {
		t.Errorf("a well-formed grant was refused: %v", errs)
	}
}

// TestRoleGrant_ResourceScopedToTheRoleDatabase — a resource naming only a
// collection is expanded to the role's own database, which is how mongod stores it;
// without that the comparison would never converge.
func TestRoleGrant_ResourceScopedToTheRoleDatabase(t *testing.T) {
	g, err := parseGrant(mustFields(t, map[string]any{
		"privileges": []any{map[string]any{
			"resource": map[string]any{"collection": "events"},
			"actions":  []any{"find"},
		}},
	}), "appdb")
	if err != nil {
		t.Fatalf("parseGrant: %v", err)
	}
	if got := g.privileges[0].resource.db; got != "appdb" {
		t.Errorf("resource db = %q, want the role's own database", got)
	}
}

// TestRolePresent_DuplicateResourceIsRefused — mongod MERGES two grants on one
// resource before storing them, so `[{events,[find]}, {events,[insert]}]` comes
// back from rolesInfo as a single `{events,[find,insert]}`: two declared against
// one live, never equal, updateRole on every apply forever. Refusing the duplicate
// is the same rule parseIndexKeys applies to a field named twice in one key.
func TestRolePresent_DuplicateResourceIsRefused(t *testing.T) {
	params := map[string]any{
		"addr": "127.0.0.1:27017", "name": "app_writer", "database": "appdb",
		"privileges": []any{
			map[string]any{"resource": map[string]any{"db": "appdb", "collection": "events"}, "actions": []any{"find"}},
			map[string]any{"resource": map[string]any{"db": "appdb", "collection": "events"}, "actions": []any{"insert"}},
		},
	}
	errs := validateRolePresent(mustFields(t, params))
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), "MERGES") {
		t.Fatalf("expected a refusal explaining the merge, got %v", errs)
	}

	conn := &fakeConn{rawByName: map[string]bson.Raw{"rolesInfo": noRoleReply(t)}}
	stream := roleApply(t, newModule(conn), "present", params)
	if msg := failureMessage(t, stream); !strings.Contains(msg, "MERGES") {
		t.Errorf("Apply must refuse it too, got %q", msg)
	}
	if n := countCommand(conn.calls, "createRole"); n != 0 {
		t.Errorf("a refused grant still reached createRole %d times", n)
	}
}

// TestValidateRole_BuiltinIsScopedToItsDatabase — `root` exists only in admin, so
// `role.present name=root database=appdb` is a legal user-defined name mongod would
// create. Refusing it would refuse input Apply accepts, which is the NIM-786 defect
// pointing the other way.
func TestValidateRole_BuiltinIsScopedToItsDatabase(t *testing.T) {
	grant := []any{map[string]any{"resource": map[string]any{"cluster": true}, "actions": []any{"listDatabases"}}}

	if errs := validateRolePresent(mustFields(t, map[string]any{
		"addr": "127.0.0.1:27017", "name": "root", "database": "appdb", "privileges": grant,
	})); len(errs) != 0 {
		t.Errorf("`root` in a non-admin database is a user-defined name and was refused: %v", errs)
	}
	if errs := validateRolePresent(mustFields(t, map[string]any{
		"addr": "127.0.0.1:27017", "name": "root", "database": "admin", "privileges": grant,
	})); len(errs) == 0 {
		t.Error("`root` in admin IS built-in and must be refused")
	}
	// The five that exist in every database are refused wherever they are named.
	if errs := validateRolePresent(mustFields(t, map[string]any{
		"addr": "127.0.0.1:27017", "name": "readWrite", "database": "appdb", "privileges": grant,
	})); len(errs) == 0 {
		t.Error("`readWrite` exists in every database and must be refused there too")
	}
}

// TestPrivilegeKey_NamesCannotCollide — the canonical form is the whole of the
// diff, so two different grants rendering the same string would make a drift
// invisible. A database name may carry a comma on Unix and a collection name very
// nearly anything.
func TestPrivilegeKey_NamesCannotCollide(t *testing.T) {
	a := privResource{db: "a", collection: "b,collection="}
	b := privResource{db: "a,collection=b", collection: ""}
	if a.key() == b.key() {
		t.Fatalf("two different resources render the same key: %s", a.key())
	}
}

// TestRoleProbe_UnreadableReplyIsAFailureNotAbsence — the same rule as the
// collection and index probes. Decoding an unparseable rolesInfo as "the role is
// not there" makes `role.absent` report `present: false` about a role that still
// grants everything it did, and a scenario gating teardown on that field walks
// straight past it.
func TestRoleProbe_UnreadableReplyIsAFailureNotAbsence(t *testing.T) {
	for _, state := range []string{"present", "absent"} {
		t.Run(state, func(t *testing.T) {
			conn := &fakeConn{rawByName: map[string]bson.Raw{
				"rolesInfo": rawDoc(t, bson.D{{Key: "ok", Value: int32(1)}}), // no roles array
			}}
			params := declaredGrant()
			stream := roleApply(t, newModule(conn), state, params)

			if msg := failureMessage(t, stream); !strings.Contains(msg, "rolesInfo") {
				t.Errorf("an unreadable probe reply must fail, got %q", msg)
			}
			for _, write := range []string{"createRole", "updateRole", "dropRole"} {
				if n := countCommand(conn.calls, write); n != 0 {
					t.Errorf("%s was sent %d times after an unreadable probe", write, n)
				}
			}
		})
	}
}

// TestRolePresent_DuplicateInheritedRoleIsRefused — the other half of the grant.
// `parsePrivileges` refuses a resource granted twice for the reason mongod merges
// them; the inherited-roles list has the same shape and needed the same rule.
func TestRolePresent_DuplicateInheritedRoleIsRefused(t *testing.T) {
	params := map[string]any{
		"addr": "127.0.0.1:27017", "name": "app_writer", "database": "appdb",
		"roles": []any{
			map[string]any{"role": "read", "db": "appdb"},
			map[string]any{"role": "read", "db": "appdb"},
		},
	}
	errs := validateRolePresent(mustFields(t, params))
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), "already inherited") {
		t.Fatalf("expected a refusal naming the duplicate, got %v", errs)
	}
}
