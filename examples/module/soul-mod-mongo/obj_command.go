// The `command` object — an arbitrary MongoDB command, run as given.
//
// Non-stateful, so level 3 takes the verb form (`mongo.command.run`), exactly as
// `redis.command.run` and `core.exec.run` do. It takes ONE verb and will keep
// taking one: the discipline that removed `pinged` and `user` from level 3 says two
// operations are two objects, so a second thing to do to a mongod becomes its own
// object rather than a second word here (ADR-020 amendment 2026-09-02).
//
// The name grants nothing. The Errand allow-list decides for a plugin by the
// `ErrandReadSafe` marker, which this artifact deliberately does not implement, so
// `command` is default-denied there like every other object of this artifact.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// command binds the object's single action to the shared driver.
func (m *MongoModule) command() *object {
	return &object{
		impl: m,
		name: "command",
		decl: commandStates(),
		actions: map[string]action{
			"run": {validate: validateCommand, apply: (*MongoModule).applyCommand},
		},
	}
}

// commandDef is the object's entry in the artifact's bundle.
func commandDef(m *MongoModule) module.Def {
	return module.Def{
		Name:         "command",
		Description:  "An arbitrary MongoDB command, run as given against one mongod.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "mongod"}},
		Impl:         m.command(),
		States:       commandStates(),
	}
}

// commandStates declares the parameters of every action this object serves.
func commandStates() map[string]module.State {
	return map[string]module.State{
		"run": {
			Description: "Run a raw command against MongoDB via db.runCommand (imperative verb-action).\n" +
				"Defaults to changed=false (probe); the operator is responsible for idempotency.\n" +
				"Output.ok — success flag of the response. No dry-run preview.",
			Input: module.Input{
				"addr": {Type: module.String, Required: true,
					Description: "mongod address: host:port (usually 127.0.0.1:27017).",
				},
				"auth_db": {Type: module.String,
					Description: "authenticationDatabase (default admin).",
				},
				"changed": {Type: module.Bool, Default: false,
					Description: "Mark the result as changed=true (for commands that actually mutate state). Defaults to false (probe semantics).",
				},
				"command": {Type: module.Map, Required: true,
					Description: "bson document of the command (first/only key — the command name), e.g. { serverStatus: 1 } or { collStats: \"events\" }. For PILOT — single-field commands.",
				},
				"db": {Type: module.String,
					Description: "Target DB for runCommand (default admin).",
				},
				"password": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "MongoDB password (vault ref; keeper resolves before Apply). Masked — is not passed into the command arguments (goes into the connection).",
				},
				"tls": {Type: module.Bool, Default: false,
					Description: "Connect over TLS. Defaults to false. PILOT: mongo in plain — not set.",
				},
				"tls_ca": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "CA PEM for verifying the server certificate. Masked (secret).",
				},
				"tls_cert": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "Client-cert PEM for mTLS (optional, with tls_key). Masked (secret).",
				},
				"tls_key": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "Client-key PEM for mTLS (optional, with tls_cert). Masked (secret).",
				},
				"tls_skip_verify": {Type: module.Bool, Default: false,
					Description: "EXPLICIT opt-out of certificate verification. Defaults to false (default secure).",
				},
				"username": {Type: module.String,
					Description: "ACL username for AUTH.",
				},
			},
		},
	}
}
