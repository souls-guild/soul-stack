package handlers

// Editorial data for core modules, for the module catalog (`GET /v1/modules`).
//
// WHICH modules the catalog lists is not decided here: it lists the served catalog,
// [coremanifest.SoulSideAddrs] plus [coremanifest.KeeperSideAddrs], which the two
// binaries' registries are held against by guard tests (NIM-890). A module's states
// come from its declaration in `shared/coremanifest`. What is left for this table is
// what no registry carries: a human-readable description, the errand-safe marker, and
// states for the two served modules that have no declaration (core.augur, core.cert).
//
// Source of truth for errand-safe — soul/internal/runtime/errandrunner/whitelist.go
// (the verb-shell set core.cmd.shell / core.exec.run) + the exact read-safe
// mixed-module state core.http.probe (core.http.request is deliberately excluded).

// coreModuleDoc — the editorial part of one core module's catalog entry.
type coreModuleDoc struct {
	// Description — human-readable description.
	Description string
	// States — set ONLY for a served module with no declaration in
	// `shared/coremanifest` (core.augur, core.cert): there is nothing to derive them
	// from. A declared module's states come from the declaration, and a guard test
	// refuses a hand-written copy beside it.
	States []string
	// ErrandSafeStates — the subset of the module's states safe for ad-hoc invocation
	// via the Errand pull contour (ADR-033). Empty = the module is not errand-safe in
	// any state.
	ErrandSafeStates []string
}

// coreModuleDocs — keyed by the module's base address. Keeper-side entries are
// published unconditionally even where the registry registers them conditionally
// (core.choir needs Deps.ChoirStore).
var coreModuleDocs = map[string]coreModuleDoc{
	// --- soul-side (ADR-015) ---
	"core.archive": {
		Description: "Extract an archive (tar/tar.gz/tar.bz2/zip) into the destination directory.",
	},
	"core.augur": {
		Description: "Read-probe of live access to an external system via the Augur broker (verb fetch, changed=false).",
		States:      []string{"fetch"},
	},
	"core.cmd": {
		Description:      "Run an arbitrary shell command (imperative verb shell).",
		ErrandSafeStates: []string{"shell"},
	},
	"core.cron": {
		Description: "Manage a cron job (present/absent).",
	},
	"core.directory": {
		Description: "Manage a directory: create it with the given owner and permissions, or remove it.",
	},
	"core.exec": {
		Description:      "Run a command without a shell wrapper (imperative verb run).",
		ErrandSafeStates: []string{"run"},
	},
	"core.file": {
		Description: "Manage a file: present (inline-content), absent, rendered (text/template render).",
	},
	"core.firewall": {
		Description: "Manage a firewall rule (present/absent).",
	},
	"core.git": {
		Description: "Clone a git repository into a directory, or fast-forward an existing checkout.",
	},
	"core.group": {
		Description: "Manage a system group (present/absent).",
	},
	"core.http": {
		Description:      "HTTP endpoint probe (GET/HEAD, changed=false) and explicit mutating request (POST/PUT/PATCH/DELETE, changed=true on success).",
		ErrandSafeStates: []string{"probe"},
	},
	"core.line": {
		Description: "Line-by-line in-place file edit (present/absent, regex-match).",
	},
	"core.module": {
		Description: "Deliver a SoulModule plugin to the host and register it; synthesized from service.yml modules[] (ADR-065).",
	},
	"core.mount": {
		Description: "Manage a mount point (present/absent/mounted/unmounted).",
	},
	"core.noop": {
		Description: "Do nothing and report changed=false — a syntactic anchor, not an operation on a resource.",
	},
	"core.pkg": {
		Description: "Manage a system package (installed/absent/latest).",
	},
	"core.repo": {
		Description: "Manage a package repository (present/absent).",
	},
	"core.service": {
		Description: "Manage an init-system service: whether it runs, a restart, and whether it starts at boot.",
	},
	"core.sysctl": {
		Description: "Manage kernel parameters: one parameter at runtime and persisted, or a set as one sysctl.d drop-in.",
	},
	"core.url": {
		Description: "Download a file by URL with checksum verification (state fetched).",
	},
	"core.user": {
		Description: "Manage a system user (present/absent).",
	},

	// --- keeper-side (ADR-017/ADR-044; routed by the module address, NIM-749) ---
	"core.bootstrap": {
		Description: "Issue one-time tokens for ready-made VM SIDs (keeper-side). Installing the agent and redeeming the token is site-specific since NIM-834 removed the `delivered` state.",
	},
	"core.cert": {
		Description: "Track the incarnation's service TLS certs in the Warrant registry: record an issued cert, or mint one through Vault PKI and record it (keeper-side).",
		States:      []string{"registered", "issued"},
	},
	"core.choir": {
		Description: "Manage Voice membership in the Choir of the current incarnation (params: incarnation, choir, sid, optional role/position; keeper-side).",
	},
	"core.soul": {
		Description: "Register a Soul in the keeper registry (keeper-side).",
	},
	"core.ssh": {
		Description: "Reach hosts with no agent over SSH: run shell steps, or deliver the agent and apply a destiny rendered per host (keeper-side).",
	},
	"core.state": {
		Description: "Write one field of the incarnation state at this step; the state suffix is the verb (keeper-side, ADR-0084).",
	},
	"core.vault": {
		Description: "Work with Vault KV on the keeper side: kv-read (explicit read with an audit event) and kv-present (generate-if-absent - guarantees the secret exists, generates missing crypto/rand). keeper-side.",
	},
}
