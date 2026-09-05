// ★ Schema convergence — the invariant that separates a state which reported the
// truth from one which reported a statement.
//
// Cassandra propagates a schema change through gossip, so the nodes do NOT hold the
// same schema the instant a DDL statement returns. An action that reported `changed`
// at that moment leaves the cluster where the NEXT step of the run fails on a
// keyspace or table that "does not exist" — the failure lands on the innocent step
// and the guilty one is green.
//
// Two mechanisms, and they are not interchangeable:
//
//  1. [converge] — every reconciling state READS the live schema before it writes,
//     and gets one of three answers. This is the redis `cluster` trick
//     ([clusterFormStatus] there): a fully built cluster is a no-op, a half-built one
//     is finished rather than rebuilt. It is what makes a state repeatable — a run
//     that died mid-DDL leaves the next run to see `convergeDiffers` and issue
//     exactly the missing part. Without it, convergence would be a one-shot promise.
//
//  2. [awaitSchema] — after its own DDL, the state waits until every reachable node
//     reports one schema version, and a wait that runs out is a FAILURE.
//
// Both live INSIDE the state. The alternative — an `instance.schema-agreed` probe
// plus a `when:` over it in the scenario — was rejected deliberately: it is a second
// guard of the same invariant, held in a different file by a different author, and
// two guards of one invariant drift apart. The state that made the change is the one
// that owes the proof.
package main

import (
	"context"
	"fmt"
)

// converge is the three-way answer a reconciling state gets from reading the live
// schema before it writes anything.
type converge int

const (
	// convergeAbsent — the subject does not exist; create it.
	convergeAbsent converge = iota
	// convergeMatches — the subject exists and is what was declared; no-op,
	// changed=false.
	convergeMatches
	// convergeDiffers — the subject exists but not as declared. What that means is
	// the state's own: a keyspace ALTERs to the declared replication, a table
	// refuses, because Cassandra cannot change a primary key or a column type and
	// re-creating the table would be data loss.
	convergeDiffers
)

// awaitSchema blocks until every reachable node reports the same schema version.
//
// This delegates to the driver rather than polling `system.local` and `system.peers`
// here, and that is a deliberate choice against the redis precedent, where
// [waitGossipConverged] does poll. The difference is the DOWN node: it lingers in
// `system.peers` carrying the schema version it had when it left, so a naive
// comparison over that table never agrees and every DDL step would fail after its
// budget. Filtering it out needs the driver's host pool — which node is actually up
// — and that is not reachable through a CQL interface. A worse copy of a correct
// implementation is not independence, it is a second bug.
//
// The budget is the driver's MaxWaitSchemaAgreement (60s by default) and is not
// exposed as a parameter: a knob here would be the second guard this file exists to
// avoid, and a cluster that has not agreed in a minute has a problem an operator
// should see rather than wait out.
func awaitSchema(ctx context.Context, s cassSession, subject string) error {
	if err := s.AwaitSchemaAgreement(ctx); err != nil {
		return fmt.Errorf("%s: the change was accepted but the cluster did not reach schema agreement: %w "+
			"(the next step would see a schema that only some nodes have)", subject, err)
	}
	return nil
}
