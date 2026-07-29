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

	// ModuleManifests resolves the manifest of a PLUGIN module so its `params:`
	// can be checked statically, the way `core.*` already is (NIM-228). Core
	// needs no resolver: its manifests are embedded, so metadata and parser
	// always ship together.
	//
	// Optional, and its absence is not silent — every plugin module address the
	// resolver cannot answer for yields a `plugin_params_unchecked` hint, so
	// "checked and clean" stops being indistinguishable from "never checked".
	// See [ModuleManifestResolver].
	ModuleManifests ModuleManifestResolver
}
