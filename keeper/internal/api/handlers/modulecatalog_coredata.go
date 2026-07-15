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
// list core.cmd.shell / core.exec.run) + marker sdk/module.ErrandReadSafe
// (core.http.probe). Any divergence from the state list in the implementation must
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

// coreModuleDocs — the table of 18 soul-side core modules of the MVP (ADR-015) +
// keeper-side core (core.cloud/core.soul/core.vault per ADR-017, core.choir per
// ADR-044). Matches soul/internal/coremod.Default (18) and
// keeper/internal/coremod.Default (core.choir is registered conditionally — when
// Deps.ChoirStore is present, but is always published in the catalog). Order is
// alphabetical by Name for deterministic output.
var coreModuleDocs = []coreModuleDoc{
	// --- soul-side (ADR-015) ---
	{
		Name:        "core.archive",
		Description: "Распаковка архива (tar/tar.gz/tar.bz2/zip) в каталог назначения.",
		States:      []string{"extracted"},
	},
	{
		Name:        "core.augur",
		Description: "Read-probe живого доступа к внешней системе через брокер Augur (verb fetch, changed=false).",
		States:      []string{"fetch"},
	},
	{
		Name:             "core.cmd",
		Description:      "Выполнение произвольной shell-команды (императивный verb shell).",
		States:           []string{"shell"},
		ErrandSafeStates: []string{"shell"},
	},
	{
		Name:        "core.cron",
		Description: "Управление cron-задачей (present/absent).",
		States:      []string{"present", "absent"},
	},
	{
		Name:             "core.exec",
		Description:      "Выполнение команды без shell-обёртки (императивный verb run).",
		States:           []string{"run"},
		ErrandSafeStates: []string{"run"},
	},
	{
		Name:        "core.file",
		Description: "Управление файлом: present (inline-content), absent, rendered (text/template-рендер).",
		States:      []string{"present", "absent", "rendered"},
	},
	{
		Name:        "core.firewall",
		Description: "Управление правилом файрвола (present/absent).",
		States:      []string{"present", "absent"},
	},
	{
		Name:        "core.git",
		Description: "Клонирование/обновление git-репозитория в каталог (state cloned).",
		States:      []string{"cloned"},
	},
	{
		Name:        "core.group",
		Description: "Управление системной группой (present/absent).",
		States:      []string{"present", "absent"},
	},
	{
		Name:             "core.http",
		Description:      "Read-probe HTTP-эндпоинта (verb probe, GET/HEAD, changed=false).",
		States:           []string{"probe"},
		ErrandSafeStates: []string{"probe"},
	},
	{
		Name:        "core.line",
		Description: "Построчная in-place правка файла (present/absent, regex-match).",
		States:      []string{"present", "absent"},
	},
	{
		Name:        "core.mount",
		Description: "Управление точкой монтирования (present/absent/mounted/unmounted).",
		States:      []string{"present", "absent", "mounted", "unmounted"},
	},
	{
		Name:        "core.pkg",
		Description: "Управление системным пакетом (installed/absent/latest).",
		States:      []string{"installed", "absent", "latest"},
	},
	{
		Name:        "core.repo",
		Description: "Управление пакетным репозиторием (present/absent).",
		States:      []string{"present", "absent"},
	},
	{
		Name:        "core.service",
		Description: "Управление сервисом init-системы (running/stopped/restarted/enabled).",
		States:      []string{"running", "stopped", "restarted", "enabled"},
	},
	{
		Name:        "core.sysctl",
		Description: "Управление параметром ядра sysctl (state present).",
		States:      []string{"present"},
	},
	{
		Name:        "core.url",
		Description: "Скачивание файла по URL с проверкой checksum (state fetched).",
		States:      []string{"fetched"},
	},
	{
		Name:        "core.user",
		Description: "Управление системным пользователем (present/absent).",
		States:      []string{"present", "absent"},
	},

	// --- keeper-side (ADR-017/ADR-044, on: keeper) ---
	// Name — base name without the state suffix (like Soul-side core); the full
	// author address = `<Name>.<state>` (core.cloud.created, core.vault.kv-read).
	{
		Name:        "core.choir",
		Description: "Управление членством Voice в Choir текущей инкарнации (params: incarnation, choir, sid, optional role/position; keeper-side, on: keeper).",
		States:      []string{"present", "absent"},
	},
	{
		Name:        "core.cloud",
		Description: "Provision/destroy облачной VM через CloudDriver-плагин (keeper-side, on: keeper).",
		States:      []string{"created", "destroyed"},
	},
	{
		Name:        "core.soul",
		Description: "Регистрация Soul-а в реестре keeper-а (keeper-side, on: keeper).",
		States:      []string{"registered"},
	},
	{
		Name:        "core.vault",
		Description: "Работа с Vault KV на keeper-стороне: kv-read (явное чтение с audit-event) и kv-present (generate-if-absent — гарантирует существование секрета, генерит недостающее crypto/rand). keeper-side, on: keeper.",
		States:      []string{"kv-read", "kv-present"},
	},
}
