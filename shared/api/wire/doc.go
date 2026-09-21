// Package wire declares the request and reply bodies of the Operator API, once.
//
// WHY IT IS NOT IN keeper. These types used to live in `keeper/internal/api`,
// and `internal/` is a wall the compiler enforces around the keeper module — so
// every consumer outside it (soulctl, the e2e/e2e-live/e2e-k8s harnesses, the
// load generator) was FORCED to declare its own struct with hand-written json
// tags. That is a second description of one contract, and encoding/json makes
// the divergence silent: an unknown key decodes to a zero value, so a copy that
// has fallen behind still compiles and still passes. NIM-729 renamed one
// identifier `name` → `id` across ten registries and left `soulctl
// push-provider create` dead against `additionalProperties:false`; nothing
// failed until a human read the diff. Moved here under NIM-776, with the
// layout recorded in ADR-011 (amendment 2026-09-05).
//
// WHAT IS HERE AND WHAT IS NOT. Here: the bodies and the contract enums — the
// SHAPE. In keeper: the huma Operation metadata, the register functions, the
// handlers, the projections from the domain views, and the RFC 7807 problem
// package — the SERVER. keeper names these types through aliases in
// `keeper/internal/api/huma_wire_alias.go`; an alias declares no fields of its
// own, so it cannot drift from what it aliases.
//
// THE TAGS TRAVEL. huma reads `json` / `required` / `pattern` / `doc` / `enum`
// and the rest by reflection and does not care which package declares the type,
// and its DefaultSchemaNamer keys on the bare type name — so moving a type here
// changed no schema name and no byte of docs/keeper/openapi.yaml, which
// keeper's openapi drift guard is what proves.
//
// NO DEPENDENCIES, DELIBERATELY. This package imports nothing outside the
// standard library. `soul` requires `shared`, so anything pulled in here would
// reach the Soul binary and erode the isolation ADR-011 puts in the compiler;
// and soulctl stays a light client — adding `shared` to its go.mod added no
// entry to its go.sum at all.
//
// The three enums the UI references by $ref (SoulStatus, SoulTransport,
// IncarnationStatus) are declared here but emit their named schema from a
// keeper-local shim: huma reaches a named schema through a METHOD, and Go
// allows a method only in the declaring package. See
// keeper/internal/api/huma_enums.go.
//
// The guard that keeps a consumer from re-describing any of this is
// single_source_test.go.
package wire
