// The `index` object — one index on one collection (index.go).
//
// ★ WHY THIS IS AN OBJECT AND NOT AN `indexes:` PARAMETER OF `collection.present`
//
// An index is its own subject with its own lifecycle, and folding a list of them
// into the collection action costs four things:
//
//  1. An index CANNOT BE MODIFIED. Changing its key means dropping it and building
//     a new one — long, IO-heavy, and the queries that used it run unindexed
//     meanwhile. As a nested list that rebuild would be buried inside a step whose
//     subject is the collection; as its own action it is a refusal the operator
//     answers deliberately.
//  2. A list of sub-subjects inside one action re-admits at the PARAMETER level
//     exactly what the address discipline removed from the address — the same
//     argument that took `params.state` off `user` (NIM-769) and `params.action`
//     off `cluster` (NIM-766).
//  3. `loop:` over indexes becomes impossible, and one failing index reds a task
//     that has already created the collection.
//  4. It forces a question with no good answer: are indexes NOT in the list to be
//     dropped? Yes silently destroys work; no makes the declaration not a
//     declaration. A separate object never raises it.
//
// The key is a LIST of { field, order } rather than a map, because an index key is
// ordered and a `structpb` map's order is not preserved — see index.go.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// index binds the object's actions to the shared driver.
func (m *MongoModule) index() *object {
	return &object{
		impl: m,
		name: "index",
		decl: indexStates(),
		actions: map[string]action{
			"present": {validate: validateIndexPresent, apply: (*MongoModule).applyIndexPresent},
			"absent":  {validate: validateIndexAbsent, apply: (*MongoModule).applyIndexAbsent},
		},
	}
}

// indexDef is the object's entry in the artifact's bundle.
func indexDef(m *MongoModule) module.Def {
	return module.Def{
		Name:         "index",
		Description:  "One index on one MongoDB collection, created through createIndexes and reconciled through collMod.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "mongod"}},
		Impl:         m.index(),
		States:       indexStates(),
	}
}

// indexSubject is the addressing triple both actions take.
func indexSubject() module.Input {
	return module.Input{
		"database": {Type: module.String, Required: true,
			Description: "The database the collection lives in.",
		},
		"collection": {Type: module.String, Required: true,
			Description: "The collection the index is on.",
		},
		"name": {Type: module.String, Required: true,
			Description: "The index name — how an index is ADDRESSED, by createIndexes and dropIndexes alike. `_id_` is refused on both actions: mongod creates and maintains that one itself and will not drop it.",
		},
	}
}

// indexStates declares the parameters of every action this object serves.
func indexStates() map[string]module.State {
	own := indexSubject()
	own["keys"] = module.Param{Type: module.List, Required: true,
		Description: "The index key, as an ORDERED list of { field, order }. ★ A LIST and not a map, because an index on { user_id: 1, created_at: -1 } is a DIFFERENT index from one on { created_at: -1, user_id: 1 }, and a YAML map reaches a plugin as an unordered structure — declaring the key as a map would build a different index from one run to the next. `field` is the document field path; `order` is 1 (ascending, the default), -1 (descending), or an index type as a string (\"2dsphere\", \"2d\", \"hashed\", …). A value that is neither is REFUSED, not defaulted: an ascending index where a descending one was meant is a query plan nobody notices until it is slow. ★ \"text\" is REFUSED: mongod stores a text index under a rewritten key ({_fts, _ftsx}, the fields moved to `weights`), so this object could never recognise one as converged — use mongo.command.run { createIndexes: … } for those.",
	}
	own["unique"] = module.Param{Type: module.Bool,
		Description: "IMMUTABLE. Reject a write that would duplicate the key. Cannot be turned on or off on a live index.",
	}
	own["sparse"] = module.Param{Type: module.Bool,
		Description: "IMMUTABLE. Index only the documents that have the field. Cannot be changed on a live index.",
	}
	own["partial_filter_expression"] = module.Param{Type: module.Map,
		Description: "IMMUTABLE. Index only the documents matching this predicate, e.g. { status: { $eq: \"active\" } }. Cannot be changed on a live index.",
	}
	own["collation"] = module.Param{Type: module.Map,
		Description: "IMMUTABLE. Collation of the index, e.g. { locale: \"en\", strength: 2 }. Compared on the keys you declare only, because mongod stores it with its own defaults filled in.",
	}
	own["expire_after_seconds"] = module.Param{Type: module.Int,
		Description: "TTL: seconds a document survives past the date in its indexed field. This is where a TTL belongs — on the INDEX, not on the collection. 0 means expire at the indexed date itself. CHANGEABLE in place (collMod) on an index that ALREADY has a TTL; ADDING one to an index that has none is not an operation mongod offers, so that is refused like any other rebuild.",
	}
	own["hidden"] = module.Param{Type: module.Bool,
		Description: "MUTABLE (collMod). Hide the index from the query planner while keeping it maintained — how an index is tested for removal without paying to rebuild it if the answer is no.",
	}

	return map[string]module.State{
		"present": {
			Description: "Create the index, or change what CAN be changed on a live one.\n" +
				"Idempotent through listIndexes: an index already matching the declaration is a no-op\n" +
				"(changed=false).\n" +
				"\n" +
				"★ AN INDEX CANNOT BE MODIFIED. A declared key that differs from the live one, or a\n" +
				"declared unique / sparse / partial_filter_expression / collation that differs,\n" +
				"is a FAILURE naming the field — applying it would\n" +
				"mean dropping the index and rebuilding it, and during that build the queries that\n" +
				"relied on it have no index at all. Drop it deliberately with mongo.index.absent if\n" +
				"that is what was meant.\n" +
				"expire_after_seconds and hidden ARE changed in place, through collMod.\n" +
				"\n" +
				"An option that is NOT declared is not compared and not sent. No dry-run preview.",
			Input: connectInput(own),
		},
		"absent": {
			Description: "Drop the index via dropIndexes. Idempotent through listIndexes: an index that is not\n" +
				"there — on a collection that may not be there either — is a no-op (changed=false).\n" +
				"\n" +
				"The documents are untouched; what goes is the index, and with it the query plans that\n" +
				"used it. `_id_` is refused. No dry-run preview.",
			Input: connectInput(indexSubject()),
		},
	}
}
