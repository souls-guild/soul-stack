// Guards on the `database` object, whose design is mostly what it does NOT serve.
//
// The refusal of admin/local/config is asserted on BOTH paths on purpose. It is the
// one gate in this artifact that is more than a description in a state's prose,
// because its blast radius is the server rather than the operator's data — and a
// runner is free never to call Validate.
package main

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
)

// dbNode scripts dropDatabase the way mongod answers it: `{dropped: <name>}` when
// it removed something, `{ok: 1}` alone when there was nothing to remove. That
// reply IS the idempotency probe, so the fake must be able to express both.
func dbNode(t *testing.T, dropped string, existing ...string) *fakeConn {
	t.Helper()
	c := &fakeConn{}
	c.respond = func(_ string, cmd bson.D) (bson.Raw, error) {
		switch cmd[0].Key {
		case "listDatabases":
			return dbListReply(t, existing...), nil
		case "dropDatabase":
			if dropped == "" {
				return okRaw(), nil
			}
			return rawDoc(t, bson.D{{Key: "dropped", Value: dropped}, {Key: "ok", Value: int32(1)}}), nil
		}
		return okRaw(), nil
	}
	return c
}

func dbApply(t *testing.T, m *MongoModule, params map[string]any) *applyStream {
	t.Helper()
	stream := &applyStream{}
	if err := m.database().Apply(&pluginv1.ApplyRequest{State: "absent", Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	return stream
}

func TestDatabaseAbsent_DropsInTheRightDatabase(t *testing.T) {
	conn := dbNode(t, "appdb", "appdb")
	stream := dbApply(t, newModule(conn), map[string]any{"addr": "127.0.0.1:27017", "name": "appdb"})

	if changed, _ := outcome(t, stream); !changed {
		t.Error("dropping an existing database must report changed=true")
	}
	call, ok := lastCommand(conn.calls, "dropDatabase")
	if !ok {
		t.Fatal("no dropDatabase was sent")
	}
	// dropDatabase acts on the database it is RUN IN, so the command going to the
	// wrong one would destroy the wrong data.
	if call.db != "appdb" {
		t.Errorf("dropDatabase ran in %q, want appdb", call.db)
	}
}

func TestDatabaseAbsent_Idempotent_NoOp(t *testing.T) {
	conn := dbNode(t, "") // no `dropped` field AND the database was not listed
	stream := dbApply(t, newModule(conn), map[string]any{"addr": "127.0.0.1:27017", "name": "appdb"})

	if changed, msg := outcome(t, stream); changed {
		t.Errorf("a database that is not there must be a no-op, got %q", msg)
	}
	if _, msg := outcome(t, stream); !strings.Contains(msg, "already absent") {
		t.Errorf("the no-op must say the database was not there, got %q", msg)
	}
}

// TestDatabaseAbsent_ChangedComesFromTheServerNotAProbe — an under-privileged
// caller gets a well-formed EMPTY listDatabases (mongod silently applies
// authorizedDatabases since 4.0.5), so a probe-based no-op would report "already
// absent" about a database that is still there. Reading dropDatabase's own answer
// has no such gap. This asserts the source of `changed`: the same fake answers
// listDatabases with nothing, and the step must still see the drop.
func TestDatabaseAbsent_ChangedComesFromTheServerNotAProbe(t *testing.T) {
	conn := dbNode(t, "appdb") // dropped, but listDatabases shows nothing
	stream := dbApply(t, newModule(conn), map[string]any{"addr": "127.0.0.1:27017", "name": "appdb"})

	if changed, msg := outcome(t, stream); !changed {
		t.Errorf("the drop the server reports must be changed=true whatever listDatabases says, got %q", msg)
	}
}

// TestDatabaseAbsent_DropWithoutTheDroppedFieldIsStillAChange — the other
// direction, and the one whose failure is worst. A reply shape this code does not
// know carries no `dropped`; reporting `changed=false` after destroying a database
// would leave a downstream `when: register.drop.changed` restore or notify unfired
// and the run's report saying nothing happened. The prior read is what decides it.
func TestDatabaseAbsent_DropWithoutTheDroppedFieldIsStillAChange(t *testing.T) {
	conn := dbNode(t, "", "appdb") // no `dropped` field, but the database WAS there
	stream := dbApply(t, newModule(conn), map[string]any{"addr": "127.0.0.1:27017", "name": "appdb"})

	if changed, msg := outcome(t, stream); !changed {
		t.Errorf("a database that was there before the drop must report changed=true, got %q", msg)
	}
}

// TestDatabaseAbsent_ServerDatabasesAreRefusedOnBothPaths — dropping admin, local
// or config destroys the users, roles and replication bookkeeping mongod itself
// needs. Validate says so before the run; Apply says so because a runner need not
// call Validate.
func TestDatabaseAbsent_ServerDatabasesAreRefusedOnBothPaths(t *testing.T) {
	for _, name := range []string{"admin", "local", "config"} {
		t.Run(name, func(t *testing.T) {
			errs := validateDatabaseAbsent(mustFields(t, map[string]any{"addr": "127.0.0.1:27017", "name": name}))
			if len(errs) == 0 {
				t.Error("Validate accepted it")
			}

			// Straight at the action, BYPASSING object.Apply. Going through it
			// would prove nothing: object.Apply runs the action's own validate
			// first, so this assertion would pass on a build with the in-action
			// guard deleted — and that guard is the one that matters, because it
			// is what protects a future caller that reaches the action directly.
			conn := dbNode(t, name)
			stream := &applyStream{}
			_ = newModule(conn).applyDatabaseAbsent(context.Background(), stream, conn,
				mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "name": name}))
			if msg := failureMessage(t, stream); !strings.Contains(msg, name) {
				t.Errorf("the in-action refusal must name the database, got %q", msg)
			}
			if n := countCommand(conn.calls, "dropDatabase"); n != 0 {
				t.Errorf("a refused step still sent dropDatabase %d times", n)
			}
		})
	}
}

// TestDatabase_ServesNoPresent — the missing action is the design, not an omission:
// MongoDB has no command that creates a database, so a `present` here could only
// report success having done nothing observable. A later change that adds one
// should have to come past this test and its reasoning.
func TestDatabase_ServesNoPresent(t *testing.T) {
	states := (&MongoModule{}).database().states()
	if len(states) != 1 || states[0] != "absent" {
		t.Fatalf("the database object serves %v; it serves `absent` alone — creating a database is "+
			"mongo.collection.present's doing, because MongoDB has no command that creates one", states)
	}
}
