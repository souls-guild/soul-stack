package api

// HUMA-NATIVE wire-DTO OPERATOR-домена (handler-native PILOT T5d). Reply/output
// Body huma-операций — native Go-struct в пакете api, БЕЗ legacy-генерата. Handler
// (handlers/operator.go) возвращает доменные result-ы с плоскими полями;
// register-func (huma_operator.go) проецирует их В ЭТИ типы напрямую — конвертеров
// legacy-генерата → native больше нет (граница api↔handlers строит wire-DTO из доменных
// полей). Ключевое:
//
//   - ИМЯ СХЕМЫ = контрактное (OperatorCreateReply / Operator / IssueTokenReply):
//     huma DefaultSchemaNamer берёт reflect.Type.Name() → схема под тем же именем.
//   - ENUM-поле AuthMethod — native OperatorAuthMethod (huma_enums.go, INLINE-enum):
//     huma инлайнит string-named-тип как `type: string` без $ref.
//   - ENVELOPE: element list-схемы (operatorListReply, huma_operator_envelope.go)
//     ссылается на этот native Operator; alias на PagedResponse[Operator].
//   - ФОРМА wire (json-теги/omitempty/date-time/nullable) — категории A-D ADR-051,
//     golden byte-exact фиксирует huma_operator_reply_test.go.
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body против схемы (эмпирически 200, не 500). aid/*_by_aid ←
// operator.AIDPattern — формат для клиент-кодогена. Миграция 058 ослабила AID
// до текущего паттерна (надмножество старого `archon-*`) → легаси-AID тоже
// матчатся, output-валидации нет → 500-риска нет. pattern-тег не влияет на
// json.Marshal (golden byte-exact цел).

import (
	"time"
)

// OperatorCreateReply — native 201-тело POST /v1/operators. SENSITIVE: jwt отдаётся
// один раз; secret-masking middleware вырезает его из логов/OTel/audit. roles — `*[]string`
// С omitempty (nil → ключ опущен). created_at — наносекундный time-wire.
type OperatorCreateReply struct {
	AID          string    `json:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedAt    time.Time `json:"created_at"`
	CreatedByAID string    `json:"created_by_aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	DisplayName  string    `json:"display_name"`
	JWT          string    `json:"jwt"`
	Roles        *[]string `json:"roles,omitempty"`
}

// Operator — native тело GET /v1/operators/{aid} (и element list-envelope). Форма 1:1
// с прежним wire: auth_method — native enum OperatorAuthMethod (schema `type: string`);
// created_by_aid/metadata/revoked_at — С omitempty (nil → ключ опущен). created_at —
// наносекундный time-wire. created_via — ВСЕГДА присутствует (NOT NULL DEFAULT 'user'
// в схеме, ADR-058(d)); плоская string с enum-тегом (домен совпадает с SQL CHECK
// `created_via_valid` и доменными константами operator.CreatedVia*) — для клиент-
// кодогена, БЕЗ named-типа (отличает источник заведения от auth_method).
type Operator struct {
	AID              string                  `json:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	AuthMethod       OperatorAuthMethod      `json:"auth_method"`
	BootstrapInitial bool                    `json:"bootstrap_initial"`
	CreatedAt        time.Time               `json:"created_at"`
	CreatedByAID     *string                 `json:"created_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CreatedVia       string                  `json:"created_via" enum:"bootstrap,user,ldap,oidc,system"`
	DisplayName      string                  `json:"display_name"`
	Metadata         *map[string]interface{} `json:"metadata,omitempty"`
	RevokedAt        *time.Time              `json:"revoked_at,omitempty"`
}

// IssueTokenReply — native 200-тело POST /v1/operators/{aid}/issue-token. SENSITIVE:
// jwt — never log. expires_at — наносекундный time-wire.
type IssueTokenReply struct {
	AID       string    `json:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	ExpiresAt time.Time `json:"expires_at"`
	JWT       string    `json:"jwt"`
}
