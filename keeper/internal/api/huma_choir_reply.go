package api

// HUMA-NATIVE wire-DTO CHOIR/VOICE-домена (handler-native T5d-2c). Reply/output Body
// huma-операций choir/voice — native Go-struct в пакете api, БЕЗ legacy-генерата. Handler
// (handlers/choir.go) возвращает доменные result-ы с плоскими полями; register-func
// (huma_choir.go) проецирует их В ЭТИ типы напрямую (newChoir / newVoice /
// newChoirListReply / newVoiceListReply) — конвертеров legacy-генерата → native больше нет.
//
// ИНВАРИАНТЫ (★ wire byte-exact + ★ имя схемы стабильно): форма байт-в-байт = прежний
// legacy-генерата (те же json-теги; created_by_aid/description/min_size/max_size — `*` БЕЗ
// omitempty → `null` при nil, категория D; time.Time-wire; envelope int — НЕ int32).
// Имя EXPORTED-struct = контрактное (Choir / Voice / ChoirListReply / VoiceListReply) →
// huma DefaultSchemaNamer даёт ту же схему.
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). created_by_aid/added_by_aid ←
// operator.AIDPattern; Voice.sid ← soul.SIDPattern; incarnation_name ←
// incarnation.NamePattern (батч 5, FK на incarnation; path-{name} при записи).
// Формат для клиент-кодогена; pattern не влияет на json.Marshal (golden byte-exact
// цел). choir_name НЕ тегируется: своя грамматика choir.choirNamePattern (kebab+`_`),
// вне name-скоупа этого батча.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (форма 1:1 с прежним legacy-генерата) ===

// Choir — native проекция Choir-записи (форма 1:1 с прежним Choir). created_by_aid/
// description/min_size/max_size — `*` БЕЗ omitempty (nil → `null`); created_at —
// наносекундный time-wire.
type Choir struct {
	ChoirName       string    `json:"choir_name"`
	CreatedAt       time.Time `json:"created_at"`
	CreatedByAID    *string   `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	Description     *string   `json:"description"`
	IncarnationName string    `json:"incarnation_name" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
	MaxSize         *int      `json:"max_size"`
	MinSize         *int      `json:"min_size"`
}

// Voice — native проекция Voice-членства (форма 1:1 с прежним Voice). added_by_aid/
// position/role — `*` БЕЗ omitempty (nil → `null`); added_at — наносекундный time-wire.
type Voice struct {
	AddedAt         time.Time `json:"added_at"`
	AddedByAID      *string   `json:"added_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	ChoirName       string    `json:"choir_name"`
	IncarnationName string    `json:"incarnation_name" pattern:"^[a-z0-9][a-z0-9-]{0,62}$"` // ← incarnation.NamePattern
	Position        *int      `json:"position"`
	Role            *string   `json:"role"`
	SID             string    `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
}

// === envelope reply-DTO (element-поле → native, форма 1:1) ===

// ChoirListReply — native 200-envelope GET .../choirs (форма 1:1 с прежним
// ChoirListReply). items — []Choir; limit/offset/total — int (НЕ int32).
type ChoirListReply struct {
	Items  []Choir `json:"items"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
	Total  int     `json:"total"`
}

// VoiceListReply — native 200-envelope GET .../voices (форма 1:1 с прежним
// VoiceListReply).
type VoiceListReply struct {
	Items  []Voice `json:"items"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
	Total  int     `json:"total"`
}

// === проекция доменных result-ов handler-а → native wire-DTO ===

// newChoir проецирует доменный handlers.ChoirView в native Choir.
func newChoir(v handlers.ChoirView) Choir {
	return Choir{
		ChoirName:       v.ChoirName,
		CreatedAt:       v.CreatedAt,
		CreatedByAID:    v.CreatedByAID,
		Description:     v.Description,
		IncarnationName: v.IncarnationName,
		MaxSize:         v.MaxSize,
		MinSize:         v.MinSize,
	}
}

// newVoice проецирует доменный handlers.VoiceView в native Voice.
func newVoice(v handlers.VoiceView) Voice {
	return Voice{
		AddedAt:         v.AddedAt,
		AddedByAID:      v.AddedByAID,
		ChoirName:       v.ChoirName,
		IncarnationName: v.IncarnationName,
		Position:        v.Position,
		Role:            v.Role,
		SID:             v.SID,
	}
}

// newChoirListReply проецирует доменный handlers.ChoirListPage в native envelope
// ChoirListReply (items non-nil []; offset 0, limit/total = len — full list без
// серверной пагинации, parity легаси ListChoirs).
func newChoirListReply(p handlers.ChoirListPage) ChoirListReply {
	items := make([]Choir, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newChoir(v))
	}
	return ChoirListReply{Items: items, Offset: 0, Limit: len(items), Total: len(items)}
}

// newVoiceListReply проецирует доменный handlers.VoiceListPage в native envelope
// VoiceListReply (items non-nil []; offset 0, limit/total = len).
func newVoiceListReply(p handlers.VoiceListPage) VoiceListReply {
	items := make([]Voice, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newVoice(v))
	}
	return VoiceListReply{Items: items, Offset: 0, Limit: len(items), Total: len(items)}
}
