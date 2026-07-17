package api

// Additional emission of the typed SoulprintFacts schema (+ 6 sub-schemas) into the aggregate
// spec — a Class C alignment (ADR-018 typed soulprint). Following the ALIAS mechanism reference
// of cadence-target (huma_cadence_envelope.go) / soul-envelope (huma_soul_envelope.go).
//
// PROBLEM. GET /v1/souls/{sid}/soulprint carries handlers.SoulprintReadReply in the Body,
// whose typed_facts field is json.RawMessage (byte-passthrough JSONB, category D,
// ADR-051: the raw bytes of souls.soulprint_facts are returned as-is, without unmarshal→map→
// re-marshal — forward-compat without recompiling the Keeper). huma's reflect walk over
// json.RawMessage does not surface the nested types → SoulprintFacts and the 6 sub-schemas do
// NOT land in components/schemas, even though the hand-written spec (docs/keeper/openapi.yaml :6950)
// declares them and the UI keeper.ts references them.
//
// MECHANISM (alias, like cadence-target — WIRE-SAFE).
// RegisterTypeAlias(handlers.SoulprintReadReply → soulprintReadReply): on encountering the
// wire type in an OUTPUT struct, huma builds the OpenAPI schema via the api-named struct
// soulprintReadReply, whose typed_facts field is typed *SoulprintFacts (NATIVE) →
// huma RECURSIVELY registers SoulprintFacts + the sub-schemas (SoulprintOsFacts/
// SoulprintKernelFacts/SoulprintMemoryFacts/SoulprintNetworkFacts/SoulprintNetworkInterface/
// SoulprintCpuFacts) under their contract names. Serialization stays on the handler type
// (json.RawMessage as-is) → the wire bytes of typed_facts do NOT change (golden TestGetSoulprint_
// BytePassthrough_Exact intact). ONLY the OpenAPI changes: the SoulprintReadReply.typed_facts schema
// references $ref SoulprintFacts instead of a free-form object, and the 7 typed schemas make it
// into components.
//
// handler-native T5d: the native soulprint types are defined HERE (not reused from oapi/) —
// shape 1:1 with proto SoulprintFacts (ADR-018). The name SoulprintCpuFacts is the contract one
// (hand-written spec :7009); the other 6 names match the hand-written spec. The wire is untouched (typed_facts byte-passthrough).

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === NATIVE typed SoulprintFacts schemas (+ 6 sub-schemas), shape 1:1 with proto SoulprintFacts ===
// Used ONLY as the shape source for OpenAPI emission (typed_facts on the wire is byte-
// passthrough json.RawMessage; these types are not serialized on the hot path).

// SoulprintFacts — typed Soulprint facts (ADR-018). Name = the contract schema name from the hand-written spec.
type SoulprintFacts struct {
	CPU      *SoulprintCpuFacts     `json:"cpu,omitempty"`
	Hostname *string                `json:"hostname,omitempty" doc:"short hostname, uname -n"`
	Kernel   *SoulprintKernelFacts  `json:"kernel,omitempty"`
	Memory   *SoulprintMemoryFacts  `json:"memory,omitempty" doc:"memory amounts in MB"`
	Network  *SoulprintNetworkFacts `json:"network,omitempty"`
	Os       *SoulprintOsFacts      `json:"os,omitempty" doc:"operating-system facts (ADR-018)"`
	SID      *string                `json:"sid,omitempty" doc:"echo SID for logs; authority - mTLS peer cert"`
}

// SoulprintCpuFacts — the CPU sub-fact under the CONTRACT name (hand-written spec :7009; the oapi
// generator would capitalize the acronym into SoulprintCPUFacts — here the name is contract from the start).
type SoulprintCpuFacts struct {
	Count  *int32  `json:"count,omitempty" doc:"number of logical CPUs (accounting for HT/SMT)"`
	Model  *string `json:"model,omitempty"`
	Vendor *string `json:"vendor,omitempty"`
}

// SoulprintKernelFacts — kernel facts.
type SoulprintKernelFacts struct {
	Release *string `json:"release,omitempty" doc:"kernel version only (5.15.0)"`
	Version *string `json:"version,omitempty" doc:"full version with dist-suffix (5.15.0-101-generic)"`
}

// SoulprintMemoryFacts — memory amounts in MB.
type SoulprintMemoryFacts struct {
	AvailableMb *int64 `json:"available_mb,omitempty"`
	SwapMb      *int64 `json:"swap_mb,omitempty"`
	TotalMb     *int64 `json:"total_mb,omitempty"`
}

// SoulprintNetworkFacts — network facts.
type SoulprintNetworkFacts struct {
	Fqdn       *string                      `json:"fqdn,omitempty"`
	Interfaces *[]SoulprintNetworkInterface `json:"interfaces,omitempty"`
	PrimaryIP  *string                      `json:"primary_ip,omitempty" doc:"primary IPv4 (interface with default route)"`
}

// SoulprintNetworkInterface — a single network interface.
type SoulprintNetworkInterface struct {
	Ipv4 *[]string `json:"ipv4,omitempty" doc:"IPv4 addresses in CIDR (10.0.0.1/24)"`
	Ipv6 *[]string `json:"ipv6,omitempty"`
	Mac  *string   `json:"mac,omitempty"`
	Mtu  *int32    `json:"mtu,omitempty"`
	Name *string   `json:"name,omitempty"`
}

// SoulprintOsFacts — operating-system facts (ADR-018).
type SoulprintOsFacts struct {
	Arch       *string `json:"arch,omitempty" doc:"amd64 / arm64"`
	Codename   *string `json:"codename,omitempty"`
	Distro     *string `json:"distro,omitempty"`
	Family     *string `json:"family,omitempty" doc:"debian / rhel / alpine / windows / darwin"`
	InitSystem *string `json:"init_system,omitempty" doc:"systemd / openrc / sysv / launchd"`
	PkgMgr     *string `json:"pkg_mgr,omitempty" doc:"apt / dnf / apk / pacman"`
	Version    *string `json:"version,omitempty"`
}

// soulprintReadReply — the alias target of the GET /v1/souls/{sid}/soulprint 200-body schema. The shape
// is checked against the committed hand-written spec (docs/keeper/openapi.yaml :6858 → SoulprintReadReply):
// sid/typed_facts (required) + collected_at/received_at (optional). ★ typed_facts here is
// typed *SoulprintFacts (NOT the json.RawMessage of the handler type) — PRECISELY to emit
// SoulprintFacts + the sub-schemas into components. The wire body is serialized by the handler type
// (json.RawMessage byte-passthrough); this type is only the shape source for OpenAPI.
type soulprintReadReply struct {
	SID         string          `json:"sid" doc:"SID (FQDN) of Soul"`
	TypedFacts  *SoulprintFacts `json:"typed_facts" doc:"typed Soulprint facts (ADR-018); byte-passthrough JSONB on wire, shaped per proto SoulprintFacts"`
	CollectedAt *time.Time      `json:"collected_at,omitempty" doc:"Soul-side timestamp of fact collection"`
	ReceivedAt  *time.Time      `json:"received_at,omitempty" doc:"Keeper-side timestamp of stream receipt"`
}

// registerSoulprintFacts hangs the typed-soulprint additional-emission alias on the registry. Called in
// newHumaCadenceAPI for each assembled huma.API. The wire type of GET soulprint (handlers.
// SoulprintReadReply with typed_facts=json.RawMessage) does NOT change — ONLY the OpenAPI
// schema changes (typed_facts → $ref SoulprintFacts) + emission of the 7 typed schemas (native).
func registerSoulprintFacts(api huma.API) {
	api.OpenAPI().Components.Schemas.RegisterTypeAlias(
		reflect.TypeFor[handlers.SoulprintReadReply](),
		reflect.TypeFor[soulprintReadReply](),
	)
}

// _ — a wire-invariant guard: the handler type's typed_facts stays json.RawMessage (the alias
// changes ONLY the schema, not serialization). If a refactor switches the wire field to a typed struct,
// this line stops compiling → a signal that "the GET soulprint wire shape is affected".
var _ = json.RawMessage(handlers.SoulprintReadReply{}.TypedFacts)
