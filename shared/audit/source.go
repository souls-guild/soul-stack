package audit

// Source — closed enum инициаторов audit-event-а по ADR-022(b).
// Соответствует колонке `audit_log.source` в Postgres. Расширение
// (`policy_check`, …) — propose-and-wait через docs/naming-rules.md.
//
// Каст произвольной строки (`Source("hax0r")`) технически возможен —
// инвариант валидируется write-path-реализацией [Writer] перед INSERT
// (см. `keeper/internal/auditpg.NewWriter`).
type Source string

const (
	// SourceSignal — hot-reload pipeline, реагирующий на SIGHUP
	// (ADR-021). `archon_aid` всегда NULL: оператор у клавиатуры на
	// хосте, identity не аутентифицирована Keeper-ом.
	SourceSignal Source = "signal"

	// SourceAPI — HTTP-middleware Operator API. `archon_aid` берётся
	// из JWT-claim `sub`.
	SourceAPI Source = "api"

	// SourceMCP — MCP-handler. `archon_aid` берётся из JWT.
	SourceMCP Source = "mcp"

	// SourceKeeperInternal — Reaper, scheduled tasks, bootstrap.
	// `archon_aid` всегда NULL.
	SourceKeeperInternal Source = "keeper_internal"

	// SourceSoulGRPC — Keeper-side forwarder событий от Soul через
	// gRPC EventStream (ADR-012). `archon_aid` всегда NULL;
	// `correlation_id` = `apply_id`.
	SourceSoulGRPC Source = "soul_grpc"

	// SourceBackground — фоновое периодическое правило Reaper-а, инициирующее
	// Scry-проверку drift (ADR-031 Slice C, `scry_background`). Отличается от
	// [SourceKeeperInternal] семантически: фоновый dry_run прогон — это
	// security-сигнал (ставится `ApplyRequest{dry_run:true}` на хосты без
	// инициативы оператора), он не должен сваливаться в общий
	// `keeper_internal`-фильтр аудита. `archon_aid` всегда NULL (нет
	// идентифицированного инициатора), `correlation_id` = `apply_id` Scry-
	// прогона.
	SourceBackground Source = "background"

	// SourceConfigBootstrap — one-shot legacy-import при старте Keeper-а
	// (ADR-032 amendment 2026-05-26, S7-4): миграция inline-`keeper.yml`-
	// блоков (`push.targets[]` / `push.providers[]`) в PG-источники под
	// явным opt-in флагом (`push.auto_import_legacy_*`). Отделено от
	// [SourceKeeperInternal] семантически: миграция данных по согласию
	// оператора — отдельный security-сигнал (отличить от Reaper / scheduled
	// tasks при фильтрации `GET /v1/audit`). `archon_aid` всегда NULL
	// (system-action, инициатива не оператора, а конфига); `correlation_id`
	// пустой.
	SourceConfigBootstrap Source = "config_bootstrap"
)

// Valid возвращает true, если значение — одно из MVP-значений closed enum-а.
func (s Source) Valid() bool {
	switch s {
	case SourceSignal, SourceAPI, SourceMCP, SourceKeeperInternal, SourceSoulGRPC, SourceBackground, SourceConfigBootstrap:
		return true
	}
	return false
}

// RequiresArchonAID возвращает true для источников, у которых
// `archon_aid` должен быть **не-NULL** (`SourceAPI` / `SourceMCP` —
// аутентифицированный оператор через JWT). Для `signal` / `keeper_internal`
// / `soul_grpc` оператор не идентифицирован, `archon_aid` = NULL.
//
// Helper не проверяется внутри [Writer.Write]: инвариант на ответственности
// инициатора (как и каст ReloadSource из shared/config). Caller может
// `assert`-нуть через этот helper перед сборкой [Event].
func (s Source) RequiresArchonAID() bool {
	return s == SourceAPI || s == SourceMCP
}
