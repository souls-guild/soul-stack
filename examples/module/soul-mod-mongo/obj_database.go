// The `database` object — one MongoDB database, dropped (database.go).
//
// ONE action, and the missing one is the design rather than an omission: MongoDB
// has no command that creates a database, so a `present` here could only report
// success having done nothing observable, or secretly create a collection to make
// itself true. `mongo.collection.present` is what brings a database into being —
// that is MongoDB's own semantics — and it reports `database_created` when it does.
//
// One action is not unusual in this artifact: `command` and `instance` have one
// each. What an object is for is being a subject, not having a pair of verbs.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// database binds the object's single action to the shared driver.
func (m *MongoModule) database() *object {
	return &object{
		impl: m,
		name: "database",
		decl: databaseStates(),
		actions: map[string]action{
			"absent": {validate: validateDatabaseAbsent, apply: (*MongoModule).applyDatabaseAbsent},
		},
	}
}

// databaseDef is the object's entry in the artifact's bundle.
func databaseDef(m *MongoModule) module.Def {
	return module.Def{
		Name:         "database",
		Description:  "One MongoDB database, dropped. There is no `present`: MongoDB creates a database when a collection is created in it (mongo.collection.present), not by a command of its own.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "mongod"}},
		Impl:         m.database(),
		States:       databaseStates(),
	}
}

// databaseStates declares the parameters of the one action this object serves.
func databaseStates() map[string]module.State {
	return map[string]module.State{
		"absent": {
			Description: "Drop the database via dropDatabase. Idempotent through listDatabases: one that is not\n" +
				"there is a no-op (changed=false).\n" +
				"\n" +
				"★ THIS DESTROYS EVERY COLLECTION, INDEX AND DOCUMENT IN THE DATABASE. There is no\n" +
				"confirmation parameter — this artifact has none anywhere, and a flag an author always\n" +
				"sets is not a gate. What guards this is a scenario deciding when to run it.\n" +
				"\n" +
				"admin, local and config are REFUSED: they are what mongod itself runs on, and dropping\n" +
				"one destroys the users, roles and replication bookkeeping the instance needs. That\n" +
				"refusal is the one gate in this artifact that is not just a description, because its\n" +
				"blast radius is the server rather than the operator's data.\n" +
				"\n" +
				"There is deliberately no `present` beside this — see the module description.\n" +
				"No dry-run preview.",
			Input: connectInput(module.Input{
				"name": {Type: module.String, Required: true,
					Description: "The database being dropped. This is the SUBJECT of the step; the connection's own auth database is `auth_db`.",
				},
			}),
		},
	}
}
