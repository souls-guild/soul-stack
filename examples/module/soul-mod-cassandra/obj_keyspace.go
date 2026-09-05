// The `keyspace` object — a Cassandra keyspace and its replication.
//
// `present` and `absent` are two ACTIONS rather than one state carrying
// `params.state`. The verb belongs in the address (ADR-020 amendment 2026-09-02,
// NIM-765), and splitting it is what lets each half declare only what it reads:
// `replication` and `durable_writes` are the present half's, and are refused on the
// absent one instead of being promised to it.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// keyspace binds the object's actions to the shared driver.
func (m *CassandraModule) keyspace() *object {
	return &object{
		impl: m,
		name: "keyspace",
		decl: keyspaceStates(),
		actions: map[string]action{
			"present": {validate: validateKeyspacePresent, apply: (*CassandraModule).applyKeyspacePresent},
			"absent":  {validate: validateKeyspaceAbsent, apply: (*CassandraModule).applyKeyspaceAbsent},
		},
	}
}

// keyspaceDef is the object's entry in the artifact's bundle.
func keyspaceDef(m *CassandraModule) module.Def {
	return module.Def{
		Name:         "keyspace",
		Description:  "A Cassandra keyspace: its replication strategy and factors, reconciled through CREATE / ALTER / DROP KEYSPACE.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "cassandra"}},
		Impl:         m.keyspace(),
		States:       keyspaceStates(),
	}
}

// keyspaceStates declares the parameters of every action this object serves.
func keyspaceStates() map[string]module.State {
	return map[string]module.State{
		"present": {
			Description: "Converge a keyspace to the declared replication, over the native CQL protocol\n" +
				"(no cqlsh, no shell). The live schema is READ first and one of three things\n" +
				"happens: absent -> CREATE KEYSPACE; already as declared -> no-op (changed=false);\n" +
				"declared differently -> ALTER KEYSPACE. Idempotent: a repeat apply on a converged\n" +
				"keyspace changes nothing and reports changed=false.\n" +
				"\n" +
				"SCHEMA AGREEMENT: after its own CREATE or ALTER the state WAITS until every\n" +
				"reachable node reports the same schema version, and fails if they do not reach\n" +
				"it. Cassandra propagates schema through gossip, so a state that reported changed\n" +
				"the moment the statement returned would leave the next step of the run failing on\n" +
				"a keyspace that 'does not exist' on the node it happened to reach.\n" +
				"\n" +
				"★ REPAIR IS NOT PART OF THIS STATE. Raising a replication factor changes the\n" +
				"SCHEMA and moves no data: the new replicas hold NOTHING until `nodetool repair`\n" +
				"has run on every node of the affected datacenters. This state does not run it and\n" +
				"does not wait for it — there is no CQL statement for repair (it is a\n" +
				"JMX/nodetool operation that runs for hours on real data), and this plugin speaks\n" +
				"only CQL. Until repair completes, a read at a consistency level the new factor\n" +
				"made reachable can return NO DATA. Output.repair_required is true whenever this\n" +
				"state raised a factor, AND whenever it changed the strategy class — the two\n" +
				"strategies do not share an option vocabulary, so which nodes hold a partition\n" +
				"changes regardless of the numbers. Gate the follow-up on\n" +
				"register.self.repair_required rather than on the step having succeeded.\n" +
				"Lowering a factor instead leaves surplus copies on the former replicas; that is\n" +
				"not a correctness problem, and `nodetool cleanup` reclaims the space when you\n" +
				"choose to.\n" +
				"No dry-run preview (the plugin does not implement PlanReadSafe).",
			Input: withConnect(module.Input{
				"name": {Type: module.String, Required: true,
					Description: "Keyspace to converge. Lowercase letters, digits and underscore, starting with a letter or underscore, at most 48 characters. An uppercase name is REFUSED rather than folded: Cassandra lowercases an unquoted identifier, so the state would create it and then never find it again.",
				},
				"replication": {Type: module.Map, Required: true,
					Description: "Replication: {class: SimpleStrategy, replication_factor: 3} or {class: NetworkTopologyStrategy, dc1: 3, dc2: 2}. Factors are INTEGERS (a string is refused, not coerced). Only these two strategies are managed.",
				},
				"durable_writes": {Type: module.Bool, Default: true,
					Description: "Whether writes go through the commit log. Default true. false trades durability for write throughput and is a deliberate choice, never a default.",
				},
			}),
		},
		"absent": {
			Description: "Drop a keyspace over the native CQL protocol (no cqlsh, no shell), with the\n" +
				"same schema-agreement wait as `present`. Idempotent: a keyspace that is not\n" +
				"there is a no-op (changed=false).\n" +
				"\n" +
				"★ DROP KEYSPACE DESTROYS EVERY TABLE AND EVERY ROW IN IT. There is no\n" +
				"confirmation parameter and no dry-run preview (the plugin does not implement\n" +
				"PlanReadSafe): the guard is that a scenario has to name the keyspace to drop it.",
			Input: withConnect(module.Input{
				"name": {Type: module.String, Required: true,
					Description: "Keyspace to drop. Same identifier rules as `present`. A keyspace that does not exist is a no-op, not an error.",
				},
			}),
		},
	}
}
