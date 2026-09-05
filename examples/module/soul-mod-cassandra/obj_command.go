// The `command` object — an arbitrary CQL statement, run as given.
//
// Non-stateful, so level 3 takes the verb form (`cassandra.command.run`), exactly as
// `core.exec.run` and `redis.command.run` do. It takes ONE verb and will keep taking
// one: the discipline that keeps this artifact's other four objects apart says two
// operations are two objects, so a second thing to do to Cassandra becomes its own
// object rather than a second word here (ADR-020 amendment 2026-09-02).
//
// The name grants nothing. The Errand allow-list decides for a plugin by the
// `ErrandReadSafe` marker, which this artifact deliberately does not implement, so
// `command` is default-denied there like every other object of it.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// command binds the object's single action to the shared driver.
//
// It is the one object with sessionKeyspace true: `params.keyspace` here is the
// keyspace the SESSION connects into, so an author can write unqualified table names
// in their statement the way they would at a cqlsh prompt.
func (m *CassandraModule) command() *object {
	return &object{
		impl:            m,
		name:            "command",
		decl:            commandStates(),
		sessionKeyspace: true,
		actions: map[string]action{
			"run": {validate: validateCommand, apply: (*CassandraModule).applyCommand},
		},
	}
}

// commandDef is the object's entry in the artifact's bundle.
func commandDef(m *CassandraModule) module.Def {
	return module.Def{
		Name:         "command",
		Description:  "An arbitrary CQL statement, run as given against one cluster.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "cassandra"}},
		Impl:         m.command(),
		States:       commandStates(),
	}
}

// commandStates declares the parameters of this object's action.
func commandStates() map[string]module.State {
	return map[string]module.State{
		"run": {
			Description: "Execute a raw CQL statement over the native protocol (imperative verb-action,\n" +
				"no cqlsh and no shell).\n" +
				"By default changed=false (like a probe); the driver cannot tell whether a\n" +
				"statement mutated anything, so declaring it is the author's job — and so is\n" +
				"idempotency, which this action does not provide.\n" +
				"\n" +
				"★ params.args WORK ON DML ONLY — SELECT, INSERT, UPDATE, DELETE, BATCH. The\n" +
				"driver prepares only those, and on any other statement it DISCARDS the values\n" +
				"silently, leaving the ? markers unbound and the statement a syntax error. So\n" +
				"args on a non-DML statement are REFUSED here rather than dropped: you get the\n" +
				"reason before the run instead of an unexplained syntax error during it.\n" +
				"On DML, write `WHERE id = ?` with args, never `WHERE id = ${ ... }`.\n" +
				"\n" +
				"This is the escape hatch for what the four managed objects deliberately leave\n" +
				"out: GRANT and REVOKE, table options, clustering order, indexes, materialized\n" +
				"views and user-defined types. Those take their values in the statement text,\n" +
				"because the driver will not bind them.\n" +
				"★ THAT MAKES IT THE WRONG PLACE FOR A SECRET. A value written into params.cql\n" +
				"is in the scenario, in the rendered task and in the statement — none of which\n" +
				"the ADR-010 masks cover. Rotating a role password through this action is\n" +
				"therefore NOT supported; see role.present on what this artifact does and does\n" +
				"not manage.\n" +
				"\n" +
				"A statement the driver would not prepare may be DDL, so this action WAITS for\n" +
				"schema agreement after it, exactly as the keyspace and table states do.\n" +
				"\n" +
				"WARNING (security): rows come back in Output.rows IN PLAINTEXT — that is\n" +
				"Cassandra's answer, not a plugin-managed secret, so it is NOT covered by the\n" +
				"ADR-010 masks. DO NOT read credential tables through this action\n" +
				"(system_auth.roles carries salted_hash). params.password itself is masked and\n" +
				"never reaches the statement or the events (see the guard tests). Values are\n" +
				"rendered as TEXT, because a CQL row holds types an event has no equivalent for;\n" +
				"do not build a comparison on their shape. At most 100 rows are returned and\n" +
				"Output.truncated says when that bound was hit.\n" +
				"No dry-run preview (the plugin does not implement PlanReadSafe).",
			Input: withConnect(module.Input{
				"cql": {Type: module.String, Required: true,
					Description: "The CQL statement. One statement — the protocol takes no batch of them separated by semicolons. On DML (SELECT/INSERT/UPDATE/DELETE/BATCH) write ? markers and pass the values in params.args; on anything else the driver binds nothing, so the values go in the statement text — and a secret must therefore never be one of them.",
				},
				"args": {Type: module.List,
					Description: "Values bound to the statement's ? markers, in order. A whole number arrives as a 64-bit integer, so a column declared int needs a value in its range. ★ Accepted ONLY on a DML statement (SELECT/INSERT/UPDATE/DELETE/BATCH): the driver binds nothing on anything else, so setting this beside a DDL or a GRANT is refused rather than silently ignored.",
				},
				"keyspace": {Type: module.String,
					Description: "Session keyspace, so the statement may name tables unqualified. Optional — a fully qualified <keyspace>.<table> needs none. ★ The session connects INTO it, so a keyspace that does not exist yet fails the connection: create it with keyspace.present first, or fully qualify and leave this unset.",
				},
				"changed": {Type: module.Bool, Default: false,
					Description: "Report the result as changed=true (for a statement that actually mutates). Default false (probe semantics).",
				},
			}),
		},
	}
}
