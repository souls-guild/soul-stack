package handlers

// HUMA-NATIVE доменные view-DTO INCARNATION-домена (T5d-2c-full handler-native). *Typed-
// функции (incarnation_typed.go) возвращают ПЛОСКИЕ доменные view-структуры этого файла —
// БЕЗ legacy-генерата. Пакет api проецирует их в native reply-DTO (huma_incarnation_reply.go →
// newIncarnationGetReply / newStateHistoryEntry). Этим из handler-слоя вырезана последняя
// live-зависимость от legacy-генерата (прежние toIncarnationGetReply/toStateHistoryEntry строили
// legacy-генерата; конвертеры в api сняты — register-func строит native напрямую из view).
//
// ИНВАРИАНТЫ (★ wire byte-exact, проекция api сохраняет форму 1:1):
//   - View несёт ДОМЕННЫЕ типы (time.Time as-is, map[string]any, string-status). Проекция в
//     api кастует status-string → native enum (тот же underlying string → byte-exact) и
//     оборачивает map → *map (nil-различимость сохранена).
//   - date-time created_at/updated_at/last_drift_check_at/scanned_at — НАНОСЕКУНДНЫЙ wire
//     (.UTC() БЕЗ Truncate, incarnation-поля — голый time.Time; усечение сломало бы байт).
//   - covens — non-nil slice (coalesceCoven → `[]` при nil), как прежний DTO.
//   - spec/state прогоняются через [audit.MaskSecrets] (defense-in-depth, вариант D) ровно
//     как прежний toDTO — наружу секреты уходят замаскированными, в БД хранится оригинал.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// IncarnationGetView — ПЛОСКАЯ доменная проекция incarnation для 200-тела GET /v1/incarnations/
// {name} (и list-element, и PATCH .../hosts). Пакет api проецирует её в native IncarnationGetReply.
// Status — RAW string домена (native-тип в api держит enum-форму). Spec/State/StatusDetails —
// map[string]any (nil → `null` через *map в проекции). CreatedByAID/LastDriftCheckAt/
// LastDriftSummary — pointer-optional. covens — non-nil slice.
type IncarnationGetView struct {
	Covens             []string
	CreatedAt          time.Time
	CreatedByAID       *string
	LastDriftCheckAt   *time.Time
	LastDriftSummary   *DriftScanSummaryView
	Name               string
	Service            string
	ServiceVersion     string
	Spec               map[string]any
	State              map[string]any
	StateSchemaVersion int32
	Status             string
	StatusDetails      map[string]any
	UpdatedAt          time.Time
}

// DriftScanSummaryView — native counts-агрегат last_drift_summary (доменная форма). int (не
// int32) — parity wire. ScannedAt — наносекундный time-wire.
type DriftScanSummaryView struct {
	HostsClean       int
	HostsDrifted     int
	HostsFailed      int
	HostsUnsupported int
	ScannedAt        time.Time
	TotalHosts       int
}

// StateHistoryView — native элемент history.items (доменная форма). ChangedByAID — *string
// (пустая строка → nil → ключ опущен в проекции). StateBefore/StateAfter — map (nil → `null`).
// CreatedAt — наносекундный time-wire.
type StateHistoryView struct {
	ApplyID      string
	ChangedByAID *string
	CreatedAt    time.Time
	HistoryID    string
	Scenario     string
	StateAfter   map[string]any
	StateBefore  map[string]any
}

// toIncarnationGetView проецирует incarnation в доменный [IncarnationGetView]. Маскировка
// spec/state — через [audit.MaskSecrets] (единый источник defense-in-depth, parity прежнего
// toDTO). date-time — `.UTC()` БЕЗ Truncate. covens nil → `[]`.
func toIncarnationGetView(inc *incarnation.Incarnation) IncarnationGetView {
	view := IncarnationGetView{
		Covens:             coalesceCoven(inc.Covens),
		CreatedAt:          inc.CreatedAt.UTC(),
		CreatedByAID:       inc.CreatedByAID,
		Name:               inc.Name,
		Service:            inc.Service,
		ServiceVersion:     inc.ServiceVersion,
		Spec:               audit.MaskSecrets(inc.Spec),
		State:              audit.MaskSecrets(inc.State),
		StateSchemaVersion: int32(inc.StateSchemaVersion),
		Status:             string(inc.Status),
		StatusDetails:      inc.StatusDetails,
		UpdatedAt:          inc.UpdatedAt.UTC(),
		LastDriftSummary:   toDriftScanSummaryView(inc.LastDriftSummary),
	}
	if inc.LastDriftCheckAt != nil {
		t := inc.LastDriftCheckAt.UTC()
		view.LastDriftCheckAt = &t
	}
	return view
}

// toDriftScanSummaryView проецирует typed-домен [incarnation.DriftScanSummary] в доменный
// view. nil (колонка NULL) → nil (проекция api опускает ключ через omitempty). ScannedAt —
// наносекундный wire (тот же json-контракт, что пишет scry).
func toDriftScanSummaryView(s *incarnation.DriftScanSummary) *DriftScanSummaryView {
	if s == nil {
		return nil
	}
	return &DriftScanSummaryView{
		HostsDrifted:     s.HostsDrifted,
		HostsClean:       s.HostsClean,
		HostsUnsupported: s.HostsUnsupported,
		HostsFailed:      s.HostsFailed,
		TotalHosts:       s.TotalHosts,
		ScannedAt:        s.ScannedAt,
	}
}

// toStateHistoryView проецирует state_history-row в доменный [StateHistoryView]. state_before/
// state_after прогоняются через [audit.MaskSecrets] (defense-in-depth, parity прежнего
// toHistoryDTO). changed_by_aid — *string (пустой → nil → ключ опущен). created_at — `.UTC()`
// без Truncate.
func toStateHistoryView(e *incarnation.HistoryEntry) StateHistoryView {
	view := StateHistoryView{
		HistoryID:   e.HistoryID,
		Scenario:    e.Scenario,
		StateBefore: audit.MaskSecrets(e.StateBefore),
		StateAfter:  audit.MaskSecrets(e.StateAfter),
		ApplyID:     e.ApplyID,
		CreatedAt:   e.At.UTC(),
	}
	if e.ChangedByAID != nil && *e.ChangedByAID != "" {
		view.ChangedByAID = e.ChangedByAID
	}
	return view
}
