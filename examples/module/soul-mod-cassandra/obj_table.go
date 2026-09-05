// The `table` object — a CQL table: its columns and its primary key.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// table binds the object's actions to the shared driver.
//
// sessionKeyspace stays false: `params.keyspace` here names where the TABLE lives,
// not a keyspace to connect into. Every statement fully qualifies `<keyspace>.<table>`,
// which is what lets `present` report a missing keyspace in the server's own words
// instead of dying in the connect path — the driver issues a USE on connect, so a
// session keyspace that does not exist yet fails the connection itself.
func (m *CassandraModule) table() *object {
	return &object{
		impl: m,
		name: "table",
		decl: tableStates(),
		actions: map[string]action{
			"present": {validate: validateTablePresent, apply: (*CassandraModule).applyTablePresent},
			"absent":  {validate: validateTableAbsent, apply: (*CassandraModule).applyTableAbsent},
		},
	}
}

// tableDef is the object's entry in the artifact's bundle.
func tableDef(m *CassandraModule) module.Def {
	return module.Def{
		Name:         "table",
		Description:  "A CQL table: its columns and primary key, created or dropped through CREATE / DROP TABLE.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "cassandra"}},
		Impl:         m.table(),
		States:       tableStates(),
	}
}

// tableStates declares the parameters of every action this object serves.
func tableStates() map[string]module.State {
	return map[string]module.State{
		"present": {
			Description: "Create a CQL table over the native CQL protocol (no cqlsh, no shell), with the\n" +
				"same schema-agreement wait as keyspace.present: the state does not report\n" +
				"changed until every reachable node holds the new schema, so the next step of the\n" +
				"run cannot fail on a table that 'does not exist'.\n" +
				"system_schema.columns is READ first: absent -> CREATE TABLE; already as declared\n" +
				"-> no-op (changed=false). Idempotent.\n" +
				"\n" +
				"★ AN EXISTING TABLE THAT DOES NOT MATCH IS A FAILURE, NOT AN ALTER. Cassandra\n" +
				"cannot change a primary key at all and cannot change a column's type; the only\n" +
				"reconciliation it would accept is adding a column, and a state that added what it\n" +
				"could while ignoring the primary key it could not would report success on a table\n" +
				"that is not the declared one. Dropping and re-creating would be data loss\n" +
				"performed by a converge step. So the state names exactly what differs and stops.\n" +
				"A column the live table has and this declaration does not is NOT a difference —\n" +
				"the declaration is the columns this service needs, not the complete set the table\n" +
				"may carry.\n" +
				"\n" +
				"TABLE OPTIONS ARE NOT MANAGED: compaction, compression, gc_grace_seconds,\n" +
				"default_time_to_live and caching are neither set nor compared. A table needing\n" +
				"them is created with command.run. There is deliberately no options parameter,\n" +
				"because one that applied only at CREATE would silently do nothing on a table that\n" +
				"already exists.\n" +
				"No dry-run preview (the plugin does not implement PlanReadSafe).",
			Input: withConnect(module.Input{
				"keyspace": {Type: module.String, Required: true,
					Description: "Keyspace the table lives in. NOT a session keyspace: statements fully qualify <keyspace>.<table>, so a keyspace that does not exist yet is reported by CREATE TABLE rather than failing the connection.",
				},
				"name": {Type: module.String, Required: true,
					Description: "Table to converge. Lowercase letters, digits and underscore, starting with a letter or underscore, at most 48 characters.",
				},
				"columns": {Type: module.Map, Required: true,
					Description: "Columns as a map of name to CQL type: {id: uuid, created_at: timestamp, tags: \"map<text, text>\"}. Every column named by partition_key or clustering_key must appear here.",
				},
				"partition_key": {Type: module.List, Required: true,
					Description: "Partition key columns, in order — the composite partition key ((a, b)) when there is more than one. Must name at least one column: a Cassandra table has no default primary key.",
				},
				"clustering_key": {Type: module.List,
					Description: "Clustering columns, in order. Optional. Clustering ORDER is not managed (the table is created with the default ascending order); a table needing DESC is created with command.run.",
				},
			}),
		},
		"absent": {
			Description: "Drop a CQL table over the native CQL protocol (no cqlsh, no shell), with the\n" +
				"same schema-agreement wait as `present`. Idempotent: a table that is not there\n" +
				"is a no-op (changed=false).\n" +
				"\n" +
				"★ DROP TABLE DESTROYS EVERY ROW IN IT. There is no confirmation parameter and no\n" +
				"dry-run preview (the plugin does not implement PlanReadSafe).",
			Input: withConnect(module.Input{
				"keyspace": {Type: module.String, Required: true,
					Description: "Keyspace the table lives in. Statements fully qualify <keyspace>.<table>; this is not a session keyspace.",
				},
				"name": {Type: module.String, Required: true,
					Description: "Table to drop. Same identifier rules as `present`. A table that does not exist is a no-op, not an error.",
				},
			}),
		},
	}
}
