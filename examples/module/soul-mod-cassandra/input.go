// The connection parameters every action declares, written once.
//
// [parseConnConfig] reads the same nine keys for every action of this artifact, so
// they are declared from one place rather than copied into nine state literals. The
// declaration is load-bearing at runtime — it is what [checkParamTypes] refuses a
// wrong-typed value against (NIM-778) — and a copy per state is a copy that can
// drift: the redis artifact carries the same nine keys nine times, and NIM-778 opens
// on exactly that asymmetry, one param made strict by hand while the one beside it
// was not.
//
// `keyspace` is NOT here. Only two objects take it, they mean different things by it
// (a session keyspace for `command`, the subject's keyspace for `table`), and a key
// declared everywhere would promise it to the three that refuse it.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// connectInput is the shared connection surface. Returned fresh each call: a
// [module.Input] is a map, and a shared one would let a state's own params leak into
// its neighbours.
func connectInput() module.Input {
	return module.Input{
		"hosts": {Type: module.List, Required: true,
			Description: "Contact points: a list of \"host\" or \"host:port\". The driver discovers the rest of the ring from them, so this is a list and not a single address — two or three nodes is enough, and one is a single point of failure for the connect only.",
		},
		"port": {Type: module.Int, Default: defaultPort,
			Description: "CQL native transport port, applied to any contact point given without one and to the peers the driver discovers. Default 9042.",
		},
		"username": {Type: module.String,
			Description: "Role to authenticate as (PasswordAuthenticator). This is WHO THE STEP CONNECTS AS, not the role a role-action manages (that is params.name).",
		},
		"password": {Type: module.String, Secret: true, Pattern: "^vault:.*",
			Description: "Password of the connecting role (vault-ref in operator-input; keeper resolves it before Apply). Goes into the connection credentials only — never into a statement, never into events, logs, traces or UI (masked, ADR-010).",
		},
		"tls": {Type: module.Bool, Default: false,
			Description: "Connect over TLS (client_encryption_options on the node). Default false (plaintext). Declared as a BOOLEAN and refused if written as a string: the insecure side of this parameter is the default, so a coerced value would send the password out in the clear (NIM-778).",
		},
		"tls_ca": {Type: module.String, Secret: true, Pattern: "^vault:.*",
			Description: "PEM CA certificate verifying the node certificates (RootCAs). Masked (secret). Resolved keeper-side from Vault during render.",
		},
		"tls_cert": {Type: module.String, Secret: true, Pattern: "^vault:.*",
			Description: "PEM client certificate for mTLS (optional, only together with tls_key). Masked (secret).",
		},
		"tls_key": {Type: module.String, Secret: true, Pattern: "^vault:.*",
			Description: "PEM client key for mTLS (optional, only together with tls_cert). Masked (secret); does not end up in events or errors.",
		},
		"tls_skip_verify": {Type: module.Bool, Default: false,
			Description: "EXPLICIT opt-out of node certificate verification. Default false (verification enabled — default secure).",
		},
	}
}

// withConnect merges an action's own params into the shared connection surface. A
// key an action declares itself wins, so an action can say something more specific
// about one of them without the shared text contradicting it.
func withConnect(own module.Input) module.Input {
	out := connectInput()
	for name, param := range own {
		out[name] = param
	}
	return out
}
