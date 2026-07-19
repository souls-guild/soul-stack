package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/api"
)

// fakeReadPool — a narrow [SoulPool] mock for GET /v1/souls/{sid} and
// GET /v1/souls/{sid}/soulprint. Kept separate from the broader [fakeSoulPool] to
// avoid multiplying SQL matchers in a shared fake — the read handlers rest on two
// queries (SelectBySID and SelectSoulprint).
type fakeReadPool struct {
	soul         *soul.Soul
	soulFactsRaw []byte
	collectedAt  *time.Time
	receivedAt   *time.Time

	// If set, SelectSoulprint returns a row where `soulprint_facts IS NULL`
	// (i.e. the Soul record exists but there are no facts) — exercises the 410 branch.
	soulprintEmpty bool

	// soulMissing=true → SelectBySID and SelectSoulprint return pgx.ErrNoRows.
	soulMissing bool
}

func (f *fakeReadPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("fakeReadPool: BeginTx not expected for read handlers")
}

func (f *fakeReadPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("fakeReadPool: Exec not expected")
}

func (f *fakeReadPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeReadPool: Query not expected")
}

func (f *fakeReadPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "soulprint_facts") && strings.Contains(sql, "WHERE sid = $1"):
		if f.soulMissing {
			return errRow{err: pgx.ErrNoRows}
		}
		sid := "soul.example.com"
		if f.soul != nil {
			sid = f.soul.SID
		}
		// staticRow.Scan(*[]byte) does r.values[i].([]byte) with no nil check: for a
		// NULL column we emulate "empty bytes" (len==0), equivalent to pgx behavior
		// for NULL bytea/jsonb (see SelectSoulprint → the len(factsJSON) == 0 check).
		// Timestamp pointers — nil is allowed (there is a **time.Time case).
		factsArg := []byte{}
		if !f.soulprintEmpty {
			if len(f.soulFactsRaw) > 0 {
				factsArg = f.soulFactsRaw
			} else {
				factsArg = []byte(`{"sid":"` + sid + `","os":{"family":"debian","pkg_mgr":"apt","init_system":"systemd"}}`)
			}
		}
		var collectedAtArg, receivedAtArg any
		if f.collectedAt != nil {
			collectedAtArg = *f.collectedAt
		}
		if f.receivedAt != nil {
			receivedAtArg = *f.receivedAt
		}
		return staticRow{values: []any{sid, factsArg, collectedAtArg, receivedAtArg}}

	case strings.Contains(sql, "FROM souls") && strings.Contains(sql, "WHERE sid = $1"):
		if f.soulMissing || f.soul == nil {
			return errRow{err: pgx.ErrNoRows}
		}
		s := f.soul
		var lastSeenAt, requestedAt any
		var lastSeenByKID, createdByAID, note any
		if s.LastSeenAt != nil {
			lastSeenAt = *s.LastSeenAt
		}
		if s.LastSeenByKID != nil {
			lastSeenByKID = *s.LastSeenByKID
		}
		if s.CreatedByAID != nil {
			createdByAID = *s.CreatedByAID
		}
		if s.RequestedAt != nil {
			requestedAt = *s.RequestedAt
		}
		if s.Note != "" {
			note = s.Note
		}
		// traits jsonb (ADR-060): nil Traits → NULL bytes (scanSoul → an empty map);
		// set traits are marshaled, as in a real pgx scan.
		traitsArg := []byte(nil)
		if len(s.Traits) > 0 {
			b, err := json.Marshal(s.Traits)
			if err != nil {
				return errRow{err: err}
			}
			traitsArg = b
		}
		return staticRow{values: []any{
			s.SID, string(s.Transport), string(s.Status), s.Coven,
			traitsArg,
			s.RegisteredAt, lastSeenAt, lastSeenByKID, createdByAID, requestedAt, note,
		}}
	}
	return errRow{err: errors.New("fakeReadPool: unexpected SQL: " + sql)}
}

// recordGet calls GetTyped directly (handler-native T5d) and serializes the result
// into the recorder through the same writeJSON/writeProblemError as the former (w,r)
// route — preserving the status/body invariants of the downstream asserts. claims=nil
// → fail-closed single-read.
func recordGet(t *testing.T, h *SoulHandler, sid, aid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/souls/"+sid, nil)
	var claims *jwt.Claims
	if aid != "" {
		claims = claimsFor(aid)
	}
	rec := httptest.NewRecorder()
	view, err := h.GetTyped(req.Context(), claims, sid)
	if err != nil {
		writeProblemError(rec, req, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, soulListViewJSON(view), h.logger)
	return rec
}

// doGetSoul assembles GET /v1/souls/{sid} (no claims — fail-closed).
func doGetSoul(t *testing.T, h *SoulHandler, sid string) *httptest.ResponseRecorder {
	t.Helper()
	return recordGet(t, h, sid, "")
}

// doGetSoulScoped — doGetSoul with injected claims (single-read scope resolution).
func doGetSoulScoped(t *testing.T, h *SoulHandler, sid, aid string) *httptest.ResponseRecorder {
	t.Helper()
	return recordGet(t, h, sid, aid)
}

// recordGetSoulprint calls GetSoulprintTyped directly and serializes the result into the recorder.
func recordGetSoulprint(t *testing.T, h *SoulHandler, sid, aid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/souls/"+sid+"/soulprint", nil)
	var claims *jwt.Claims
	if aid != "" {
		claims = claimsFor(aid)
	}
	rec := httptest.NewRecorder()
	resp, err := h.GetSoulprintTyped(req.Context(), claims, sid)
	if err != nil {
		writeProblemError(rec, req, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, resp, h.logger)
	return rec
}

// doGetSoulprint assembles GET /v1/souls/{sid}/soulprint (no claims).
func doGetSoulprint(t *testing.T, h *SoulHandler, sid string) *httptest.ResponseRecorder {
	t.Helper()
	return recordGetSoulprint(t, h, sid, "")
}

// doGetSoulprintScoped — doGetSoulprint with injected claims (scope resolution).
func doGetSoulprintScoped(t *testing.T, h *SoulHandler, sid, aid string) *httptest.ResponseRecorder {
	t.Helper()
	return recordGetSoulprint(t, h, sid, aid)
}

// soulListViewJSON projects the domain SoulListView into a map with the same json
// keys as the former wire (for the test's downstream asserts on the fields of the
// GET /v1/souls/{sid} response). Reproduces the native SoulListEntry shape
// (covens/traits non-null, nullable as null).
func soulListViewJSON(v SoulListView) map[string]any {
	m := map[string]any{
		"sid":              v.SID,
		"transport":        v.Transport,
		"status":           v.Status,
		"covens":           v.Covens,
		"traits":           v.Traits,
		"registered_at":    v.RegisteredAt,
		"created_by_aid":   v.CreatedByAID,
		"last_seen_at":     v.LastSeenAt,
		"last_seen_by_kid": v.LastSeenByKid,
		"requested_at":     v.RequestedAt,
	}
	return m
}

func TestGetSoul_Happy(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	pool := &fakeReadPool{soul: &soul.Soul{
		SID:          "soul.example.com",
		Transport:    soul.TransportAgent,
		Status:       soul.StatusConnected,
		Coven:        []string{"dev"},
		RegisteredAt: now,
	}}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doGetSoulScoped(t, h, "soul.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["sid"] != "soul.example.com" || out["transport"] != "agent" || out["status"] != "connected" {
		t.Errorf("body = %v, missing expected fields", out)
	}
	covens, _ := out["covens"].([]any)
	if len(covens) != 1 || covens[0] != "dev" {
		t.Errorf("covens = %v, want [dev]", out["covens"])
	}
	// bare-soul with no traits → `{}` (object), not null/a missing key (ADR-060
	// read-path coalesceTraits): the UI renders an empty set without a nil check.
	traits, ok := out["traits"].(map[string]any)
	if !ok {
		t.Fatalf("traits = %v (%T), want object {}", out["traits"], out["traits"])
	}
	if len(traits) != 0 {
		t.Errorf("traits = %v, want empty {}", traits)
	}
}

// TestGetSoul_WithTraits — the detail response carries operator-set traits as an
// object (ADR-060 read-path: souls.traits jsonb → SoulListView.Traits → SoulListEntry).
func TestGetSoul_WithTraits(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	pool := &fakeReadPool{soul: &soul.Soul{
		SID:          "soul.example.com",
		Transport:    soul.TransportAgent,
		Status:       soul.StatusConnected,
		Coven:        []string{"dev"},
		Traits:       map[string]any{"tier": "gold", "zones": []any{"a", "b"}},
		RegisteredAt: now,
	}}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doGetSoulScoped(t, h, "soul.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	traits, ok := out["traits"].(map[string]any)
	if !ok {
		t.Fatalf("traits = %v (%T), want object", out["traits"], out["traits"])
	}
	if traits["tier"] != "gold" {
		t.Errorf("traits[tier] = %v, want gold", traits["tier"])
	}
	zones, _ := traits["zones"].([]any)
	if len(zones) != 2 || zones[0] != "a" || zones[1] != "b" {
		t.Errorf("traits[zones] = %v, want [a b]", traits["zones"])
	}
}

func TestGetSoul_NotFound(t *testing.T) {
	pool := &fakeReadPool{soulMissing: true}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoul(t, h, "ghost.example.com")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetSoul_InvalidSID(t *testing.T) {
	pool := &fakeReadPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoul(t, h, "InvalidUPPER")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// --- single-read scope gate (ADR-047 S3b-1): out-of-scope → 404 ---

// scopedReadPool — a narrow read-pool with a host in covens=[prod] for scope tests.
func scopedReadPool() *fakeReadPool {
	return &fakeReadPool{soul: &soul.Soul{
		SID:          "prod-01.example.com",
		Transport:    soul.TransportAgent,
		Status:       soul.StatusConnected,
		Coven:        []string{"prod"},
		RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}}
}

// TestGetSoul_InScope_CovenMatch — a scoped operator (covens=[prod]) reads a host in
// its own coven → 200.
func TestGetSoul_InScope_CovenMatch(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{covens: []string{"prod"}}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_OutOfScope_404 — the MAIN security invariant S3b-1: a scoped operator
// (covens=[staging]) reads ANOTHER's host (covens=[prod]) → 404 (NOT 403, NOT 200):
// we do not reveal the existence of a foreign host. Regression = a scoped operator
// reads a foreign host by direct SID.
func TestGetSoul_OutOfScope_404(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{covens: []string{"staging"}}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-eve")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (out-of-scope does not leak existence); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_InScope_HostGlobMatch_200 — a host-glob-scoped operator
// (host matches prod-*) reads a matching host (prod-01) → 200. The single-read
// gate uses the same boolean scope as List, so a host visible in List by its
// host predicate is also reachable by a direct GET /{sid}.
func TestGetSoul_InScope_HostGlobMatch_200(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{exprs: []string{"host matches prod-*"}}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-webops")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (glob prod-* matches prod-01); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_OutOfHostGlobScope_404 — a host-glob-scoped operator
// (host matches web-*) reads a NON-matching host (prod-01) → 404 (does not
// reveal existence outside the host boundary).
func TestGetSoul_OutOfHostGlobScope_404(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{exprs: []string{"host matches web-*"}}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-webops")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (glob web-* does NOT match prod-01); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoulprint_InScope_HostGlobMatch_200 — soulprint under the same host-glob
// predicate: a matching host → 200, facts revealed.
func TestGetSoulprint_InScope_HostGlobMatch_200(t *testing.T) {
	pool := scopedReadPool()
	pool.soulFactsRaw = []byte(`{"sid":"prod-01.example.com","os":{"family":"debian"}}`)
	h := NewSoulHandler(pool, fakeScoper{exprs: []string{"host matches prod-*"}}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "prod-01.example.com", "archon-webops")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (glob match); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_Unrestricted_200 — an Unrestricted operator reads any host → 200.
func TestGetSoul_Unrestricted_200(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-root")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_EmptyPurview_404 — Purview{} (no rights, fail-closed) → 404 (not 200,
// not 403): the operator is entitled to no host.
func TestGetSoul_EmptyPurview_404(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{empty: true}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-nobody")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (fail-closed); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_NoClaims_404 — no claims (a defensive invariant; normally the route is
// under RequireJWT) → 404 (fail-closed, symmetric with List no-claims).
func TestGetSoul_NoClaims_404(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoul(t, h, "prod-01.example.com") // no claims.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no claims → fail-closed); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoulprint_OutOfScope_404 — the same-dimension scope gate on a soulprint read:
// a foreign host → 404 BEFORE any facts are revealed.
func TestGetSoulprint_OutOfScope_404(t *testing.T) {
	pool := scopedReadPool()
	pool.soulFactsRaw = []byte(`{"sid":"prod-01.example.com","os":{"family":"debian"}}`)
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"staging"}}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "prod-01.example.com", "archon-eve")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (soulprint out-of-scope); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoulprint_InScope_200 — soulprint of one's own coven → 200.
func TestGetSoulprint_InScope_200(t *testing.T) {
	pool := scopedReadPool()
	pool.soulFactsRaw = []byte(`{"sid":"prod-01.example.com","os":{"family":"debian"}}`)
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"prod"}}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "prod-01.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoulprint_EmptyPurview_404 — no rights → 404 (fail-closed).
func TestGetSoulprint_EmptyPurview_404(t *testing.T) {
	pool := scopedReadPool()
	pool.soulFactsRaw = []byte(`{"sid":"prod-01.example.com","os":{"family":"debian"}}`)
	h := NewSoulHandler(pool, fakeScoper{empty: true}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "prod-01.example.com", "archon-nobody")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (fail-closed); body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetSoulprint_Happy(t *testing.T) {
	collected := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	received := collected.Add(2 * time.Second)
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID: "soul.example.com", Transport: soul.TransportAgent,
			Status: soul.StatusConnected, Coven: []string{"dev"},
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
		soulFactsRaw: []byte(`{"sid":"soul.example.com","hostname":"soul","os":{"family":"debian","distro":"ubuntu","version":"22.04","arch":"amd64","pkg_mgr":"apt","init_system":"systemd"}}`),
		collectedAt:  &collected,
		receivedAt:   &received,
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doGetSoulprintScoped(t, h, "soul.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		SID         string         `json:"sid"`
		TypedFacts  map[string]any `json:"typed_facts"`
		CollectedAt string         `json:"collected_at"`
		ReceivedAt  string         `json:"received_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SID != "soul.example.com" {
		t.Errorf("sid = %q, want soul.example.com", out.SID)
	}
	osMap, ok := out.TypedFacts["os"].(map[string]any)
	if !ok {
		t.Fatalf("typed_facts.os = %v, want object", out.TypedFacts["os"])
	}
	if osMap["pkg_mgr"] != "apt" || osMap["init_system"] != "systemd" {
		t.Errorf("os = %v, want pkg_mgr=apt init_system=systemd", osMap)
	}
	if out.CollectedAt == "" || out.ReceivedAt == "" {
		t.Errorf("collected_at/received_at empty: %+v", out)
	}
}

func TestGetSoulprint_NotFound(t *testing.T) {
	pool := &fakeReadPool{soulMissing: true}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoulprint(t, h, "ghost.example.com")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetSoulprint_NotReceived_410(t *testing.T) {
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID: "soul.example.com", Transport: soul.TransportAgent,
			Status: soul.StatusConnected, Coven: []string{"dev"},
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
		soulprintEmpty: true,
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "soul.example.com", "archon-alice")
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410; body=%s", rec.Code, rec.Body.String())
	}
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	pType, _ := p["type"].(string)
	if !strings.Contains(pType, "soulprint-not-received") {
		t.Errorf("problem type = %q, want soulprint-not-received", pType)
	}
}

// TestGetSoulprint_BytePassthrough_Exact — a byte-passthrough guard (finding 2,
// category D of ADR-051): the raw JSONB bytes of `souls.soulprint_facts` reach the
// wire in typed_facts WITHOUT unmarshal→map→re-marshal. The proof is byte-for-byte:
// raw carries a non-alphabetical key order (`sid` before `os`, and inside os
// `pkg_mgr` before `init_system`); the former map path would have re-sorted them
// lexicographically, passthrough returns them exactly as stored. We assert the exact
// raw substring in the body.
func TestGetSoulprint_BytePassthrough_Exact(t *testing.T) {
	// Non-alphabetical order: a map-re-marshal would give init_system before pkg_mgr
	// and os before sid — passthrough keeps the original.
	raw := []byte(`{"sid":"soul.example.com","os":{"pkg_mgr":"apt","init_system":"systemd"}}`)
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID: "soul.example.com", Transport: soul.TransportAgent,
			Status: soul.StatusConnected, Coven: []string{"dev"},
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
		soulFactsRaw: raw,
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "soul.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// typed_facts in the body must be BYTE-FOR-BYTE equal to raw (not re-sorted).
	var out struct {
		TypedFacts json.RawMessage `json:"typed_facts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(out.TypedFacts) != string(raw) {
		t.Fatalf("typed_facts byte-passthrough broken:\n got = %s\nwant = %s", out.TypedFacts, raw)
	}
}

// TestGetSoulprint_ForwardCompat_UnknownKey — a forward-compat guard (finding 2): an
// extension key that is NOT in proto SoulprintFacts IS PRESENT on the wire. Proves
// Keeper does not parse/filter the contents of typed_facts — future proto fields of
// the Soul agent reach the client without recompiling Keeper.
func TestGetSoulprint_ForwardCompat_UnknownKey(t *testing.T) {
	// `future_field` is absent from SoulprintFacts (proto/keeper/v1/soulprint.proto).
	raw := []byte(`{"sid":"soul.example.com","future_field":{"nested":42}}`)
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID: "soul.example.com", Transport: soul.TransportAgent,
			Status: soul.StatusConnected, Coven: []string{"dev"},
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
		soulFactsRaw: raw,
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "soul.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		TypedFacts map[string]any `json:"typed_facts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ff, ok := out.TypedFacts["future_field"].(map[string]any)
	if !ok {
		t.Fatalf("future_field absent on wire (passthrough broken): typed_facts=%v", out.TypedFacts)
	}
	if ff["nested"] != float64(42) {
		t.Errorf("future_field.nested = %v, want 42 (passthrough must keep unknown content verbatim)", ff["nested"])
	}
}

// --- GET /v1/souls/{sid}/history (SelectHistory) ---

// fakeHistoryPool — a [SoulPool] mock for GET /v1/souls/{sid}/history. QueryRow serves
// TWO queries: the scope-gate (SelectBySID — a souls row with covens, ADR-047 G1 Fix
// 3) and the count SQL (total). Query — the page SQL (historyRows). Captures the
// SQL/args of the page call to verify filters (type/since/pagination).
type fakeHistoryPool struct {
	total    int
	items    []soul.HistoryItem
	queryErr error

	// hostCovens — the host's covens for the scope-gate InScope (Fix 3). nil → a host
	// with empty covens (an unrestricted operator sees it, a coven-scoped one does not).
	hostCovens []string
	// soulMissing=true → SelectBySID returns pgx.ErrNoRows (404 before the history fetch).
	soulMissing bool

	querySQL  string
	queryArgs []any
}

func (f *fakeHistoryPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("fakeHistoryPool: BeginTx not expected")
}

func (f *fakeHistoryPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("fakeHistoryPool: Exec not expected")
}

func (f *fakeHistoryPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	// scope-gate (SelectBySID) — a souls row with covens; runs BEFORE the history count SQL.
	if strings.Contains(sql, "FROM souls") && strings.Contains(sql, "WHERE sid = $1") {
		if f.soulMissing {
			return errRow{err: pgx.ErrNoRows}
		}
		return staticRow{values: []any{
			"soul.example.com", "agent", "connected", f.hostCovens,
			[]byte(nil), // traits jsonb (ADR-060): NULL → an empty map in scanSoul
			time.Now(), nil, nil, nil, nil, nil,
		}}
	}
	return staticRow{values: []any{f.total}}
}

func (f *fakeHistoryPool) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.querySQL = sql
	f.queryArgs = args
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return &historyRows{items: f.items}, nil
}

// historyRows — pgx.Rows over []soul.HistoryItem, emulates the 9-column projection of
// SelectHistory (type,id,incarnation,scenario,module,status,started_at,finished_at,
// voyage_id) with nullable pointers.
type historyRows struct {
	items []soul.HistoryItem
	idx   int
}

func (r *historyRows) Next() bool {
	if r.idx >= len(r.items) {
		return false
	}
	r.idx++
	return true
}

func (r *historyRows) Scan(dest ...any) error {
	it := r.items[r.idx-1]
	*dest[0].(*string) = string(it.Type)
	*dest[1].(*string) = it.ID
	*dest[2].(**string) = nullableStr(it.Incarnation)
	*dest[3].(**string) = nullableStr(it.Scenario)
	*dest[4].(**string) = nullableStr(it.Module)
	*dest[5].(*string) = it.Status
	*dest[6].(*time.Time) = it.StartedAt
	*dest[7].(**time.Time) = it.FinishedAt
	*dest[8].(**string) = it.VoyageID
	return nil
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (r *historyRows) Err() error                                   { return nil }
func (r *historyRows) Close()                                       {}
func (r *historyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *historyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *historyRows) Values() ([]any, error)                       { return nil, nil }
func (r *historyRows) RawValues() [][]byte                          { return nil }
func (r *historyRows) Conn() *pgx.Conn                              { return nil }

// doGetHistory calls HistoryTyped directly (handler-native T5d), parsing rawQuery
// (type[]/since/offset/limit) the same way as the former (w,r) route, and serializing
// the result into the recorder. The History scope gate resolves readScope from claims
// (ADR-047 G1 Fix 3).
func doGetHistory(t *testing.T, h *SoulHandler, sid, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v1/souls/" + sid + "/history"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	q := req.URL.Query()
	page, perr := api.ParsePage(q)
	if perr != nil {
		problem.Write(rec, problem.New(problem.TypeMalformedRequest, req.URL.Path, perr.Error()))
		return rec
	}
	var since time.Time
	if s := q.Get("since"); s != "" {
		ts, e := time.Parse(time.RFC3339, s)
		if e != nil {
			problem.Write(rec, problem.New(problem.TypeValidationFailed, req.URL.Path, "query 'since' must be RFC3339 timestamp"))
			return rec
		}
		since = ts
	}
	view, err := h.HistoryTyped(req.Context(), claimsFor("archon-alice"), SoulHistoryInput{
		SID:    sid,
		Types:  q["type"],
		Since:  since,
		Offset: page.Offset,
		Limit:  page.Limit,
	})
	if err != nil {
		writeProblemError(rec, req, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, soulHistoryViewJSON(view), h.logger)
	return rec
}

// soulHistoryViewJSON projects the domain SoulHistoryView into a map with the same
// json keys as the former wire (for the test's downstream asserts). Reproduces the
// native SoulHistoryReply shape (sid/items/offset/limit/total; per-item
// pointer-omitempty).
func soulHistoryViewJSON(v SoulHistoryView) map[string]any {
	items := make([]map[string]any, 0, len(v.Items))
	for _, it := range v.Items {
		m := map[string]any{
			"type":       it.Type,
			"id":         it.ID,
			"status":     it.Status,
			"started_at": it.StartedAt,
		}
		if it.FinishedAt != nil {
			m["finished_at"] = *it.FinishedAt
		}
		if it.Incarnation != nil {
			m["incarnation"] = *it.Incarnation
		}
		if it.Scenario != nil {
			m["scenario"] = *it.Scenario
		}
		if it.Module != nil {
			m["module"] = *it.Module
		}
		if it.VoyageID != nil {
			m["voyage_id"] = *it.VoyageID
		}
		items = append(items, m)
	}
	return map[string]any{
		"sid":    v.SID,
		"items":  items,
		"offset": v.Offset,
		"limit":  v.Limit,
		"total":  v.Total,
	}
}

type historyReplyDTO struct {
	SID   string `json:"sid"`
	Items []struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		Incarnation string `json:"incarnation"`
		Scenario    string `json:"scenario"`
		Module      string `json:"module"`
		Status      string `json:"status"`
		StartedAt   string `json:"started_at"`
		FinishedAt  string `json:"finished_at"`
		VoyageID    string `json:"voyage_id"`
	} `json:"items"`
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	Total  int `json:"total"`
}

func TestHistory_Happy_MergeShape(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fin := now.Add(time.Minute)
	voyageID := "vy-1"
	pool := &fakeHistoryPool{
		total: 2,
		items: []soul.HistoryItem{
			// errand with a back-link to a Voyage (ADR-043) — voyage_id present.
			{Type: soul.HistoryTypeErrand, ID: "e1", Module: "core.exec.run", Status: "success", StartedAt: now.Add(time.Hour), FinishedAt: &fin, VoyageID: &voyageID},
			// a direct scenario-run with no Voyage — voyage_id is omitted.
			{Type: soul.HistoryTypeScenario, ID: "a1", Incarnation: "web", Scenario: "deploy", Status: "running", StartedAt: now},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doGetHistory(t, h, "soul.example.com", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out historyReplyDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SID != "soul.example.com" || out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("envelope = %+v", out)
	}
	// errand row: module present, scenario fields empty (omitempty).
	if out.Items[0].Type != "errand" || out.Items[0].Module != "core.exec.run" {
		t.Errorf("item0 = %+v", out.Items[0])
	}
	if out.Items[0].Incarnation != "" || out.Items[0].Scenario != "" {
		t.Errorf("item0 scenario fields leaked: %+v", out.Items[0])
	}
	if out.Items[0].FinishedAt == "" {
		t.Errorf("item0 terminal - finished_at should be set: %+v", out.Items[0])
	}
	// item0 — the Voyage back-link (ADR-043) is forwarded.
	if out.Items[0].VoyageID != "vy-1" {
		t.Errorf("item0 voyage_id = %q, want vy-1", out.Items[0].VoyageID)
	}
	// scenario row: incarnation/scenario present, module/finished_at (running) empty.
	if out.Items[1].Type != "scenario" || out.Items[1].Incarnation != "web" ||
		out.Items[1].Scenario != "deploy" {
		t.Errorf("item1 = %+v", out.Items[1])
	}
	if out.Items[1].Module != "" || out.Items[1].FinishedAt != "" {
		t.Errorf("item1 errand fields/finished leaked: %+v", out.Items[1])
	}
	// item1 — a direct run with no Voyage: voyage_id omitted (omitempty).
	if out.Items[1].VoyageID != "" {
		t.Errorf("item1 voyage_id should be empty (outside Voyage): %q", out.Items[1].VoyageID)
	}
}

func TestHistory_InvalidSID_422(t *testing.T) {
	pool := &fakeHistoryPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetHistory(t, h, "InvalidUPPER", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHistory_InvalidType_422(t *testing.T) {
	pool := &fakeHistoryPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com", "type=push")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHistory_InvalidSince_422(t *testing.T) {
	pool := &fakeHistoryPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com", "since=not-a-time")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHistory_BadPagination_400(t *testing.T) {
	pool := &fakeHistoryPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com", "limit=abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHistory_FiltersForwarded(t *testing.T) {
	pool := &fakeHistoryPool{total: 0}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com",
		"type=scenario&type=errand&since=2026-05-01T00:00:00Z&offset=10&limit=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// since → $2; LIMIT/OFFSET — the last two args (5, 10).
	if !strings.Contains(pool.querySQL, "started_at > $2") {
		t.Errorf("since not forwarded to SQL: %s", pool.querySQL)
	}
	n := len(pool.queryArgs)
	if n < 4 {
		t.Fatalf("queryArgs = %v, want sid+since+limit+offset", pool.queryArgs)
	}
	if pool.queryArgs[n-2] != 5 || pool.queryArgs[n-1] != 10 {
		t.Errorf("limit/offset args = %v, want [...,5,10]", pool.queryArgs)
	}
	// Both sources (type=scenario+errand) → UNION ALL.
	if !strings.Contains(pool.querySQL, "UNION ALL") {
		t.Errorf("both types should produce UNION ALL: %s", pool.querySQL)
	}
}

func TestHistory_Empty(t *testing.T) {
	pool := &fakeHistoryPool{total: 0, items: nil}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out historyReplyDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 0 || len(out.Items) != 0 {
		t.Errorf("empty: total=%d items=%d", out.Total, len(out.Items))
	}
}

func TestHistory_DBError_500(t *testing.T) {
	pool := &fakeHistoryPool{total: 0, queryErr: errors.New("boom")}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHistory_InScope_CovenMatch_200 — guard (ADR-047 G1 Fix 3): a coven-scoped
// operator reads the timeline of a host in its OWN coven → 200 (the scope-gate lets
// it through).
func TestHistory_InScope_CovenMatch_200(t *testing.T) {
	pool := &fakeHistoryPool{total: 0, hostCovens: []string{"prod"}}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"prod"}}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history of own coven = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHistory_OutOfScope_404 — guard (Fix 3, closing a leak): a coven-scoped operator
// reads the timeline of ANOTHER's host (covens=[prod], scope=[staging]) → 404 (does
// not reveal existence and does not expose the timeline). Before Fix 3, History
// fetched SelectHistory by SID directly with no scope check — a leak of run history.
func TestHistory_OutOfScope_404(t *testing.T) {
	pool := &fakeHistoryPool{total: 5, hostCovens: []string{"prod"}}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"staging"}}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("history of foreign host = %d, want 404 (scope-gate blocks the leak); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHistory_OutOfHostGlobScope_404 — guard (list↔get consistency by host glob): a
// host-glob-scoped operator (host matches web-*) reads a non-matching host → 404.
func TestHistory_OutOfHostGlobScope_404(t *testing.T) {
	pool := &fakeHistoryPool{total: 5, hostCovens: []string{"prod"}}
	h := NewSoulHandler(pool, fakeScoper{exprs: []string{"host matches web-*"}}, nil, nil)
	rec := doGetHistory(t, h, "db-01.example.com", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("history of non-matching host = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHistory_HostMissing_404 — guard (Fix 3): a nonexistent host → 404 at the
// scope-gate (SelectBySID ErrNoRows), without reaching SelectHistory.
func TestHistory_HostMissing_404(t *testing.T) {
	pool := &fakeHistoryPool{soulMissing: true}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetHistory(t, h, "ghost.example.com", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("history of nonexistent host = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
