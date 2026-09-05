// The `collection` object — one MongoDB collection with its options
// (collection.go).
//
// Two actions and no third: creating a collection is also what brings its DATABASE
// into being, since MongoDB has no command that creates one, so there is no
// `database.present` beside this and the `database` object serves `absent` alone.
//
// Indexes are NOT a parameter here. They are their own object, for reasons written
// out in obj_index.go — the short form being that an index has its own lifecycle,
// cannot be modified in place, and a list of them inside this action would re-admit
// at the parameter level exactly what the address discipline removed.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// collection binds the object's actions to the shared driver.
func (m *MongoModule) collection() *object {
	return &object{
		impl: m,
		name: "collection",
		decl: collectionStates(),
		actions: map[string]action{
			"present": {validate: validateCollectionPresent, apply: (*MongoModule).applyCollectionPresent},
			"absent":  {validate: validateCollectionAbsent, apply: (*MongoModule).applyCollectionAbsent},
		},
	}
}

// collectionDef is the object's entry in the artifact's bundle.
func collectionDef(m *MongoModule) module.Def {
	return module.Def{
		Name:         "collection",
		Description:  "One MongoDB collection with its options, created through `create` and reconciled through `collMod`.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "mongod"}},
		Impl:         m.collection(),
		States:       collectionStates(),
	}
}

// collectionSubject is the addressing pair both actions take.
func collectionSubject() module.Input {
	return module.Input{
		"database": {Type: module.String, Required: true,
			Description: "The database the collection lives in. REQUIRED and NOT defaulted to admin: a collection in the admin database is almost never intended, and a default there would create one silently. Creating a collection is also what brings a database into being — MongoDB has no command that creates one.",
		},
		"name": {Type: module.String, Required: true,
			Description: "The collection this step is about.",
		},
	}
}

// collectionStates declares the parameters of every action this object serves.
func collectionStates() map[string]module.State {
	own := collectionSubject()
	own["capped"] = module.Param{Type: module.Bool,
		Description: "IMMUTABLE. A capped collection: fixed size, insertion-ordered, oldest documents overwritten. Requires `size`. Cannot be turned on or off on a live collection.",
	}
	own["size"] = module.Param{Type: module.Int,
		Description: "IMMUTABLE. Maximum size in BYTES of a capped collection. Required when `capped` is true.",
	}
	own["max"] = module.Param{Type: module.Int,
		Description: "IMMUTABLE. Maximum number of documents in a capped collection (optional, on top of `size`).",
	}
	own["collation"] = module.Param{Type: module.Map,
		Description: "IMMUTABLE. Default collation of the collection, e.g. { locale: \"en\", strength: 2 }. Fixed at creation.",
	}
	own["timeseries"] = module.Param{Type: module.Map,
		Description: "IMMUTABLE. Time-series options, e.g. { timeField: \"ts\", metaField: \"meta\", granularity: \"seconds\" }. A time-series collection is a different kind of collection and cannot be converted to or from one.",
	}
	own["clustered_index"] = module.Param{Type: module.Map,
		Description: "IMMUTABLE. Clustered-collection spec, e.g. { key: { _id: 1 }, unique: true }. Fixed at creation.",
	}
	own["validator"] = module.Param{Type: module.Map,
		Description: "MUTABLE (collMod). Document validation predicate, e.g. { $jsonSchema: { … } }. Compared against the live one on a canonical form — map keys sorted, numbers by value — so a validator that means the same thing does not report a change on every apply.",
	}
	own["validation_level"] = module.Param{Type: module.String, Enum: []any{"off", "strict", "moderate"},
		Description: "MUTABLE (collMod). Which documents the validator applies to: off, strict or moderate.",
	}
	own["validation_action"] = module.Param{Type: module.String, Enum: []any{"error", "warn"},
		Description: "MUTABLE (collMod). What a validation failure does: error (reject the write) or warn (log it and accept).",
	}

	return map[string]module.State{
		"present": {
			Description: "Create the collection, or bring its MUTABLE options to what is declared.\n" +
				"Idempotent through listCollections: a collection already carrying the declared options\n" +
				"is a no-op (changed=false).\n" +
				"\n" +
				"★ THE MUTABLE/IMMUTABLE SPLIT IS THE POINT. validator / validation_level /\n" +
				"validation_action are changed with collMod. capped / size / max / collation /\n" +
				"timeseries / clustered_index CANNOT be changed on a live collection, and a declared\n" +
				"value that differs from the live one is a FAILURE naming the field — not a silent\n" +
				"no-op and not a drop-and-recreate. The only way to apply such a change is to lose the\n" +
				"collection's data, and that is an operator's decision, not a converge step's.\n" +
				"\n" +
				"An option that is NOT declared is not compared and not sent: mongod's default applies\n" +
				"on create, the live value survives on a collMod. Creating a collection also creates its\n" +
				"database; Output.database_created says whether this step is what did.\n" +
				"The live KIND must be the one DECLARED: a plain collection where `timeseries` was\n" +
				"declared, a time-series one where it was not, or a VIEW either way, is refused. The\n" +
				"time-series form itself IS managed here — listCollections reports it as\n" +
				"type: \"timeseries\", and refusing that outright would fail the second apply of a step\n" +
				"that succeeded on the first. No dry-run preview.",
			Input: connectInput(own),
		},
		"absent": {
			Description: "Drop the collection. Idempotent through listCollections: one that is not there is a\n" +
				"no-op (changed=false).\n" +
				"\n" +
				"★ THIS DESTROYS THE COLLECTION'S DOCUMENTS AND ITS INDEXES. There is no confirmation\n" +
				"parameter — this artifact has none anywhere, and a flag an author always sets is not a\n" +
				"gate. What guards this is a scenario deciding when to run it. No dry-run preview.",
			Input: connectInput(collectionSubject()),
		},
	}
}
