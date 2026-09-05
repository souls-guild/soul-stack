// The `instance` object — a live mongod as a whole: is it answering.
//
// `pinged` is a read probe (changed=false by design, impl.go). The PILOT slice
// gives this object one action; a second thing to ask a live instance about
// (`role-probed` on a replica set, say) is a second action here, not a second
// object.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// instance binds the object's actions to the shared driver. The table is the
// object's boundary: nothing else in this artifact is reachable through it.
func (m *MongoModule) instance() *object {
	return &object{
		impl: m,
		name: "instance",
		decl: instanceStates(),
		actions: map[string]action{
			"pinged": {validate: validatePinged, apply: (*MongoModule).applyPinged},
		},
	}
}

// instanceDef is the object's entry in the artifact's bundle: what it declares to
// an operator, plus the implementation that serves it.
func instanceDef(m *MongoModule) module.Def {
	return module.Def{
		Name:         "instance",
		Description:  "A live mongod: the health probe that gates everything a scenario does after it.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "mongod"}},
		Impl:         m.instance(),
		States:       instanceStates(),
	}
}

// instanceStates declares the parameters of every action this object serves.
func instanceStates() map[string]module.State {
	return map[string]module.State{
		"pinged": {
			Description: "MongoDB health probe via go-mongo-driver Ping (primary). Read-only,\n" +
				"changed=false by design (probe, not a mutation). Output.ok — condition for the\n" +
				"health gate (until: register.self.ok == true). No dry-run preview.",
			Input: module.Input{
				"addr": {Type: module.String, Required: true,
					Description: "mongod address: host:port (usually 127.0.0.1:27017).",
				},
				"auth_db": {Type: module.String,
					Description: "authenticationDatabase (default admin).",
				},
				"password": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "MongoDB password (vault ref in operator-input; keeper resolves before Apply). Masked — does not end up in events/logs. Not passed to Ping (goes into the connection).",
				},
				"tls": {Type: module.Bool, Default: false,
					Description: "Connect to mongod over TLS. Defaults to false (plaintext). PILOT: mongo in plain mode — not set.",
				},
				"tls_ca": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "PEM of the CA certificate for verifying the server certificate (RootCAs). Masked (secret). Resolved keeper-side from Vault (render phase).",
				},
				"tls_cert": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "PEM of the client certificate for mTLS (optional, only together with tls_key). Masked (secret).",
				},
				"tls_key": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "PEM of the client key for mTLS (optional, only together with tls_cert). Masked (secret); does not end up in events/errors.",
				},
				"tls_skip_verify": {Type: module.Bool, Default: false,
					Description: "EXPLICIT opt-out of server certificate verification. Defaults to false (verification enabled — default secure).",
				},
				"username": {Type: module.String,
					Description: "ACL username for AUTH (if not anonymous).",
				},
			},
		},
	}
}
