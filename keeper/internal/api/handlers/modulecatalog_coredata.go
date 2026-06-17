package handlers

// Doc-данные core-модулей для module-catalog (`GET /v1/modules`).
//
// Почему таблица, а не интроспекция реестра: keeper НЕ импортирует
// soul/internal/coremod (изоляция Soul по ADR-011 — soul-side core статически
// встроен в `soul`-бинарь, keeper его не видит компилятором). Сами реализации
// модулей не несут ни описания, ни input-схемы в коде (params валидируются
// императивно внутри Apply/Validate, не декларативно). Поэтому каталог core
// публикует то, что РЕАЛЬНО доступно как статический факт: имя, kind, описание,
// допустимые state-ы и errand-safe-маркер. Полная input-схема НЕ формализована
// (это новая сущность — см. отчёт; stop-rule), params core-модулей здесь пусты.
//
// Источник правды для имён/state-ов — Validate-свитчи soul-side core-модулей
// (soul/internal/coremod/<m>/<m>.go) и docs/module/core/<m>/. Источник правды
// для errand-safe — soul/internal/runtime/errandrunner/whitelist.go (жёсткий
// список core.cmd.shell / core.exec.run) + marker sdk/module.ErrandReadSafe
// (core.http.probe). Любое расхождение со списком state-ов в реализации обязано
// синхронизироваться вручную — это doc-data, а не интроспекция.

// coreModuleDoc — статическая запись одного core-модуля каталога.
type coreModuleDoc struct {
	// Name — каноническое имя модуля без state-суффикса (`core.cmd`).
	Name string
	// Description — человекочитаемое описание (русский, как остальные сообщения).
	Description string
	// States — допустимые state-/verb-суффиксы модуля (полный адрес —
	// `<Name>.<state>`).
	States []string
	// ErrandSafeStates — подмножество States, безопасное к ad-hoc-вызову через
	// Errand pull-контур (ADR-033). Пусто = модуль не errand-safe ни в одном
	// state.
	ErrandSafeStates []string
}

// coreModuleDocs — таблица 18 soul-side core-модулей MVP (ADR-015) +
// keeper-side core (core.cloud/core.soul/core.vault по ADR-017, core.choir по
// ADR-044). Соответствует soul/internal/coremod.Default (18) и
// keeper/internal/coremod.Default (core.choir регистрируется условно — при
// наличии Deps.ChoirStore, но в каталоге публикуется всегда). Порядок —
// алфавитный по Name для детерминированной выдачи.
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
	// Name — base-имя без state-суффикса (как у Soul-side core); полный
	// author-адрес = `<Name>.<state>` (core.cloud.created, core.vault.kv-read).
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
		Description: "Явное чтение Vault KV на keeper-стороне (state kv-read, keeper-side, on: keeper).",
		States:      []string{"kv-read"},
	},
}
