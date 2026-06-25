package api

// HUMA-NATIVE reply-DTO SOUL-домена (Teardown T5b, паттерн pilot T5a —
// huma_incarnation_reply.go). Reply/output Body huma-операций soul — native Go-struct в
// пакете api, НЕ генерёный legacy-генерата. Граница/инварианты — см. шапку huma_incarnation_reply.go.
//
// СКОУП T5b (финал, 4 архетипа architect-паттерна). Reply-Body soul-домена переведены на
// huma-native:
//   - SoulCreateReply (POST /v1/souls)            — flat, enum status/transport остаётся legacy-генерата;
//   - SoulIssueTokenReply (POST .../issue-token)  — flat scalars;
//   - SoulSshTargetReply (PUT .../ssh-target)     — class-A reuse nested SoulSshTarget (тот же
//     native-тип, что input PUT-тела); rename-alias SoulSSHTargetReply снят (native-Body
//     даёт схему сам);
//   - SoulListEntry (GET /v1/souls/{sid})         — shared get-Body + envelope-element: один
//     native-тип, get-Body на native; envelope-alias-ключ PagedResponse[SoulListEntry]
//     НЕ тронут (резолвит wire-тип handler-а в native-envelope → native-element);
//   - SoulHistoryReply + SoulHistoryItem (GET .../history) — nested-envelope (НЕ generic).
//
// ИМЯ/ФОРМА 1:1: enum-поля — NATIVE SoulStatus/SoulTransport (huma_enums.go, T5d-2c-full
// Phase 1) — alias aliasSoulStatusTransport выносит их named-схемой с $ref (native-типы сами
// реализуют SchemaProvider, huma_soul_status.go). SoulHistoryItem.type — native SoulHistoryItemType
// (рукопись объявляет enum INLINE внутри SoulHistoryItem, НЕ standalone-схемой → alias не нужен,
// huma инлайнит как `type: string`). Проекция доменных handlers.Soul*View → native кастует
// status/transport plain-string → native enum (byte-exact: тот же string). covens/bootstrap_token/
// expires_at — *-optional С omitempty (nil → ключ опущен).
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). sid ← soul.SIDPattern; created_by_aid ←
// operator.AIDPattern; SoulHistoryItem.id / voyage_id — машинно ULID (id = apply_id|
// errand_id, voyage_id = audit.NewULID); covens[] ← soul.CovenPattern (per-element,
// батч 5 — output covens в Soul* View/Reply). Формат для клиент-кодогена; pattern не
// влияет на json.Marshal (golden byte-exact цел). Reply-типы output-only (create/
// issue-token — отдельные *Request/*Input) → input-422-риска нет.

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (форма 1:1 с legacy-генерата) ===

// SoulCreateReply — native 201-тело POST /v1/souls. Форма 1:1 с SoulCreateReply:
// bootstrap_token/covens/expires_at — *-optional С omitempty (присутствуют только для
// transport=agent; nil → ключ опущен); status/transport — oapi-enum ($ref через alias);
// registered_at — наносекундный time-wire.
type SoulCreateReply struct {
	BootstrapToken *string       `json:"bootstrap_token,omitempty"`
	Covens         *[]string     `json:"covens,omitempty" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"` // ← soul.CovenPattern (per-element)
	CreatedByAID   string        `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"`   // ← operator.AIDPattern
	ExpiresAt      *time.Time    `json:"expires_at,omitempty"`
	RegisteredAt   time.Time     `json:"registered_at"`
	SID            string        `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	Status         SoulStatus    `json:"status"`
	Transport      SoulTransport `json:"transport"`
}

// SoulIssueTokenReply — native 200-тело POST /v1/souls/{sid}/issue-token. Форма 1:1 с
// SoulIssueTokenReply: bootstrap_token/expires_at/sid (все required); expires_at —
// наносекундный time-wire.
type SoulIssueTokenReply struct {
	BootstrapToken string    `json:"bootstrap_token"`
	ExpiresAt      time.Time `json:"expires_at"`
	SID            string    `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
}

// SoulSshTargetReply — native 200-тело PUT /v1/souls/{sid}/ssh-target (КЛАСС A, reuse). Форма
// 1:1 с SoulSSHTargetReply (рукопись :6399): sid + ssh_target (snapshot сохранённого
// target-а), оба required. ssh_target — РЕЮЗ существующего native SoulSshTarget (тот же тип, что
// input PUT-тела, huma_soul_op.go) → одна валидная схема SoulSshTarget для input↔output.
// Имя структуры = контрактное имя схемы (huma DefaultSchemaNamer → "SoulSshTargetReply");
// native-Body эмитит схему сам — rename-alias SoulSSHTargetReply → soulSshTargetReply снят.
type SoulSshTargetReply struct {
	SID       string        `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	SSHTarget SoulSshTarget `json:"ssh_target"`
}

// SoulListEntry — native проекция реестра souls (shared get-Body GET /v1/souls/{sid} +
// element list-envelope). covens — []string БЕЗ omitempty (всегда массив); traits — map БЕЗ
// omitempty (всегда object; bare-soul → `{}` через coalesceTraits, ADR-060 read-path);
// created_by_aid/last_seen_at/last_seen_by_kid/requested_at — *-БЕЗ-omitempty (nil → `null`);
// status/transport — oapi-enum ($ref через aliasSoulStatusTransport); registered_at —
// наносекундный time-wire. Имя структуры = контрактное имя схемы. ★ Envelope-element
// ссылается на эту же схему через alias-ключ PagedResponse[SoulListEntry] → схемы get-Body
// и envelope-element идентичны (TestFullSpec_NoSchemaCollision).
type SoulListEntry struct {
	Covens        []string       `json:"covens" pattern:"^[a-z][a-z0-9]*(-[a-z0-9]+)*$"` // ← soul.CovenPattern (per-element)
	Traits        map[string]any `json:"traits" doc:"operator-set key→value метки (ADR-060); значение — scalar или list of scalars; bare-soul → {}"`
	CreatedByAID  *string        `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	LastSeenAt    *time.Time     `json:"last_seen_at"`
	LastSeenByKid *string        `json:"last_seen_by_kid"`
	RegisteredAt  time.Time      `json:"registered_at"`
	RequestedAt   *time.Time     `json:"requested_at"`
	SID           string         `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	Status        SoulStatus     `json:"status"`
	Transport     SoulTransport  `json:"transport"`
}

// SoulHistoryReply — native 200-envelope GET /v1/souls/{sid}/history (самостоятельный envelope,
// НЕ generic PagedResponse). Форма 1:1 с SoulHistoryReply (types.gen.go :3302):
// items/limit/offset/total (limit/offset/total — int, parity legacy-генерата) + top-level sid (echo
// хоста). Имя структуры = контрактное имя схемы.
type SoulHistoryReply struct {
	Items  []SoulHistoryItem `json:"items"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
	SID    string            `json:"sid" pattern:"^[a-z0-9][a-z0-9.-]{0,253}$"` // ← soul.SIDPattern
	Total  int               `json:"total"`
}

// SoulHistoryItem — native элемент history.items (форма 1:1 с SoulHistoryItem, types.gen.go
// :3276): finished_at/incarnation/module/scenario/voyage_id — *-С omitempty (взаимоисключающие
// nil-поля type=scenario/errand → ключ опущен); id/status — string; started_at — наносекундный
// time-wire; type — SoulHistoryItemType (enum INLINE в рукописи, huma инлайнит `type: string`,
// alias не нужен).
type SoulHistoryItem struct {
	FinishedAt  *time.Time          `json:"finished_at,omitempty"`
	ID          string              `json:"id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (apply_id|errand_id)
	Incarnation *string             `json:"incarnation,omitempty"`
	Module      *string             `json:"module,omitempty"`
	Scenario    *string             `json:"scenario,omitempty"`
	StartedAt   time.Time           `json:"started_at"`
	Status      string              `json:"status"`
	Type        SoulHistoryItemType `json:"type"`
	VoyageID    *string             `json:"voyage_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (audit.NewULID)
}

// === проекция доменных handlers.Soul*View → native wire-DTO (byte-exact passthrough формы) ===

// newSoulCreateReply проецирует плоскую доменную handlers.SoulCreateView в native. Covens —
// non-nil slice handler-а → `&covens` (ключ всегда present, omitempty не срабатывает).
// status/transport — native enum-каст (тот же underlying string).
func newSoulCreateReply(v handlers.SoulCreateView) SoulCreateReply {
	covens := v.Covens
	return SoulCreateReply{
		BootstrapToken: v.BootstrapToken,
		Covens:         &covens,
		CreatedByAID:   v.CreatedByAID,
		ExpiresAt:      v.ExpiresAt,
		RegisteredAt:   v.RegisteredAt,
		SID:            v.SID,
		Status:         SoulStatus(v.Status),
		Transport:      SoulTransport(v.Transport),
	}
}

func newSoulIssueTokenReply(v handlers.SoulIssueTokenView) SoulIssueTokenReply {
	return SoulIssueTokenReply{
		BootstrapToken: v.BootstrapToken,
		ExpiresAt:      v.ExpiresAt,
		SID:            v.SID,
	}
}

// newSoulSshTargetReply проецирует доменную handlers.SoulSshTargetView в native (class-A reuse:
// nested ssh_target — тот же native SoulSshTarget, что input PUT-тела; ssh_provider пусто →
// omitempty опускает ключ).
func newSoulSshTargetReply(v handlers.SoulSshTargetView) SoulSshTargetReply {
	return SoulSshTargetReply{
		SID: v.SID,
		SSHTarget: SoulSshTarget{
			SoulPath:    v.SoulPath,
			SSHPort:     v.SSHPort,
			SSHProvider: v.SSHProvider,
			SSHUser:     v.SSHUser,
		},
	}
}

// newSoulListEntry проецирует плоскую доменную handlers.SoulListView в native SoulListEntry
// (shared get-Body + envelope-element). Covens — slice as-is (non-nullable, handler даёт `[]`);
// Traits — map as-is (non-nullable, handler даёт `{}` через coalesceTraits); nullable-указатели
// as-is; status/transport — native enum-каст.
func newSoulListEntry(v handlers.SoulListView) SoulListEntry {
	return SoulListEntry{
		Covens:        v.Covens,
		Traits:        v.Traits,
		CreatedByAID:  v.CreatedByAID,
		LastSeenAt:    v.LastSeenAt,
		LastSeenByKid: v.LastSeenByKid,
		RegisteredAt:  v.RegisteredAt,
		RequestedAt:   v.RequestedAt,
		SID:           v.SID,
		Status:        SoulStatus(v.Status),
		Transport:     SoulTransport(v.Transport),
	}
}

func newSoulHistoryItem(v handlers.SoulHistoryItemView) SoulHistoryItem {
	return SoulHistoryItem{
		FinishedAt:  v.FinishedAt,
		ID:          v.ID,
		Incarnation: v.Incarnation,
		Module:      v.Module,
		Scenario:    v.Scenario,
		StartedAt:   v.StartedAt,
		Status:      v.Status,
		Type:        SoulHistoryItemType(v.Type),
		VoyageID:    v.VoyageID,
	}
}

// newSoulHistoryReply проецирует доменный handlers.SoulHistoryView в native. Items сохраняют
// nil-vs-empty 1:1 (nil → `null`, [] → `[]`) ради byte-exact (handler даёт non-nil []).
func newSoulHistoryReply(v handlers.SoulHistoryView) SoulHistoryReply {
	var items []SoulHistoryItem
	if v.Items != nil {
		items = make([]SoulHistoryItem, len(v.Items))
		for i := range v.Items {
			items[i] = newSoulHistoryItem(v.Items[i])
		}
	}
	return SoulHistoryReply{
		Items:  items,
		Limit:  v.Limit,
		Offset: v.Offset,
		SID:    v.SID,
		Total:  v.Total,
	}
}
