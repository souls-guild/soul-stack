// The `user` object — one MongoDB user of a live mongod, created or removed
// (user.go).
//
// MongoDB users live in `admin.system.users`, not in a config file (unlike the
// redis `users.acl` a destiny renders), so the subject reaches the instance
// directly through createUser / dropUser and nothing here renders or re-reads a
// file.
//
// `present` and `absent` are two ACTIONS, where they used to be one state carrying
// `params.state`. The verb belongs in the address (ADR-020 amendment 2026-09-02,
// NIM-765), and splitting it is what lets each half declare only what it reads:
// `roles` and `user_password` are the present half's and are now refused on the
// absent one instead of being promised to it.
package main

import "github.com/souls-guild/soul-stack/sdk/module"

// user binds the object's actions to the shared driver. Both open their own
// connection (applyOwn), because the auth path is decided from the live state —
// see the localhost-exception in user.go.
func (m *MongoModule) user() *object {
	return &object{
		impl: m,
		name: "user",
		decl: userStates(),
		actions: map[string]action{
			"present": {validate: validateUserPresent, applyOwn: (*MongoModule).applyUserPresent},
			"absent":  {validate: validateUserAbsent, applyOwn: (*MongoModule).applyUserAbsent},
		},
	}
}

// userDef is the object's entry in the artifact's bundle.
func userDef(m *MongoModule) module.Def {
	return module.Def{
		Name:         "user",
		Description:  "One MongoDB user of a live mongod, reconciled through createUser / dropUser.",
		Side:         module.SideSoul,
		Capabilities: []module.Capability{module.NetworkOutbound},
		SideEffects:  []module.SideEffect{{Service: "mongod"}},
		Impl:         m.user(),
		States:       userStates(),
	}
}

// userStates declares the parameters of every action this object serves.
func userStates() map[string]module.State {
	return map[string]module.State{
		"present": {
			Description: "Create a MongoDB user via createUser (go-mongo-driver). Idempotent via\n" +
				"usersInfo: a user that already exists is a no-op (changed=false) — changing the\n" +
				"password or roles of an existing user is day-2, out of PILOT scope. ★ the first\n" +
				"admin is created via the localhost-exception (no-auth loopback connection, while\n" +
				"the admin DB is empty). No dry-run preview.",
			Input: module.Input{
				"addr": {Type: module.String, Required: true,
					Description: "mongod address: host:port (usually 127.0.0.1:27017).",
				},
				"auth_db": {Type: module.String,
					Description: "authenticationDatabase of the connection (default admin).",
				},
				"database": {Type: module.String,
					Description: "The DB in which the user is created (login/roles context). Default admin (system users). Roles without an explicit db inherit this DB.",
				},
				"name": {Type: module.String, Required: true,
					Description: "MongoDB username to create. This is the SUBJECT of the step, not the user to authenticate as (that is `username`).",
				},
				"password": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "Password of the ADMIN CONNECTION (the user is created under username; vault ref, keeper resolves before Apply). Goes into the connection credentials; does not end up in events/logs. ★ This is NOT the password of the user being created (see user_password) — except for bootstrapping the first admin, where the admin creates ITSELF (password serves both roles when user_password is not set).",
				},
				"roles": {Type: module.List,
					Description: "User roles (array of {role, db}). Each element — {role: \"readWrite\", db: \"appdb\"}. db with no value inherits database. Must be non-empty (a user without roles is meaningless).",
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
				"user_password": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "Password of the CREATED user (pwd of the createUser document; vault ref, keeper resolves before Apply). Separate from password (admin connection auth): for operator users the connection is under the admin password, while createUser uses this password. Not set → falls back to password (bootstrap of the first admin: the admin creates itself under the same password). Masked.",
				},
				"username": {Type: module.String,
					Description: "ACL username for the AUTH connection (the admin under which the user is created). During bootstrap of the first admin, auth is not yet possible → localhost-exception no-auth.",
				},
			},
		},
		"absent": {
			Description: "Remove a MongoDB user via dropUser (go-mongo-driver). Idempotent via usersInfo:\n" +
				"a user the instance does not have is a no-op (changed=false).\n" +
				"\n" +
				"The localhost-exception fallback of `present` does NOT apply here: removing a\n" +
				"user requires privileges, so an auth failure is a failure and not a bootstrap.\n" +
				"No dry-run preview.",
			Input: module.Input{
				"addr": {Type: module.String, Required: true,
					Description: "mongod address: host:port (usually 127.0.0.1:27017).",
				},
				"auth_db": {Type: module.String,
					Description: "authenticationDatabase of the connection (default admin).",
				},
				"database": {Type: module.String,
					Description: "The DB the user lives in (default admin). dropUser runs in this DB's context.",
				},
				"name": {Type: module.String, Required: true,
					Description: "MongoDB username to remove. This is the SUBJECT of the step, not the user to authenticate as (that is `username`).",
				},
				"password": {Type: module.String, Secret: true, Pattern: "^vault:.*",
					Description: "Password of the ADMIN CONNECTION under which the user is removed (vault ref, keeper resolves before Apply). Goes into the connection credentials; does not end up in events/logs.",
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
					Description: "ACL username for the AUTH connection (the admin under which the user is removed).",
				},
			},
		},
	}
}
