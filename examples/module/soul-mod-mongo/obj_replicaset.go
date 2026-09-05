// The `replicaset` object — a MongoDB replica set: brought into being, grown,
// shrunk, and reconfigured (replicaset.go).
//
// Four actions rather than one state carrying a verb, for the reason NIM-765/NIM-769
// give and for a second one specific to this object: the four differ in what they
// are ALLOWED to do, and that difference is the safety property. `initiated` is
// additive and refuses to touch an existing member; `reconfigured` is the one that
// may, and can therefore force an election. Folding them into `params.action` would
// put that distinction back into a value, where a template could compute it.
//
// Every action opens its own connection (applyOwn): `initiated` needs the
// localhost-exception bootstrap path, and all four may have to hop to the primary,
// which replSetReconfig requires and `params.addr` need not be.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// replicaset binds the object's actions to the shared driver.
func (m *MongoModule) replicaset() *object {
	return &object{
		impl: m,
		name: "replicaset",
		decl: replicasetStates(),
		actions: map[string]action{
			"initiated":      {validate: validateRSInitiated, applyOwn: (*MongoModule).applyRSInitiated},
			"member-added":   {validate: validateRSMemberAdded, applyOwn: (*MongoModule).applyRSMemberAdded},
			"member-removed": {validate: validateRSMemberRemoved, applyOwn: (*MongoModule).applyRSMemberRemoved},
			"reconfigured":   {validate: validateRSReconfigured, applyOwn: (*MongoModule).applyRSReconfigured},
		},
	}
}

// replicasetDef is the object's entry in the artifact's bundle.
func replicasetDef(m *MongoModule) module.Def {
	return module.Def{
		Name:         "replicaset",
		Description:  "A MongoDB replica set: initiated from scratch, completed to the declared membership, grown, shrunk and reconfigured.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "mongod"}},
		Impl:         m.replicaset(),
		States:       replicasetStates(),
	}
}

// memberSpecDescription is the shape of one member, written once because three
// actions take it and a member that means something different in one of them would
// be a trap.
const memberSpecDescription = "Member attributes: `host` (REQUIRED, host:port as the OTHER MEMBERS see it — this is what goes into the replica-set config), " +
	"`addr` (optional, host:port to DIAL from this host when it differs from `host` — a set whose members address each other on a private network), " +
	"`priority` (number, default 1; 0 means it can never be elected), `votes` (0 or 1, default 1), `arbiter_only`, `hidden`, " +
	"`build_indexes` (default true), `secondary_delay_secs`, `tags` (map of string to string). " +
	"An attribute that is NOT written is not touched: mongod's own default applies on a member being created, and the live value survives on one being changed."

// replicasetStates declares the parameters of every action this object serves.
// It is lifted out of [replicasetDef] because [object] reads it too: the declared
// type of a parameter is what Validate and Apply refuse a wrong-typed value against
// (NIM-800), and a second copy of it would be a second answer.
func replicasetStates() map[string]module.State {
	return map[string]module.State{
		"initiated": {
			Description: "Bring the replica set to the declared membership, deciding what to do by ASKING the\n" +
				"instance rather than by being told: replSetGetConfig says NotYetInitialized ->\n" +
				"replSetInitiate; says the set is already exactly this -> no-op (changed=false); says it\n" +
				"holds SOME of the declared members -> add only the missing ones by reconfig, on top of\n" +
				"the LIVE config document, with version+1.\n" +
				"\n" +
				"★ ADDITIVE ONLY, and that is the safety property. A member the live config holds and\n" +
				"params.members does not is REFUSED, not dropped (use member-removed) — a silent drop is\n" +
				"how a set loses its majority. An existing member whose attributes differ is REFUSED, not\n" +
				"rewritten (use reconfigured) — a priority change can force an election, and this step\n" +
				"reads as assembly.\n" +
				"\n" +
				"★ A mongod started WITHOUT replication.replSetName fails here by name\n" +
				"(NoReplicationEnabled), because the plugin cannot fix a server command line and\n" +
				"initiating such an instance is impossible, not merely pending.\n" +
				"\n" +
				"Runs over the LOCALHOST-EXCEPTION when auth is not yet possible: with\n" +
				"security.authorization enabled a set is initiated BEFORE the first admin exists.\n" +
				"Ends by waiting for a PRIMARY; not getting one is a failure. No dry-run preview.",
			Input: connectInput(module.Input{
				"name": {Type: module.String, Required: true,
					Description: "The replica set name — `_id` of the config, and it must equal the mongod's own replication.replSetName. A LIVE set whose name differs is refused rather than renamed: renaming a replica set is not an operation.",
				},
				"members": {Type: module.Map, Required: true,
					Description: "The members of the set: a map of stable key (SID/name) -> member spec. Keys are SORTED, and that determines the `_id` assigned to each member on a fresh initiate — so the same input yields the same config. " + memberSpecDescription,
				},
				"wait_primary_seconds": waitPrimaryParam(),
				"primary_addr":         primaryAddrParam(),
			}),
		},
		"member-added": {
			Description: "Join ONE member to a formed set (day-2): the live config plus one member, version+1,\n" +
				"sent to the PRIMARY. Not a single existing entry is touched, and no `_id` is reused —\n" +
				"a new member takes max(_id)+1, because handing a new host the identity of an old one\n" +
				"is a way to break a set.\n" +
				"Idempotent: a host already in the config is a no-op (changed=false).\n" +
				"Ends by waiting for a PRIMARY; not getting one is a failure. No dry-run preview.",
			Input: connectInput(module.Input{
				"member": {Type: module.Map, Required: true,
					Description: "The member being joined. " + memberSpecDescription,
				},
				"wait_primary_seconds": waitPrimaryParam(),
				"primary_addr":         primaryAddrParam(),
			}),
		},
		"member-removed": {
			Description: "Evict ONE member from a formed set (day-2): the live config minus that member,\n" +
				"version+1, sent to the PRIMARY.\n" +
				"Idempotent: a host the config does not hold is a no-op (changed=false).\n" +
				"\n" +
				"★ Two refusals rather than two surprises. Removing the CURRENT PRIMARY by reconfig\n" +
				"forces an election, so it is refused and the operator is told to step it down first\n" +
				"(mongo.command.run with { replSetStepDown: <seconds> }). A removal that would leave no\n" +
				"member with votes > 0 AND priority > 0 is refused, because the result could not elect\n" +
				"a primary and so could not take a write.\n" +
				"No dry-run preview.",
			Input: connectInput(module.Input{
				"host": {Type: module.String, Required: true,
					Description: "The `members[].host` being evicted, as it appears in the replica-set config (host:port; a host written without a port is compared as :27017, which is how mongod stores it).",
				},
				"wait_primary_seconds": waitPrimaryParam(),
				"primary_addr":         primaryAddrParam(),
			}),
		},
		"reconfigured": {
			Description: "Change attributes of members that ALREADY exist — the deliberate, dangerous action,\n" +
				"separate for that reason: a priority or votes change can force an election and break\n" +
				"writes in flight, and `initiated` refuses this work and names this action instead.\n" +
				"\n" +
				"★ A PATCH, not a replacement. Only the attributes written in params.members are laid\n" +
				"over the live member documents; every other field of each member, and every member not\n" +
				"named, ride through untouched — as do `settings`, `protocolVersion` and anything else\n" +
				"the live config carries, because the document sent is the live one with version+1 and\n" +
				"never one rebuilt from params.\n" +
				"Idempotent: a run where every declared attribute already matches is a no-op\n" +
				"(changed=false). A member that is NOT in the set is refused (use member-added).\n" +
				"Ends by waiting for a PRIMARY; not getting one is a failure. No dry-run preview.",
			Input: connectInput(module.Input{
				"members": {Type: module.Map, Required: true,
					Description: "The members to change: a map of stable key -> member spec, each of which must already be in the set. Only the attributes present here are applied. " + memberSpecDescription,
				},
				"wait_primary_seconds": waitPrimaryParam(),
				"primary_addr":         primaryAddrParam(),
			}),
		},
	}
}
