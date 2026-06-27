package api

// HUMA-NATIVE reply-DTO INCARNATION-домена (T5d-2c-full handler-native). Reply/output Body
// huma-операций — native Go-struct в пакете api, БЕЗ legacy-генерата. Register-func (huma_incarnation.go)
// проецирует ПЛОСКИЕ доменные handlers.*View → эти native-типы НАПРЯМУЮ (newX(view)) — конвертеров
// legacy-генерата→native больше нет. Ключевое для incarnation:
//
//   - ФОРМА байт-в-байт = прежняя legacy-генерата (json-теги / omitempty (nil → ключ опущен) vs без
//     omitempty (nil-`*map`/`*string` → `null`) / date-time RFC3339Nano / категории A-D ADR-051).
//   - ИМЯ СХЕМЫ = контрактное (IncarnationCreateReply / IncarnationGetReply / ...): huma
//     DefaultSchemaNamer берёт reflect.Type.Name() и капитализирует первую букву → схема под тем
//     же именем, что давал прежний legacy-генерата. Агрегатор-спека (TestFullSpec_) не меняется.
//   - STATUS-ПОЛЯ — NATIVE enum IncarnationStatus (huma_enums.go) с $ref на named-схему
//     "IncarnationStatus" (SchemaProvider). Проекция кастует доменный status-string → native
//     enum (тот же underlying string → byte-exact). Прежний alias IncarnationStatus →
//     native более не нужен (нет ни одного oapi-поля в reflected-Body).
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body против схемы (writeResponse → Transform → Marshal, без Validate;
// эмпирически 200, не 500). `pattern:` на output ID-полях — чисто документация
// формата для клиент-кодогена. apply_id/history_id — машинно-генерируемые ULID
// (audit.NewULID, миграция 006: «history_id (ULID …)»), формат гарантирован.
// *_by_aid ← operator.AIDPattern (миграция 058 — текущий паттерн надмножество
// старого, легаси-AID тоже матчатся). golden byte-exact цел: pattern-тег не влияет
// на json.Marshal.
//
// OUTPUT-PATTERN ИМЁН (батч 5): incarnation_name (Name + echo Incarnation) ←
// incarnation.NamePattern; covens[] ← soul.CovenPattern (per-element, output covens в
// Incarnation* View/Reply). Reply-типы output-only (create/run/upgrade/rerun-create —
// отдельные *Request/*Input) → input-422-риска нет. service — FK на serviceregistry,
// формат покрыт INPUT-доменом (incarnation.create service, батч 4) — output-эхо НЕ
// тегируем (вне name-скоупа Service-View этого батча).

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (форма 1:1 с прежней legacy-генерата-формой) ===

// IncarnationCreateReply — native 202-тело POST /v1/incarnations. apply_id опц.
// (lifecycle.auto_create:false → инкарнация в ready без прогона, apply_id опущен).
type IncarnationCreateReply struct {
	ApplyID     *string `json:"apply_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
	Incarnation string  `json:"incarnation" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"`       // ← incarnation.NamePattern
}

// IncarnationRunReply — native 202-тело POST .../scenarios/{scenario} (apply_id + echo).
type IncarnationRunReply struct {
	ApplyID     string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`     // ULID (audit.NewULID)
	Incarnation string `json:"incarnation" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
	Scenario    string `json:"scenario"`
}

// IncarnationUnlockReply — native 200-тело POST .../unlock. status/previous_status —
// native enum IncarnationStatus (выносится SchemaProvider-ом, wire — строка). unlocked_at —
// наносекундный time-wire (handler даёт .UTC()).
type IncarnationUnlockReply struct {
	Name           string            `json:"name"`
	PreviousStatus IncarnationStatus `json:"previous_status"`
	Status         IncarnationStatus `json:"status"`
	UnlockedAt     time.Time         `json:"unlocked_at"`
	UnlockedByAID  string            `json:"unlocked_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
}

// IncarnationUpgradeReply — native 202-тело POST .../upgrade (apply_id).
type IncarnationUpgradeReply struct {
	ApplyID string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// IncarnationRerunCreateReply — native 202-тело POST .../rerun-create (apply_id + echo).
type IncarnationRerunCreateReply struct {
	ApplyID     string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`     // ULID (audit.NewULID)
	Incarnation string `json:"incarnation" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
}

// IncarnationDestroyReply — native 202-тело DELETE /v1/incarnations/{name} (apply_id).
type IncarnationDestroyReply struct {
	ApplyID string `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// IncarnationGetReply — native тело GET /v1/incarnations/{name} (и PATCH .../hosts, list-element).
// Форма 1:1 с прежней IncarnationGetReply: covens всегда массив (БЕЗ omitempty, не nil);
// created_by_aid/spec/state/status_details — `*map`/`*string` БЕЗ omitempty (nil → `null`);
// last_drift_check_at/last_drift_summary/created_scenario/traits — С omitempty (nil/пустой →
// ключ опущен; traits — голый map, НЕ `*map`, чтобы пустой `{}` опускался). created_at/
// updated_at — наносекундный time-wire (handler даёт .UTC() без Truncate).
type IncarnationGetReply struct {
	Covens             []string                `json:"covens" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"` // ← soul.CovenPattern (per-element)
	CreatedAt          time.Time               `json:"created_at"`
	CreatedByAID       *string                 `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedScenario    string                  `json:"created_scenario,omitempty"`                             // стартовый сценарий (механизм нескольких create); пустой → опущен
	LastDriftCheckAt   *time.Time              `json:"last_drift_check_at,omitempty"`
	LastDriftSummary   *DriftScanSummary       `json:"last_drift_summary,omitempty"`
	Name               string                  `json:"name" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
	Service            string                  `json:"service"`
	ServiceVersion     string                  `json:"service_version"`
	Spec               *map[string]interface{} `json:"spec"`
	State              *map[string]interface{} `json:"state"`
	StateSchemaVersion int32                   `json:"state_schema_version"`
	Status             IncarnationStatus       `json:"status"`
	StatusDetails      *map[string]interface{} `json:"status_details"`
	Traits             map[string]interface{}  `json:"traits,omitempty"` // operator-set метки (ADR-060); пустой map → опущен
	UpdatedAt          time.Time               `json:"updated_at"`
}

// === nested reply-DTO ===

// DriftScanSummary — native counts-агрегат last_drift_summary (форма 1:1 с прежней
// DriftScanSummary; scanned_at — наносекундный time-wire). int (не int32) — parity.
type DriftScanSummary struct {
	HostsClean       int       `json:"hosts_clean"`
	HostsDrifted     int       `json:"hosts_drifted"`
	HostsFailed      int       `json:"hosts_failed"`
	HostsUnsupported int       `json:"hosts_unsupported"`
	ScannedAt        time.Time `json:"scanned_at"`
	TotalHosts       int       `json:"total_hosts"`
}

// StateHistoryEntry — native элемент history.items (форма 1:1 с прежней StateHistoryEntry):
// changed_by_aid — `*string` С omitempty (nil → ключ опущен); state_before/state_after — `*map`
// БЕЗ omitempty (nil → `null`); created_at — наносекундный time-wire (.UTC()).
type StateHistoryEntry struct {
	ApplyID      string                  `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`                      // ULID (audit.NewULID)
	ChangedByAID *string                 `json:"changed_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedAt    time.Time               `json:"created_at"`
	HistoryID    string                  `json:"history_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (миграция 006)
	Scenario     string                  `json:"scenario"`
	StateAfter   *map[string]interface{} `json:"state_after"`
	StateBefore  *map[string]interface{} `json:"state_before"`
}

// === проекция доменных handlers.*View → native wire-DTO (byte-exact passthrough формы) ===

func newIncarnationCreateReply(v handlers.IncarnationCreateView) IncarnationCreateReply {
	return IncarnationCreateReply{ApplyID: v.ApplyID, Incarnation: v.Incarnation}
}

func newIncarnationRunReply(v handlers.IncarnationRunView) IncarnationRunReply {
	return IncarnationRunReply{ApplyID: v.ApplyID, Incarnation: v.Incarnation, Scenario: v.Scenario}
}

func newIncarnationUnlockReply(v handlers.IncarnationUnlockView) IncarnationUnlockReply {
	return IncarnationUnlockReply{
		Name:           v.Name,
		PreviousStatus: IncarnationStatus(v.PreviousStatus),
		Status:         IncarnationStatus(v.Status),
		UnlockedAt:     v.UnlockedAt,
		UnlockedByAID:  v.UnlockedByAID,
	}
}

func newIncarnationUpgradeReply(v handlers.IncarnationUpgradeView) IncarnationUpgradeReply {
	return IncarnationUpgradeReply{ApplyID: v.ApplyID}
}

func newIncarnationRerunCreateReply(v handlers.IncarnationRerunCreateView) IncarnationRerunCreateReply {
	return IncarnationRerunCreateReply{ApplyID: v.ApplyID, Incarnation: v.Incarnation}
}

func newIncarnationDestroyReply(v handlers.IncarnationDestroyView) IncarnationDestroyReply {
	return IncarnationDestroyReply{ApplyID: v.ApplyID}
}

// newDriftScanSummary проецирует доменный *handlers.DriftScanSummaryView в native (nil → nil:
// omitempty опускает ключ).
func newDriftScanSummary(v *handlers.DriftScanSummaryView) *DriftScanSummary {
	if v == nil {
		return nil
	}
	return &DriftScanSummary{
		HostsClean:       v.HostsClean,
		HostsDrifted:     v.HostsDrifted,
		HostsFailed:      v.HostsFailed,
		HostsUnsupported: v.HostsUnsupported,
		ScannedAt:        v.ScannedAt,
		TotalHosts:       v.TotalHosts,
	}
}

// newIncarnationGetReply проецирует плоский доменный handlers.IncarnationGetView в native.
// map-поля spec/state/status_details оборачиваются в *map (nil → `null` БЕЗ omitempty).
// status — native enum-каст (тот же underlying string).
func newIncarnationGetReply(v handlers.IncarnationGetView) IncarnationGetReply {
	return IncarnationGetReply{
		Covens:             v.Covens,
		CreatedAt:          v.CreatedAt,
		CreatedByAID:       v.CreatedByAID,
		CreatedScenario:    v.CreatedScenario,
		LastDriftCheckAt:   v.LastDriftCheckAt,
		LastDriftSummary:   newDriftScanSummary(v.LastDriftSummary),
		Name:               v.Name,
		Service:            v.Service,
		ServiceVersion:     v.ServiceVersion,
		Spec:               ptrMap(v.Spec),
		State:              ptrMap(v.State),
		StateSchemaVersion: v.StateSchemaVersion,
		Status:             IncarnationStatus(v.Status),
		StatusDetails:      ptrMap(v.StatusDetails),
		Traits:             v.Traits,
		UpdatedAt:          v.UpdatedAt,
	}
}

// newStateHistoryEntry проецирует доменный handlers.StateHistoryView в native. state_before/
// state_after оборачиваются в *map (nil → `null`); changed_by_aid as-is (nil → ключ опущен).
func newStateHistoryEntry(v handlers.StateHistoryView) StateHistoryEntry {
	return StateHistoryEntry{
		ApplyID:      v.ApplyID,
		ChangedByAID: v.ChangedByAID,
		CreatedAt:    v.CreatedAt,
		HistoryID:    v.HistoryID,
		Scenario:     v.Scenario,
		StateAfter:   ptrMap(v.StateAfter),
		StateBefore:  ptrMap(v.StateBefore),
	}
}

// ptrMap оборачивает домен-`map[string]any` в `*map[string]interface{}`, сохраняя nil-различимость:
// nil-map → nil-указатель (json-тег без omitempty → `null`), непустой → указатель на тот же map.
func ptrMap(m map[string]any) *map[string]interface{} {
	if m == nil {
		return nil
	}
	cp := map[string]interface{}(m)
	return &cp
}
