package api

// Доэмиссия typed-схемы SoulprintFacts (+ 6 под-схем) в агрегатор-спеку — Class C
// выравнивания (ADR-018 typed soulprint). По эталону ALIAS-механизма cadence-target
// (huma_cadence_envelope.go) / soul-envelope (huma_soul_envelope.go).
//
// ПРОБЛЕМА. GET /v1/souls/{sid}/soulprint несёт в Body handlers.SoulprintReadReply,
// поле typed_facts которого — json.RawMessage (byte-passthrough JSONB, категория D,
// ADR-051: сырые байты souls.soulprint_facts отдаются as-is, без unmarshal→map→
// re-marshal — forward-compat без рекомпиляции Keeper-а). reflect-обход huma по
// json.RawMessage не выводит вложенные типы → SoulprintFacts и 6 под-схем НЕ
// попадают в components/schemas, хотя рукопись (docs/keeper/openapi.yaml :6950) их
// объявляет, а UI keeper.ts на них ссылается.
//
// МЕХАНИЗМ (alias, как cadence-target — WIRE-БЕЗОПАСЕН).
// RegisterTypeAlias(handlers.SoulprintReadReply → soulprintReadReply): при встрече
// wire-типа в OUTPUT-структуре huma строит OpenAPI-схему через api-named-struct
// soulprintReadReply, поле typed_facts которого типизировано *SoulprintFacts (NATIVE) →
// huma РЕКУРСИВНО регистрирует SoulprintFacts + под-схемы (SoulprintOsFacts/
// SoulprintKernelFacts/SoulprintMemoryFacts/SoulprintNetworkFacts/SoulprintNetworkInterface/
// SoulprintCpuFacts) под их контрактными именами. Сериализация остаётся на handler-типе
// (json.RawMessage as-is) → wire-байты typed_facts НЕ меняются (golden TestGetSoulprint_
// BytePassthrough_Exact цел). Меняется ТОЛЬКО OpenAPI: схема SoulprintReadReply.typed_facts
// ссылается на $ref SoulprintFacts вместо free-form object, и 7 типизированных схем доезжают
// в components.
//
// handler-native T5d: native soulprint-типы определены ЗДЕСЬ (а не реюзятся из oapi/) —
// форма 1:1 с proto SoulprintFacts (ADR-018). Имя SoulprintCpuFacts — контрактное (рукопись
// :7009); прочие 6 имён совпадают с рукописью. wire не затронут (typed_facts byte-passthrough).

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === NATIVE typed-схемы SoulprintFacts (+ 6 под-схем), форма 1:1 с proto SoulprintFacts ===
// Используются ТОЛЬКО как source формы для OpenAPI-эмиссии (typed_facts на wire — byte-
// passthrough json.RawMessage; эти типы не сериализуются на горячем пути).

// SoulprintFacts — typed-факты Soulprint (ADR-018). Имя = контрактное имя схемы рукописи.
type SoulprintFacts struct {
	CPU      *SoulprintCpuFacts     `json:"cpu,omitempty"`
	Hostname *string                `json:"hostname,omitempty" doc:"короткое имя хоста, uname -n"`
	Kernel   *SoulprintKernelFacts  `json:"kernel,omitempty"`
	Memory   *SoulprintMemoryFacts  `json:"memory,omitempty" doc:"объёмы памяти в МБ"`
	Network  *SoulprintNetworkFacts `json:"network,omitempty"`
	Os       *SoulprintOsFacts      `json:"os,omitempty" doc:"факты об операционной системе (ADR-018)"`
	SID      *string                `json:"sid,omitempty" doc:"echo SID для логов; authority — mTLS peer cert"`
}

// SoulprintCpuFacts — под-факт CPU под КОНТРАКТНЫМ именем (рукопись :7009; oapi-генератор
// капитализировал бы аббревиатуру в SoulprintCPUFacts — здесь имя сразу контрактное).
type SoulprintCpuFacts struct {
	Count  *int32  `json:"count,omitempty" doc:"количество logical CPUs (с учётом HT/SMT)"`
	Model  *string `json:"model,omitempty"`
	Vendor *string `json:"vendor,omitempty"`
}

// SoulprintKernelFacts — факты ядра.
type SoulprintKernelFacts struct {
	Release *string `json:"release,omitempty" doc:"только версия ядра (5.15.0)"`
	Version *string `json:"version,omitempty" doc:"полная версия с dist-suffix (5.15.0-101-generic)"`
}

// SoulprintMemoryFacts — объёмы памяти в МБ.
type SoulprintMemoryFacts struct {
	AvailableMb *int64 `json:"available_mb,omitempty"`
	SwapMb      *int64 `json:"swap_mb,omitempty"`
	TotalMb     *int64 `json:"total_mb,omitempty"`
}

// SoulprintNetworkFacts — сетевые факты.
type SoulprintNetworkFacts struct {
	Fqdn       *string                      `json:"fqdn,omitempty"`
	Interfaces *[]SoulprintNetworkInterface `json:"interfaces,omitempty"`
	PrimaryIP  *string                      `json:"primary_ip,omitempty" doc:"основной IPv4 (интерфейс с default-route)"`
}

// SoulprintNetworkInterface — один сетевой интерфейс.
type SoulprintNetworkInterface struct {
	Ipv4 *[]string `json:"ipv4,omitempty" doc:"IPv4-адреса в CIDR (10.0.0.1/24)"`
	Ipv6 *[]string `json:"ipv6,omitempty"`
	Mac  *string   `json:"mac,omitempty"`
	Mtu  *int32    `json:"mtu,omitempty"`
	Name *string   `json:"name,omitempty"`
}

// SoulprintOsFacts — факты об операционной системе (ADR-018).
type SoulprintOsFacts struct {
	Arch       *string `json:"arch,omitempty" doc:"amd64 / arm64"`
	Codename   *string `json:"codename,omitempty"`
	Distro     *string `json:"distro,omitempty"`
	Family     *string `json:"family,omitempty" doc:"debian / rhel / alpine / windows / darwin"`
	InitSystem *string `json:"init_system,omitempty" doc:"systemd / openrc / sysv / launchd"`
	PkgMgr     *string `json:"pkg_mgr,omitempty" doc:"apt / dnf / apk / pacman"`
	Version    *string `json:"version,omitempty"`
}

// soulprintReadReply — alias-цель схемы GET /v1/souls/{sid}/soulprint 200-тела. Форма
// сверена с committed-рукописью (docs/keeper/openapi.yaml :6858 → SoulprintReadReply):
// sid/typed_facts (required) + collected_at/received_at (optional). ★ typed_facts здесь
// типизирован *SoulprintFacts (а НЕ json.RawMessage handler-типа) — ИМЕННО ради эмиссии
// SoulprintFacts + под-схем в components. Wire-тело сериализует handler-тип (json.RawMessage
// byte-passthrough), этот тип — только source формы для OpenAPI.
type soulprintReadReply struct {
	SID         string          `json:"sid" doc:"SID (FQDN) Soul-а"`
	TypedFacts  *SoulprintFacts `json:"typed_facts" doc:"typed-факты Soulprint (ADR-018); byte-passthrough JSONB на wire, форма по proto SoulprintFacts"`
	CollectedAt *time.Time      `json:"collected_at,omitempty" doc:"Soul-side timestamp момента сбора фактов"`
	ReceivedAt  *time.Time      `json:"received_at,omitempty" doc:"Keeper-side timestamp приёма стрима"`
}

// registerSoulprintFacts вешает alias доэмиссии typed-soulprint на registry. Вызывается в
// newHumaCadenceAPI для каждой собранной huma.API. Wire-тип GET soulprint (handlers.
// SoulprintReadReply с typed_facts=json.RawMessage) НЕ меняется — меняется ТОЛЬКО OpenAPI-
// схема (typed_facts → $ref SoulprintFacts) + эмиссия 7 типизированных схем (native).
func registerSoulprintFacts(api huma.API) {
	api.OpenAPI().Components.Schemas.RegisterTypeAlias(
		reflect.TypeFor[handlers.SoulprintReadReply](),
		reflect.TypeFor[soulprintReadReply](),
	)
}

// _ — guard wire-инварианта: handler-тип typed_facts остаётся json.RawMessage (alias
// меняет ТОЛЬКО схему, не сериализацию). Если рефактор сменит wire-поле на typed-struct,
// эта строка перестанет компилироваться → сигнал «wire-форма GET soulprint затронута».
var _ = json.RawMessage(handlers.SoulprintReadReply{}.TypedFacts)
