// The `instance` object — a live Cassandra node: is it serving, and what is it.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// instance binds the object's action to the shared driver. The table is the object's
// boundary: nothing else in this artifact is reachable through it.
func (m *CassandraModule) instance() *object {
	return &object{
		impl: m,
		name: "instance",
		decl: instanceStates(),
		actions: map[string]action{
			"pinged": {validate: validatePinged, apply: (*CassandraModule).applyPinged},
		},
	}
}

// instanceDef is the object's entry in the artifact's bundle.
func instanceDef(m *CassandraModule) module.Def {
	return module.Def{
		Name:         "instance",
		Description:  "A live Cassandra node: liveness and the version, cluster, datacenter and rack it reports for itself.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "cassandra"}},
		Impl:         m.instance(),
		States:       instanceStates(),
	}
}

// instanceStates declares the parameters of every action this object serves. It is
// lifted out of [instanceDef] because [object] reads it too: the declared type of a
// parameter is what Validate and Apply refuse a wrong-typed value against (NIM-778),
// and a second copy of it would be a second answer.
func instanceStates() map[string]module.State {
	return map[string]module.State{
		"pinged": {
			Description: "Health-probe a Cassandra node over the native CQL protocol (no cqlsh, no shell):\n" +
				"open a session and read system.local. Opening the session already proves TCP,\n" +
				"the CQL handshake and — with credentials — authentication; the query proves the\n" +
				"node is SERVING and not merely accepting connections.\n" +
				"Read-only, changed=false by design (a probe, not a mutation). Output carries\n" +
				"ok plus release_version / cluster_name / data_center / rack / host_id, so a\n" +
				"health gate reads register.self.ok and a version gate reads\n" +
				"register.self.release_version.\n" +
				"No dry-run preview (the plugin does not implement PlanReadSafe).",
			Input: withConnect(nil),
		},
	}
}
