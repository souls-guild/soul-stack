package api

// Served mechanism of GET /openapi.yaml and GET /openapi.json. The spec is served from
// CODE — a runtime dump of the huma-operation aggregator ([HumaFullSpecYAML] /
// [buildFullOpenAPISpec], FastAPI-style), OpenAPI 3.1.0. There is one source —
// the huma aggregator; committed docs/keeper/openapi.yaml is its derived snapshot
// for the UI vendor (make gen-openapi), not a separate reference.
//
// YAML vs JSON: both variants are built from ONE source of truth (the huma
// aggregator). YAML (.YAML()) — for humans and tools; JSON (json.Marshal) — for
// the inline render of the /docs viewer (RapiDoc loadSpec expects an OBJECT, the JSON is parsed on
// the client; RapiDoc would treat YAML text as a URL). huma .YAML() itself under
// the hood marshals JSON → converts to YAML, so both formats are equivalent.
//
// CACHE: huma reflection is expensive, and the spec is immutable for the process lifetime (operations
// are registered statically). So each dump is built EXACTLY ONCE
// via sync.Once on first access and cached in a []byte; every
// subsequent GET returns the buffer without rebuilding.
//
// SECURITY: both routes are behind JWT (security in the spec) — mounted by a DIRECT
// chi mount OUTSIDE the /v1 group with RequireJWT, so they carry NO RBAC/audit (see
// router.go).

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
)

// contentTypeOpenAPI — application/yaml per RFC 9512. Kept locally so the
// served handler is self-contained.
const contentTypeOpenAPI = "application/yaml; charset=utf-8"

// contentTypeOpenAPIJSON — application/json for the JSON variant of the spec (inline
// RapiDoc render on /docs).
const contentTypeOpenAPIJSON = "application/json; charset=utf-8"

// servedSpec lazily builds and caches the YAML dump of the full huma spec. buildErr
// is recorded once: if the first build failed (merge collision — aggregator a/b
// gate), the handler returns 500 rather than rebuilding on every request.
var servedSpec struct {
	once  sync.Once
	bytes []byte
	err   error
}

// servedSpecJSON — the JSON analog of servedSpec (same source of truth, a separate
// cache and Once, since the format differs).
var servedSpecJSON struct {
	once  sync.Once
	bytes []byte
	err   error
}

// openAPISpecBytes returns the cached YAML dump of the full huma spec,
// building it on first call. Thread-safe (sync.Once).
func openAPISpecBytes() ([]byte, error) {
	servedSpec.once.Do(func() {
		y, err := HumaFullSpecYAML()
		if err != nil {
			servedSpec.err = err
			return
		}
		servedSpec.bytes = []byte(y)
	})
	return servedSpec.bytes, servedSpec.err
}

// openAPISpecJSONBytes returns the cached JSON dump of the full huma spec.
// The source of truth is the same buildFullOpenAPISpec as YAML; *huma.OpenAPI
// implements MarshalJSON, so json.Marshal yields canonical 3.1 JSON.
func openAPISpecJSONBytes() ([]byte, error) {
	servedSpecJSON.once.Do(func() {
		spec, err := buildFullOpenAPISpec()
		if err != nil {
			servedSpecJSON.err = err
			return
		}
		b, err := json.Marshal(spec)
		if err != nil {
			servedSpecJSON.err = err
			return
		}
		servedSpecJSON.bytes = b
	})
	return servedSpecJSON.bytes, servedSpecJSON.err
}

// servedOpenAPIHandler serves the cached 3.1 dump of the huma spec with status 200
// (Content-Type application/yaml, Content-Length from the buffer length). GET is the
// only method; router.go mounts it via r.Get(...). If the aggregator didn't build
// (merge collision) — 500 without a spec body.
func servedOpenAPIHandler(w http.ResponseWriter, _ *http.Request) {
	spec, err := openAPISpecBytes()
	if err != nil {
		http.Error(w, "openapi spec assembly failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeOpenAPI)
	w.Header().Set("Content-Length", strconv.Itoa(len(spec)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec)
}

// servedOpenAPIJSONHandler — the JSON analog of servedOpenAPIHandler. Needed by the
// /docs viewer: RapiDoc.loadSpec accepts an OBJECT, the page parses this JSON and feeds the
// parsed object (RapiDoc would treat a string as a spec URL). 200/500 as in
// the YAML variant.
func servedOpenAPIJSONHandler(w http.ResponseWriter, _ *http.Request) {
	spec, err := openAPISpecJSONBytes()
	if err != nil {
		http.Error(w, "openapi spec assembly failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeOpenAPIJSON)
	w.Header().Set("Content-Length", strconv.Itoa(len(spec)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec)
}
