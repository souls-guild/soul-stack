package config

// ValidateOptions controls config-validator behavior.
//
// MVP is a single flag, reserved for reachability checks at `keeper`/`soul`
// startup (e.g. `vault.addr` reachable): those validators will live in
// `shared/config/runtime/` and activate only under `AllowNetworkCalls: true`
// (default `false`).
type ValidateOptions struct {
	// AllowNetworkCalls reserved for future semantic-validate phases
	// (Vault reachability, Postgres ping). Not used in M0.thin.
	AllowNetworkCalls bool
}
