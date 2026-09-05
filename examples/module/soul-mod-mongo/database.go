// The `database` object — one MongoDB database, DROPPED.
//
// ★ WHY THERE IS NO `present`, AND WHY THAT IS THE ANSWER RATHER THAN A GAP
//
// MongoDB has no command that creates a database. One exists once it holds a
// collection, and stops existing when it holds none. So a `database.present` action
// could only do one of two things: nothing at all — a step reporting success having
// had no observable effect, which is the shape of lie this artifact exists to avoid
// — or secretly create a collection, inventing state the operator never declared
// and leaving a collection nobody asked for behind.
//
// What actually creates a database is `mongo.collection.present`, which is
// MongoDB's own semantics rather than a workaround, and it reports
// `database_created` so a scenario can see when it happened.
//
// Dropping one, on the other hand, IS a real operation with a real command and a
// readable before/after, so it is here. NIM-383 asked whether a `database` module
// makes sense at all; this file is the answer: half of it does.
package main

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/protobuf/types/known/structpb"
)

// protectedDatabases are the databases mongod runs on. `dropDatabase` against one
// of them destroys the users, roles and replica-set bookkeeping the instance itself
// needs, so they are refused as a subject — the one place in this artifact where a
// destructive action is gated by more than its own description, because the blast
// radius is the server rather than the operator's data.
var protectedDatabases = map[string]bool{"admin": true, "local": true, "config": true}

// applyDatabaseAbsent drops the database. Idempotent through listDatabases: one that
// is not there is a no-op.
//
// ★ THIS DESTROYS EVERY COLLECTION, INDEX AND DOCUMENT IN THE DATABASE. As with
// `collection.absent` there is no confirmation parameter — a flag an author always
// sets is not a gate. What guards it is a scenario deciding when to run it, and the
// refusal above for the three databases whose loss would break the server.
func (m *MongoModule) applyDatabaseAbsent(ctx context.Context, stream eventStream, conn mongoConn, params *structpb.Struct) error {
	f := params.GetFields()
	secrets := paramSecrets(f)
	name := stringOrEmpty(f["name"])

	// Refused in Apply as well as in Validate: a runner need not call Validate,
	// and this is the one refusal here whose cost is the instance.
	if protectedDatabases[name] {
		return sendFailure(stream, fmt.Sprintf(
			"database %q is one mongod runs on (admin, local, config): dropping it destroys the users, roles and "+
				"replication bookkeeping the instance needs", name))
	}

	// ★ `changed` HAS TWO SOURCES, AND NEITHER ALONE IS ENOUGH.
	//
	// `dropDatabase` replies `{dropped: "<name>", ok: 1}` when it removed a database
	// and `{ok: 1}` with no `dropped` when there was none. That is the server's own
	// answer and it is the better one — but only in the direction it asserts
	// something. A MISSING `dropped` is not the fact "nothing was dropped": it is
	// also what a reply shape this code does not know looks like, and reporting
	// `changed=false` after destroying a database is the worst outcome in this
	// artifact — a downstream `when: register.drop.changed` restore or notify never
	// fires, and the run's report says nothing happened.
	//
	// So the prior read decides that direction. It cannot decide the other one:
	// since 4.0.5 mongod silently applies `authorizedDatabases: true` when the
	// caller lacks the cluster `listDatabases` action, so an under-privileged step
	// gets a well-formed EMPTY list rather than an error. Together they have no gap:
	// `dropped` present -> changed, whatever the list said; the list said it was
	// there -> changed, whatever the reply carried; only both saying nothing is a
	// no-op.
	existed, err := databaseExists(ctx, conn, name)
	if err != nil {
		return sendFailure(stream, redactError(err, secrets...))
	}

	raw, err := conn.RunCommand(ctx, name, bson.D{{Key: "dropDatabase", Value: 1}})
	if err != nil {
		return sendFailure(stream, "dropDatabase: "+redactError(err, secrets...))
	}
	_, noDroppedField := raw.LookupErr("dropped")
	dropped := noDroppedField == nil

	if !dropped && !existed {
		return sendOutcome(stream, false, fmt.Sprintf("database %q already absent", name), map[string]any{
			"name": name, "present": false,
		})
	}
	return sendOutcome(stream, true, fmt.Sprintf("database %q dropped", name), map[string]any{
		"name": name, "present": false,
	})
}

// validateDatabaseAbsent — somewhere to connect to, the database this step is
// about, and the refusal of the three the server runs on, so it is said before the
// run rather than in the middle of it (NIM-786).
func validateDatabaseAbsent(f map[string]*structpb.Value) []string {
	errs := validateAddr(f)
	errs = append(errs, requireString(f, "name")...)

	if name := stringOrEmpty(f["name"]); protectedDatabases[name] {
		errs = append(errs, fmt.Sprintf(
			"params.name: %q is one of the databases mongod runs on (admin, local, config) and is not droppable here", name))
	}
	return errs
}
