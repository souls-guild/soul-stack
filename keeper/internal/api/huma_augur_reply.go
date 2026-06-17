package api

// HUMA-NATIVE wire-DTO AUGUR-домена (omens + rites; handler-native T5d-2c). Reply/
// output Body huma-операций augur — native Go-struct в пакете api, БЕЗ legacy-генерата.
// Handler (handlers/augur.go) возвращает доменные result-ы с плоскими полями;
// register-func (huma_augur.go) проецирует их В ЭТИ типы напрямую (newOmenView /
// newRiteView / newOmenListReply / newRiteListReply) — конвертеров legacy-генерата → native
// больше нет.
//
// СОСТАВ. omen create/get → OmenView; omen list → OmenListReply (envelope,
// items[]→OmenView); rite create → RiteView; rite list → RiteListReply (items-only,
// без пагинации). delete-роуты тела не несут (204).
//
// ИМЯ/ФОРМА 1:1 с прежним legacy-генерата (TestSchemaNames_Augur ждёт OmenView/RiteView/
// OmenListReply/RiteListReply): native-структуры названы РОВНО контрактно, форма
// (json-теги/omitempty/date-time/nullable/FIELD-ORDER под oapi byte-order)
// побайтово та же — golden byte-exact пинит huma_augur_reply_test.go. Enum
// source_type — native OmenViewSourceType (huma_enums.go, INLINE string-enum, без
// $ref). allow — json.RawMessage byte-passthrough (ADR-051 категория D).

// OUTPUT-PATTERN ИМЁН (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). name ← augur.NamePattern (kebab); RiteView.omen —
// FK-ссылка на тот же augur.NamePattern (имя Omen-а). Формат для клиент-кодогена; pattern
// не влияет на json.Marshal (golden byte-exact цел). Output-типы не шарятся с request-Body
// (create/grant используют отдельные *Request) → input-422-риска нет.

import (
	"encoding/json"
	"time"
)

// OmenView — native проекция записи реестра omens (create/get + element OmenListReply.items[]).
// Форма 1:1 с прежним OmenView: created_by_aid — *string С omitempty (nil → ключ опущен);
// source_type — OmenViewSourceType (inline string-enum, без $ref); created_at —
// наносекундный time-wire.
type OmenView struct {
	AuthRef      string             `json:"auth_ref"`
	CreatedAt    time.Time          `json:"created_at"`
	CreatedByAID *string            `json:"created_by_aid,omitempty"`
	Endpoint     string             `json:"endpoint"`
	Name         string             `json:"name" pattern:"^[a-z0-9-]{1,63}$"` // ← augur.NamePattern
	SourceType   OmenViewSourceType `json:"source_type"`
}

// OmenListReply — native envelope GET /v1/augur/omens (4-поля-offset). Форма 1:1 с
// прежним OmenListReply: items[]→OmenView; offset/limit/total — int32. Конкретный
// named-struct (НЕ generic PagedResponse), используется как Body напрямую.
type OmenListReply struct {
	Items  []OmenView `json:"items"`
	Limit  int32      `json:"limit"`
	Offset int32      `json:"offset"`
	Total  int32      `json:"total"`
}

// RiteView — native проекция записи реестра rites (create + element RiteListReply.items[]).
// Форма 1:1 с прежним RiteView: allow — json.RawMessage (byte-passthrough JSONB); coven/sid/
// created_by_aid/token_num_uses/token_ttl — *-optional С omitempty; created_at — наносекундный
// time-wire; id — int64.
type RiteView struct {
	Allow        json.RawMessage `json:"allow"`
	Coven        *string         `json:"coven,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	CreatedByAID *string         `json:"created_by_aid,omitempty"`
	Delegate     bool            `json:"delegate"`
	ID           int64           `json:"id"`
	Omen         string          `json:"omen" pattern:"^[a-z0-9-]{1,63}$"` // ← augur.NamePattern (FK на omens.name)
	SID          *string         `json:"sid,omitempty"`
	TokenNumUses *int            `json:"token_num_uses,omitempty"`
	TokenTTL     *string         `json:"token_ttl,omitempty"`
}

// RiteListReply — native тело GET /v1/augur/rites (items-only, list-by-omen без пагинации).
// Форма 1:1 с прежним RiteListReply: РОВНО одно поле items[]→RiteView.
type RiteListReply struct {
	Items []RiteView `json:"items"`
}
