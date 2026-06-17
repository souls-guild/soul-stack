package api

// Вынос enum SoulStatus и SoulTransport в components/schemas как named-схемы с $ref
// (тираж-батч N5, ENUM-механизм по эталону huma_incarnation_status.go).
//
// ПРОБЛЕМА. huma DefaultSchemaNamer выносит в components/schemas (getsRef=true) ТОЛЬКО
// struct-типы; string-based named-тип (SoulStatus / SoulTransport) huma всегда
// ИНЛАЙНИТ как `type: string` БЕЗ $ref. Рукопись (docs/keeper/openapi.yaml :4198/:4207)
// объявляет SoulTransport и SoulStatus отдельными схемами с enum-значениями и ссылается на
// них через $ref (SoulListEntry.status/.transport, SoulCreateReply.status/.transport,
// SoulCovenAssignSelector.status) — UI ждёт именно named-схемы.
//
// МЕХАНИЗМ (huma-чистый, без правки генерёного oapi-пакета): на каждый enum — string-тип в
// пакете api с huma.SchemaProvider, регистрирующий named-схему и возвращающий $ref на неё, +
// RegisterTypeAlias доменного oapi-типа на наш SchemaProvider. wire-тип (строка) НЕ меняется,
// меняется лишь OpenAPI-схема: status/transport становятся $ref вместо инлайн `type: string`.
// Регистрация идемпотентна. Оба alias вызываются в newHumaCadenceAPI (общая фабрика всех
// huma.API).
//
// ENUM-СОСТАВ. Значения берутся из ДОМЕННОЙ истины (internal/soul: Status*/Transport*,
// validStatus/validTransport) — те же 6 статусов / 2 транспорта, что код уже эмитит инлайном
// на status-полях и валидирует доменно. Рукопись :4207 объявляет SoulStatus урезанным набором
// (pending/connected/disconnected/expired — без revoked/destroyed) — это пред-существующий
// контент-дрейф рукописи, НЕ наименование (см. отчёт N5); named-схема несёт полный доменный
// набор, как существующая инлайн-схема.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// soulStatusSchemaName / soulTransportSchemaName — контрактные имена named-схем (из рукописи;
// UI ссылается по $ref).
const (
	soulStatusSchemaName    = "SoulStatus"
	soulTransportSchemaName = "SoulTransport"
)

const (
	soulStatusSchemaRef    = "#/components/schemas/" + soulStatusSchemaName
	soulTransportSchemaRef = "#/components/schemas/" + soulTransportSchemaName
)

// soulStatusEnum — статусы Soul в реестре. Состав — доменная истина (internal/soul.validStatus):
// 6 значений. Рукопись :4207 несёт урезанный набор — named-схема следует домену (см. шапку).
var soulStatusEnum = []any{
	string(soul.StatusPending),
	string(soul.StatusConnected),
	string(soul.StatusDisconnected),
	string(soul.StatusRevoked),
	string(soul.StatusExpired),
	string(soul.StatusDestroyed),
}

// soulTransportEnum — способ доставки конфигурации. Состав — доменная истина
// (internal/soul.validTransport): agent / ssh.
var soulTransportEnum = []any{
	string(soul.TransportAgent),
	string(soul.TransportSSH),
}

const soulStatusDescription = "Статус Soul в реестре."

const soulTransportDescription = "Способ доставки конфигурации. agent — демон soul поверх " +
	"mTLS gRPC stream; ssh — push без агента."

	// SchemaProvider-цели — NATIVE enum-типы SoulStatus / SoulTransport (huma_enums.go, T5d-2c-full
	// Phase 1). native enum-типы сами реализуют huma.SchemaProvider (выносят named-схемы "SoulStatus"/
	// "SoulTransport" с доменным enum-набором и $ref). Константы schemaName/Ref/Enum/Description выше —
	// общая истина, читаемая методами Schema() native-типов.
	//
	// handler-native T5d: reply/get/list-Body несут native SoulStatus/SoulTransport НАПРЯМУЮ (поля
	// проецируются из доменных Soul*View плоских string-ов), поэтому отдельный RegisterTypeAlias
	// SoulStatus → native более НЕ нужен (нет ни одного SoulStatus-поля в reflected-Body).
