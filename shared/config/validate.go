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

	// OuterRegisters — register names declared OUTSIDE this file that are in
	// scope for it: the includer's registers (and its own ancestors'), threaded
	// down by [ExpandIncludes]. Only an included body ever gets a non-empty set;
	// a top-level file is validated against its own declarations alone.
	//
	// The asymmetry is deliberate. An included body reading a register the
	// includer declares is safe: a group-dropped include removes the READER, so
	// nothing can dangle. The reverse — a main file reading a register declared
	// inside a conditional include — stays rejected, because dropping that group
	// removes the DECLARATION and leaves a live reference behind (the invariant
	// render.Pipeline relies on; see include_expand_test.go).
	//
	// Seeded into the cross-reference set only, never into the address space:
	// a name colliding across files is a duplicate_task_address, and that is
	// validateFlatTaskAddresses' verdict to give, on the flat expanded plan.
	OuterRegisters map[string]bool

	// DestinyTasks marks the file being loaded as a DESTINY's `tasks/main.yml`
	// rather than a scenario's included body — the two share
	// [LoadDestinyTasksFromBytes] and are otherwise indistinguishable at that
	// layer, since both are a bare top-level task sequence.
	//
	// One rule needs the distinction (NIM-749): a destiny task is Soul-side by
	// construction — it is rendered per host and dispatched to a Soul — so a
	// keeper-side module address in one can never execute
	// (`keeper_module_in_destiny`). In a scenario the same address is the
	// ordinary, correct way to write a keeper-side step.
	//
	// Defaults to false, which is the safe direction: the scenario and include
	// paths must NOT raise that diagnostic, and a destiny loader that forgets the
	// flag loses one check rather than rejecting valid files.
	DestinyTasks bool
}
