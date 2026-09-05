// The `role` object — a Cassandra role: the login a service authenticates as.
//
// Cassandra has roles, not users — one concept covering both a login and a group of
// privileges, with `CREATE USER` kept only as a deprecated alias. The object is named
// after the thing it manages.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// role binds the object's actions to the shared driver.
func (m *CassandraModule) role() *object {
	return &object{
		impl: m,
		name: "role",
		decl: roleStates(),
		actions: map[string]action{
			"present": {validate: validateRolePresent, apply: (*CassandraModule).applyRolePresent},
			"absent":  {validate: validateRoleAbsent, apply: (*CassandraModule).applyRoleAbsent},
		},
	}
}

// roleDef is the object's entry in the artifact's bundle.
func roleDef(m *CassandraModule) module.Def {
	return module.Def{
		Name:         "role",
		Description:  "A Cassandra role — the login a service authenticates as — reconciled through CREATE / ALTER / DROP ROLE.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "cassandra"}},
		Impl:         m.role(),
		States:       roleStates(),
	}
}

// roleStates declares the parameters of every action this object serves.
func roleStates() map[string]module.State {
	return map[string]module.State{
		"present": {
			Description: "Converge a Cassandra role over the native CQL protocol (no cqlsh, no shell).\n" +
				"system_auth.roles is READ first: absent -> CREATE ROLE; login and superuser\n" +
				"already as declared -> no-op (changed=false); either of them different ->\n" +
				"ALTER ROLE. Idempotent.\n" +
				"\n" +
				"★ THE PASSWORD OF AN EXISTING ROLE IS NEITHER READ NOR CHANGED. role_password is\n" +
				"applied only on the CREATE path. Cassandra stores the password as a salted hash,\n" +
				"so it cannot be compared with the declared one, and a state that re-applied it\n" +
				"every run would report changed forever — a state that never converges.\n" +
				"★ ROTATION IS NOT SUPPORTED BY THIS ARTIFACT AT ALL, and command.run is NOT a\n" +
				"way around it: the driver binds no values on an ALTER ROLE, so the password\n" +
				"would have to be written into params.cql — into the scenario, the rendered task\n" +
				"and the statement text, none of which the ADR-010 masks cover. Rotate out of\n" +
				"band until this artifact grows a state that owns it.\n" +
				"\n" +
				"On the CREATE path the password IS written into the statement text, because the\n" +
				"driver refuses to bind it there. It never reaches an event, a message or an\n" +
				"error — every failure of this action is redacted — but a cluster with audit or\n" +
				"slow-query logging will record it, which is inherent to Cassandra role\n" +
				"management and true of cqlsh and every other driver.\n" +
				"\n" +
				"No schema-agreement wait, deliberately: a role is a ROW in system_auth, not a\n" +
				"schema change, and it does not travel the gossip path keyspace and table DDL do.\n" +
				"\n" +
				"Grants are not managed by this object. `GRANT`/`REVOKE` on a keyspace or table\n" +
				"go through command.run.\n" +
				"No dry-run preview (the plugin does not implement PlanReadSafe).",
			Input: withConnect(module.Input{
				"name": {Type: module.String, Required: true,
					Description: "Role to converge. This is the SUBJECT of the step, not the role the step authenticates as (that is `username`). Lowercase letters, digits and underscore, at most 48 characters.",
				},
				"login": {Type: module.Bool, Default: true,
					Description: "Whether the role may open a session. Default true. A false role is a privilege group other roles are granted, not a login.",
				},
				"superuser": {Type: module.Bool, Default: false,
					Description: "Whether the role bypasses every permission check. Default false. A superuser can read and alter system_auth itself — grant it deliberately.",
				},
				"role_password": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "Password OF THE ROLE BEING CREATED (vault-ref; keeper resolves it before Apply). Distinct from `password`, which authenticates the connection. Masked in logs/traces/UI and never placed in an event or an error. ★ Applied only when the role is CREATED, and written into the CREATE ROLE statement as a quoted literal because the driver binds no values on that statement — see the state description on what that means and on rotation.",
				},
			}),
		},
		"absent": {
			Description: "Drop a Cassandra role over the native CQL protocol (no cqlsh, no shell).\n" +
				"Idempotent via system_auth.roles: a role that does not exist is a no-op\n" +
				"(changed=false).\n" +
				"\n" +
				"Cassandra refuses to drop a role that still owns permissions or is still\n" +
				"granted to another role; that refusal is reported as the failure it is, not\n" +
				"worked around.\n" +
				"No dry-run preview (the plugin does not implement PlanReadSafe).",
			Input: withConnect(module.Input{
				"name": {Type: module.String, Required: true,
					Description: "Role to drop. The SUBJECT of the step, not the role it authenticates as (`username`). A role that does not exist is a no-op, not an error.",
				},
			}),
		},
	}
}
