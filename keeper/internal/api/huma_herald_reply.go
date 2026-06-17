package api

// HUMA-NATIVE wire-DTO HERALD-домена (heralds + tidings; handler-native T5d-2c).
// Reply/output Body huma-операций herald/tiding — native Go-struct в пакете api, БЕЗ
// legacy-генерата. Handler (handlers/herald.go) возвращает доменные result-ы с плоскими
// полями; register-func (huma_herald.go) проецирует их В ЭТИ типы напрямую
// (newHerald / newTiding / newHeraldListReply / newTidingListReply) — конвертеров
// legacy-генерата → native больше нет.
//
// ИНВАРИАНТЫ (★ wire byte-exact + ★ имя схемы стабильно): форма байт-в-байт = прежний
// legacy-генерата (те же json-теги/omitempty/time.Time); имя exported-структуры = контрактное
// имя схемы (Herald / Tiding / HeraldListReply / TidingListReply — huma
// DefaultSchemaNamer берёт reflect.Type.Name()). ENUM-поле Type — native HeraldType
// (huma_enums.go, INLINE string-enum, wire — строка byte-exact).
//
// ENVELOPE. HeraldListReply/TidingListReply — НЕ generic PagedResponse[T], а прямые
// named-struct с Items []Herald / []Tiding (native element). Поля items/offset/limit/
// total → wire byte-exact (offset/limit/total — Go-int parity прежнего legacy-генерата).

// OUTPUT-PATTERN ИМЁН (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). name ← herald.NamePattern (kebab, Herald/Tiding);
// Tiding.herald — FK на Herald-имя тем же herald.NamePattern. Формат для клиент-кодогена;
// pattern не влияет на json.Marshal (golden byte-exact цел). Output-типы не шарятся с
// request-Body (create/update — отдельные *Request) → input-422-риска нет.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (форма 1:1 с прежним legacy-генерата) ===

// Herald — native тело herald create (201) / get (200) / update (200). Форма 1:1 с
// прежним Herald: config — map БЕЗ omitempty; created_by_aid/secret_ref —
// `*string` С omitempty (nil → ключ опущен); enabled — bool; type — HeraldType
// (enum-поле, inline-схема, wire — строка); created_at/updated_at — наносекундный
// time-wire.
type Herald struct {
	Config       map[string]interface{} `json:"config"`
	CreatedAt    time.Time              `json:"created_at"`
	CreatedByAID *string                `json:"created_by_aid,omitempty"`
	Enabled      bool                   `json:"enabled"`
	Name         string                 `json:"name" pattern:"^[a-z0-9-]{1,63}$"` // ← herald.NamePattern
	SecretRef    *string                `json:"secret_ref,omitempty"`
	Type         HeraldType             `json:"type"`
	UpdatedAt    time.Time              `json:"updated_at"`
}

// Tiding — native тело tiding create (201) / get (200) / update (200). Форма 1:1 с
// прежним Tiding: annotations — `*map` С omitempty; cadence/created_by_aid/
// ephemeral/incarnation/projection/task/voyage_id — опц. указатели С omitempty;
// event_types — []string БЕЗ omitempty; only_changes/only_failures/enabled — bool;
// created_at/updated_at — наносекундный time-wire.
type Tiding struct {
	Annotations  *map[string]interface{} `json:"annotations,omitempty"`
	Cadence      *string                 `json:"cadence,omitempty"`
	CreatedAt    time.Time               `json:"created_at"`
	CreatedByAID *string                 `json:"created_by_aid,omitempty"`
	Enabled      bool                    `json:"enabled"`
	Ephemeral    *bool                   `json:"ephemeral,omitempty"`
	EventTypes   []string                `json:"event_types"`
	Herald       string                  `json:"herald" pattern:"^[a-z0-9-]{1,63}$"` // ← herald.NamePattern (FK на heralds.name)
	Incarnation  *string                 `json:"incarnation,omitempty"`
	Name         string                  `json:"name" pattern:"^[a-z0-9-]{1,63}$"` // ← herald.NamePattern
	OnlyChanges  bool                    `json:"only_changes"`
	OnlyFailures bool                    `json:"only_failures"`
	Projection   *[]string               `json:"projection,omitempty"`
	Task         *string                 `json:"task,omitempty"`
	UpdatedAt    time.Time               `json:"updated_at"`
	VoyageID     *string                 `json:"voyage_id,omitempty"`
}

// === envelope reply-DTO (форма 1:1 с прежним legacy-генерата; element → native) ===

// HeraldListReply — native 200-envelope GET /v1/heralds. Форма 1:1 с прежним
// HeraldListReply (items/limit/offset/total; offset/limit/total — Go-int). Items —
// []Herald (native element).
type HeraldListReply struct {
	Items  []Herald `json:"items"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
	Total  int      `json:"total"`
}

// TidingListReply — native 200-envelope GET /v1/tidings. Форма 1:1 с прежним
// TidingListReply. Items — []Tiding (native element).
type TidingListReply struct {
	Items  []Tiding `json:"items"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
	Total  int      `json:"total"`
}

// === проекция доменных result-ов handler-а → native wire-DTO ===

// newHerald проецирует доменный handlers.HeraldView в native Herald. type — native
// enum HeraldType (inline string-enum).
func newHerald(v handlers.HeraldView) Herald {
	return Herald{
		Config:       v.Config,
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Enabled:      v.Enabled,
		Name:         v.Name,
		SecretRef:    v.SecretRef,
		Type:         HeraldType(v.Type),
		UpdatedAt:    v.UpdatedAt,
	}
}

// newTiding проецирует доменный handlers.TidingView в native Tiding.
func newTiding(v handlers.TidingView) Tiding {
	return Tiding{
		Annotations:  v.Annotations,
		Cadence:      v.Cadence,
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Enabled:      v.Enabled,
		Ephemeral:    v.Ephemeral,
		EventTypes:   v.EventTypes,
		Herald:       v.Herald,
		Incarnation:  v.Incarnation,
		Name:         v.Name,
		OnlyChanges:  v.OnlyChanges,
		OnlyFailures: v.OnlyFailures,
		Projection:   v.Projection,
		Task:         v.Task,
		UpdatedAt:    v.UpdatedAt,
		VoyageID:     v.VoyageID,
	}
}

// newHeraldListReply проецирует доменный handlers.HeraldListPage в native envelope
// HeraldListReply (items non-nil []).
func newHeraldListReply(p handlers.HeraldListPage) HeraldListReply {
	items := make([]Herald, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newHerald(v))
	}
	return HeraldListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}

// newTidingListReply проецирует доменный handlers.TidingListPage в native envelope
// TidingListReply (items non-nil []).
func newTidingListReply(p handlers.TidingListPage) TidingListReply {
	items := make([]Tiding, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newTiding(v))
	}
	return TidingListReply{Items: items, Limit: p.Limit, Offset: p.Offset, Total: p.Total}
}
