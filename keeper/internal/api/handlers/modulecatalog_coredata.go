package handlers

// Doc data for core modules, for the module-catalog (`GET /v1/modules`).
//
// Why a table and not registry introspection: keeper does NOT import
// soul/internal/coremod (Soul isolation per ADR-011 — soul-side core is statically
// built into the `soul` binary; the compiler does not expose it to keeper). The module
// implementations themselves carry neither a description nor an input schema in code
// (params are validated imperatively inside Apply/Validate, not declaratively). So the
// core catalog publishes what is ACTUALLY available as a static fact: name, kind,
// description, allowed states and the errand-safe marker. The full input schema is NOT
// formalized (a new entity — see the report; stop-rule), core-module params are empty here.
//
// Source of truth for names/states — the Validate switches of soul-side core modules
// (soul/internal/coremod/<m>/<m>.go) and docs/module/core/<m>/. Source of truth
// for errand-safe — soul/internal/runtime/errandrunner/whitelist.go (the hard
// list core.cmd.shell / core.exec.run) + the exact read-safe mixed-module state
// core.http.probe (core.http.request is deliberately excluded). Any divergence
// from the state list in the implementation must
// be synced by hand — this is doc-data, not introspection.

// coreModuleDoc — a static catalog entry for one core module.
type coreModuleDoc struct {
	// Name — the canonical module name without the state suffix (`core.cmd`).
	Name string
	// Description — human-readable description.
	Description string
	// States — the module's allowed state/verb suffixes (full address —
	// `<Name>.<state>`).
	States []string
	// ErrandSafeStates — the subset of States safe for ad-hoc invocation via the
	// Errand pull contour (ADR-033). Empty = the module is not errand-safe in any
	// state.
	ErrandSafeStates []string
}

// coreModuleDocs — the hand-written catalog served by GET /v1/modules for the
// `core.*` namespace. Keeper-side entries are published unconditionally even where
// the registry registers them conditionally (core.choir needs Deps.ChoirStore).
// Order is alphabetical by Name for deterministic output.
//
// ⚠ It does NOT match the two registries: 22 entries against their 28. Missing are
// core.directory / core.noop / core.module on the Soul side and core.cert /
// core.ssh / core.state on the keeper side, so an operator asking the platform what
// exists is not told about them. The test beside this file compares len(resp) with
// len(coreModuleDocs), i.e. the table against itself, which is why nothing caught
// it. Tracked as NIM-890 with the derived guard that would have; do not add an entry
// here without adding that guard, or the next omission is as silent as these.
var coreModuleDocs = []coreModuleDoc{
	// --- soul-side (ADR-015) ---
	{
		Name:        "core.archive",
		Description: "Extract an archive (tar/tar.gz/tar.bz2/zip) into the destination directory.",
		States:      []string{"extracted"},
	},
	{
		Name:        "core.augur",
		Description: "Read-probe of live access to an external system via the Augur broker (verb fetch, changed=false).",
		States:      []string{"fetch"},
	},
	{
		Name:             "core.cmd",
		Description:      "Run an arbitrary shell command (imperative verb shell).",
		States:           []string{"shell"},
		ErrandSafeStates: []string{"shell"},
	},
	{
		Name:        "core.cron",
		Description: "Manage a cron job (present/absent).",
		States:      []string{"present", "absent"},
	},
	{
		Name:             "core.exec",
		Description:      "Run a command without a shell wrapper (imperative verb run).",
		States:           []string{"run"},
		ErrandSafeStates: []string{"run"},
	},
	{
		Name:        "core.file",
		Description: "Manage a file: present (inline-content), absent, rendered (text/template render).",
		States:      []string{"present", "absent", "rendered"},
	},
	{
		Name:        "core.firewall",
		Description: "Manage a firewall rule (present/absent).",
		States:      []string{"present", "absent"},
	},
	{
		Name:        "core.git",
		Description: "Clone/update a git repository into a directory (state cloned).",
		States:      []string{"cloned"},
	},
	{
		Name:        "core.group",
		Description: "Manage a system group (present/absent).",
		States:      []string{"present", "absent"},
	},
	{
		Name:             "core.http",
		Description:      "HTTP endpoint probe (GET/HEAD, changed=false) and explicit mutating request (POST/PUT/PATCH/DELETE, changed=true on success).",
		States:           []string{"probe", "request"},
		ErrandSafeStates: []string{"probe"},
	},
	{
		Name:        "core.line",
		Description: "Line-by-line in-place file edit (present/absent, regex-match).",
		States:      []string{"present", "absent"},
	},
	{
		Name:        "core.mount",
		Description: "Manage a mount point (present/absent/mounted/unmounted).",
		States:      []string{"present", "absent", "mounted", "unmounted"},
	},
	{
		Name:        "core.pkg",
		Description: "Manage a system package (installed/absent/latest).",
		States:      []string{"installed", "absent", "latest"},
	},
	{
		Name:        "core.repo",
		Description: "Manage a package repository (present/absent).",
		States:      []string{"present", "absent"},
	},
	{
		Name:        "core.service",
		Description: "Manage an init-system service (running/stopped/restarted/enabled).",
		States:      []string{"running", "stopped", "restarted", "enabled"},
	},
	{
		Name:        "core.sysctl",
		Description: "Manage a sysctl kernel parameter (state present).",
		States:      []string{"present"},
	},
	{
		Name:        "core.url",
		Description: "Download a file by URL with checksum verification (state fetched).",
		States:      []string{"fetched"},
	},
	{
		Name:        "core.user",
		Description: "Manage a system user (present/absent).",
		States:      []string{"present", "absent"},
	},

	// --- keeper-side (ADR-017/ADR-044; routed by the module address, NIM-749) ---
	// Name — base name without the state suffix (like Soul-side core); the full
	// author address = `<Name>.<state>` (core.soul.registered, core.vault.kv-read).
	{
		Name:        "core.bootstrap",
		Description: "Issue one-time tokens for ready-made VM SIDs (keeper-side). Installing the agent and redeeming the token is site-specific since NIM-834 removed the `delivered` state.",
		States:      []string{"issued"},
	},
	{
		Name:        "core.choir",
		Description: "Manage Voice membership in the Choir of the current incarnation (params: incarnation, choir, sid, optional role/position; keeper-side).",
		States:      []string{"present", "absent"},
	},
	{
		Name:        "core.soul",
		Description: "Register a Soul in the keeper registry (keeper-side).",
		States:      []string{"registered"},
	},
	{
		Name:        "core.vault",
		Description: "Work with Vault KV on the keeper side: kv-read (explicit read with an audit event) and kv-present (generate-if-absent - guarantees the secret exists, generates missing crypto/rand). keeper-side.",
		States:      []string{"kv-read", "kv-present"},
	},
}
