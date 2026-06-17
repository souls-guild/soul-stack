package api

// HUMA-NATIVE wire-DTO ORACLE-домена (vigils + decrees; handler-native T5d-2c).
// Reply/output Body huma-операций oracle — native Go-struct в пакете api, БЕЗ legacy-генерата.
// Handler (handlers/oracle.go) возвращает доменные result-ы с плоскими полями;
// register-func (huma_oracle.go) проецирует их В ЭТИ типы напрямую (newVigilView /
// newDecreeView / newVigilListReply / newDecreeListReply) — конвертеров legacy-генерата → native
// больше нет.
//
// ИНВАРИАНТЫ (★ wire byte-exact + ★ имя схемы стабильно): имя EXPORTED-struct =
// контрактное (VigilView / DecreeView / VigilListReply / DecreeListReply) → huma
// DefaultSchemaNamer даёт ту же схему; форма (json-теги/omitempty/json.RawMessage-
// nil→`null`/time.Time-wire/FIELD-ORDER под oapi byte-order) побайтово та же —
// golden byte-exact пинит huma_oracle_reply_test.go. params/action_input —
// json.RawMessage byte-passthrough (ADR-051 категория D).
//
// ENVELOPE. VigilListReply/DecreeListReply — НЕ generic alias PagedResponse[X], а
// конкретные reply-типы; element-поле Items → native ([]VigilView/[]DecreeView),
// форма items/offset/limit/total 1:1.

// OUTPUT-PATTERN ИМЁН (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). name ← oracle.NamePattern (kebab, Vigil/Decree);
// on_beacon — FK на Vigil-имя тем же oracle.NamePattern; incarnation_name ←
// oracle.IncarnationPattern (тот же const, что INPUT decree.create incarnation_name). Формат
// для клиент-кодогена; pattern не влияет на json.Marshal (golden byte-exact цел). Output-типы
// не шарятся с request-Body (create — отдельные *Request) → input-422-риска нет. coven НЕ
// тегируется: вне coven-скоупа этого батча (Soul*/Incarnation* View), отдельный домен.

import (
	"encoding/json"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (форма 1:1 с прежним legacy-генерата) ===

// VigilView — native проекция записи реестра vigils (форма 1:1 с прежним VigilView).
// coven/created_by_aid/sid — `*[]string`/`*string` С omitempty (nil → ключ опущен);
// params — json.RawMessage БЕЗ omitempty (nil → `null`); created_at/updated_at —
// наносекундный time-wire.
type VigilView struct {
	Check        string          `json:"check"`
	Coven        *[]string       `json:"coven,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
	Enabled      bool            `json:"enabled"`
	Interval     string          `json:"interval"`
	Name         string          `json:"name" pattern:"^[a-z0-9-]{1,63}$"` // ← oracle.NamePattern
	Params       json.RawMessage `json:"params"`
	SID          *string         `json:"sid,omitempty"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// DecreeView — native проекция записи реестра decrees (форма 1:1 с прежним DecreeView).
// coven/created_by_aid/sid/where — С omitempty (nil → ключ опущен); action_input —
// json.RawMessage БЕЗ omitempty (nil → `null`); created_at/updated_at — наносекундный
// time-wire.
type DecreeView struct {
	ActionInput     json.RawMessage `json:"action_input"`
	ActionScenario  string          `json:"action_scenario"`
	Cooldown        string          `json:"cooldown"`
	Coven           *[]string       `json:"coven,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	CreatedByAID    *string         `json:"created_by_aid,omitempty"`
	Enabled         bool            `json:"enabled"`
	IncarnationName string          `json:"incarnation_name" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← oracle.IncarnationPattern
	Name            string          `json:"name" pattern:"^[a-z0-9-]{1,63}$"`                     // ← oracle.NamePattern
	OnBeacon        string          `json:"on_beacon" pattern:"^[a-z0-9-]{1,63}$"`                // ← oracle.NamePattern (FK на Vigil-имя)
	SID             *string         `json:"sid,omitempty"`
	UpdatedAt       time.Time       `json:"updated_at"`
	Where           *string         `json:"where,omitempty"`
}

// === envelope reply-DTO (element-поле → native, форма 1:1) ===

// VigilListReply — native 200-envelope GET /v1/vigils (форма 1:1 с прежним VigilListReply).
// items — []VigilView (native element); offset/limit/total — int32.
type VigilListReply struct {
	Items  []VigilView `json:"items"`
	Limit  int32       `json:"limit"`
	Offset int32       `json:"offset"`
	Total  int32       `json:"total"`
}

// DecreeListReply — native 200-envelope GET /v1/decrees (форма 1:1 с прежним DecreeListReply).
type DecreeListReply struct {
	Items  []DecreeView `json:"items"`
	Limit  int32        `json:"limit"`
	Offset int32        `json:"offset"`
	Total  int32        `json:"total"`
}

// === проекция доменных result-ов handler-а → native wire-DTO ===

// newVigilView проецирует доменный handlers.VigilView (плоские поля) в native VigilView.
func newVigilView(v handlers.VigilView) VigilView {
	return VigilView{
		Check:        v.Check,
		Coven:        v.Coven,
		CreatedAt:    v.CreatedAt,
		CreatedByAID: v.CreatedByAID,
		Enabled:      v.Enabled,
		Interval:     v.Interval,
		Name:         v.Name,
		Params:       v.Params,
		SID:          v.SID,
		UpdatedAt:    v.UpdatedAt,
	}
}

// newDecreeView проецирует доменный handlers.DecreeView в native DecreeView.
func newDecreeView(d handlers.DecreeView) DecreeView {
	return DecreeView{
		ActionInput:     d.ActionInput,
		ActionScenario:  d.ActionScenario,
		Cooldown:        d.Cooldown,
		Coven:           d.Coven,
		CreatedAt:       d.CreatedAt,
		CreatedByAID:    d.CreatedByAID,
		Enabled:         d.Enabled,
		IncarnationName: d.IncarnationName,
		Name:            d.Name,
		OnBeacon:        d.OnBeacon,
		SID:             d.SID,
		UpdatedAt:       d.UpdatedAt,
		Where:           d.Where,
	}
}

// newVigilListReply проецирует доменный handlers.VigilListPage в native envelope
// VigilListReply (items non-nil [], offset/limit/total int32).
func newVigilListReply(p handlers.VigilListPage) VigilListReply {
	items := make([]VigilView, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newVigilView(v))
	}
	return VigilListReply{Items: items, Limit: int32(p.Limit), Offset: int32(p.Offset), Total: int32(p.Total)}
}

// newDecreeListReply проецирует доменный handlers.DecreeListPage в native envelope
// DecreeListReply.
func newDecreeListReply(p handlers.DecreeListPage) DecreeListReply {
	items := make([]DecreeView, 0, len(p.Items))
	for _, d := range p.Items {
		items = append(items, newDecreeView(d))
	}
	return DecreeListReply{Items: items, Limit: int32(p.Limit), Offset: int32(p.Offset), Total: int32(p.Total)}
}
