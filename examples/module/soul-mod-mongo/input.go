// The connection parameters every action of this artifact reads, declared once.
//
// [parseConnConfig] (impl.go) and [parseTLS] (tls.go) read the same nine keys for
// every action there is, so every action must DECLARE all nine: a key a state omits
// has no declaration, and since NIM-204 a legitimate call carrying it fails with
// module.unknown_param (ADR-0076(t)). That is the NIM-206 hole, and it was found by
// four states out of eleven having quietly dropped the TLS block.
//
// The PILOT three (`command`, `instance`, `user`) spell the nine out inline, because
// several of their descriptions are action-specific — `user.present` explains at
// length that `password` is the ADMIN connection's and not the created user's. Their
// declarations are the published schema document and are not rewritten here. The
// objects added by NIM-805 have no such per-action wording and take the block from
// [connectInput] instead, so a tenth connection parameter cannot be added to the
// parser and forgotten on five objects at once.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// connectInput returns the shared connection block merged with an action's own
// parameters. The action's own win on a name collision, which is how an action that
// needs different wording for a shared key keeps it.
//
// A fresh map every call: the returned value ends up in a [module.State] that is
// handed to both the schema renderer and [object.decl], and a shared map would let
// one action's declaration reach another's.
func connectInput(own module.Input) module.Input {
	out := make(module.Input, len(own)+9)

	out["addr"] = module.Param{Type: module.String, Required: true,
		Description: "mongod address: host:port (usually 127.0.0.1:27017).",
	}
	out["auth_db"] = module.Param{Type: module.String,
		Description: "authenticationDatabase of the connection (default admin).",
	}
	out["username"] = module.Param{Type: module.String,
		Description: "MongoDB username the step authenticates as.",
	}
	out["password"] = module.Param{Type: module.String, Secret: true, Pattern: "^vault:.*",
		Description: "Password of the connection (vault ref; keeper resolves it before Apply). Goes into the connection credentials ONLY — never into a command argument, where `ps` on the host would show it. Masked: does not reach events/logs/errors.",
	}
	out["tls"] = module.Param{Type: module.Bool, Default: false,
		Description: "Connect over TLS. Defaults to false (plaintext). A value of the wrong type is REFUSED, not coerced: `tls: \"true\"` written as a string used to read as false and send the password out in the clear (NIM-778/NIM-800).",
	}
	out["tls_ca"] = module.Param{Type: module.String, Secret: true, Pattern: "^vault:.*",
		Description: "CA PEM for verifying the server certificate (RootCAs). Masked (secret).",
	}
	out["tls_cert"] = module.Param{Type: module.String, Secret: true, Pattern: "^vault:.*",
		Description: "Client-cert PEM for mTLS (optional, only together with tls_key). Masked (secret).",
	}
	out["tls_key"] = module.Param{Type: module.String, Secret: true, Pattern: "^vault:.*",
		Description: "Client-key PEM for mTLS (optional, only together with tls_cert). Masked (secret); does not reach events/errors.",
	}
	out["tls_skip_verify"] = module.Param{Type: module.Bool, Default: false,
		Description: "EXPLICIT opt-out of server certificate verification. Defaults to false (verification enabled — default secure).",
	}

	for name, p := range own {
		out[name] = p
	}
	return out
}

// The replica-set connection extras, shared by the four `replicaset` actions.
//
// `wait_primary_seconds` is on every one of them because every one of them ends by
// waiting for a primary: a set with none takes no writes, so a step that returned
// success without one would report an unusable set as reconciled.
//
// `primary_addr` exists because replSetReconfig must run on the primary and
// replSetGetStatus names it by its CONFIG host, which is routable between members
// and not necessarily from the host this plugin runs on.
func waitPrimaryParam() module.Param {
	return module.Param{Type: module.Int, Default: defaultWaitPrimarySeconds,
		Description: "How long to wait for a PRIMARY to be elected, in seconds (default 60; 0 means read once and do not poll). Running out is a FAILURE, not a warning: a replica set without a primary accepts no writes, so reporting the step as reconciled would be false.",
	}
}

func primaryAddrParam() module.Param {
	return module.Param{Type: module.String,
		Description: "Where to reach the PRIMARY from this host, if its replica-set `host` is not routable here. Default: the host replSetGetStatus names, then the matching `addr` of a declared member. replSetReconfig only runs on the primary, so this is how a set whose members address each other on a private network is managed from outside it.",
	}
}
