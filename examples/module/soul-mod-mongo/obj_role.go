// The `role` object — one user-defined MongoDB role of a live mongod, reconciled
// through createRole / updateRole / dropRole (role.go).
//
// It is separate from `user` and not a parameter of it because a role is its own
// subject: it is named, it is referenced by users that do not know each other, and
// it outlives any of them. And unlike a user, its whole state is READABLE —
// rolesInfo returns the grant structurally — so this is the object of the artifact
// that converges on a real diff rather than only on existence.
//
// Built-in roles are refused on both actions. `read`, `root` and the rest are part
// of mongod's own authorization model; redefining or dropping one is not a state
// this object serves.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// role binds the object's actions to the shared driver. Both go through the shared
// connect path: a role is managed after the first admin exists, so there is no
// bootstrap case and no primary to hop to.
func (m *MongoModule) role() *object {
	return &object{
		impl: m,
		name: "role",
		decl: roleStates(),
		actions: map[string]action{
			"present": {validate: validateRolePresent, apply: (*MongoModule).applyRolePresent},
			"absent":  {validate: validateRoleAbsent, apply: (*MongoModule).applyRoleAbsent},
		},
	}
}

// roleDef is the object's entry in the artifact's bundle.
func roleDef(m *MongoModule) module.Def {
	return module.Def{
		Name:         "role",
		Description:  "One user-defined MongoDB role, reconciled through createRole / updateRole / dropRole.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "mongod"}},
		Impl:         m.role(),
		States:       roleStates(),
	}
}

// roleStates declares the parameters of every action this object serves.
func roleStates() map[string]module.State {
	return map[string]module.State{
		"present": {
			Description: "Reconcile a user-defined role to the declared grant. Reads the LIVE grant first\n" +
				"(rolesInfo with showPrivileges) and branches on the comparison: absent -> createRole;\n" +
				"present and already granting exactly this -> no-op (changed=false); present and\n" +
				"different -> updateRole.\n" +
				"\n" +
				"★ updateRole REPLACES the grant rather than adding to it, which is what makes this a\n" +
				"converge: a privilege the role picked up out of band is gone afterwards, and the\n" +
				"declared grant is what remains.\n" +
				"\n" +
				"The comparison is on a canonical form — actions sorted and de-duplicated, resources in\n" +
				"one spelling — because mongod returns them in an order of its own and a raw comparison\n" +
				"would report a change on every apply. A role that grants nothing (neither privileges\n" +
				"nor roles) is refused. A BUILT-IN name is refused. No dry-run preview.",
			Input: connectInput(module.Input{
				"name": {Type: module.String, Required: true,
					Description: "The role being managed. This is the SUBJECT of the step, not the role of the account it connects as. A built-in mongod name (read/readWrite/root/…) is refused: this object manages user-defined roles only.",
				},
				"database": {Type: module.String, Default: "admin",
					Description: "The DB the role lives in — a role name is unique only within one. Defaults to admin, the home of cluster-wide roles. A privilege resource and an inherited role that name no db of their own inherit this one.",
				},
				"privileges": {Type: module.List,
					Description: "What the role grants directly: a list of { resource, actions }. `resource` names EXACTLY ONE of — { db: \"appdb\", collection: \"events\" } (collection may be \"\" for the whole DB; db omitted inherits `database`), { cluster: true }, or { any_resource: true }; naming two is refused rather than resolved, since they grant very different things. `actions` is a non-empty list of mongod action names (find, insert, replSetGetStatus, …), sorted and de-duplicated before comparison. May be empty if `roles` is not.",
				},
				"roles": {Type: module.List,
					Description: "The roles this one INHERITS: a list of { role, db }. db with no value inherits `database`. May be empty if `privileges` is not — but not both: a role that confers nothing is not a state to declare.",
				},
			}),
		},
		"absent": {
			Description: "Remove a user-defined role via dropRole. Idempotent through the same rolesInfo probe:\n" +
				"a role the instance does not have is a no-op (changed=false).\n" +
				"\n" +
				"A BUILT-IN name is refused rather than attempted — dropping one would be removing part\n" +
				"of mongod's own authorization model.\n" +
				"\n" +
				"★ Users holding the role are NOT touched: mongod removes the role from them, and any\n" +
				"access it granted goes with it. Dropping a role is a privilege change for every account\n" +
				"that held it. No dry-run preview.",
			Input: connectInput(module.Input{
				"name": {Type: module.String, Required: true,
					Description: "The role being removed. This is the SUBJECT of the step, not the role of the account it connects as.",
				},
				"database": {Type: module.String, Default: "admin",
					Description: "The DB the role lives in (default admin). dropRole runs in this DB's context.",
				},
			}),
		},
	}
}
