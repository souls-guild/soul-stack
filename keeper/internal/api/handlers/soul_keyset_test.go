package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	sharedapi "github.com/souls-guild/soul-stack/shared/api"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// mustJSON decodes the response body into dst, fails on error.
func mustJSON(t *testing.T, body []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
}

// keysetReplyDTO — the response envelope for keyset-mode List (next_cursor/
// total_approximate). Items carries transport — the keyset card must be
// shape-identical to the offset card (presence-overlay requires status).
type keysetReplyDTO struct {
	Items []struct {
		SID       string `json:"sid"`
		Status    string `json:"status"`
		Transport string `json:"transport"`
	} `json:"items"`
	Offset           int     `json:"offset"`
	Limit            int     `json:"limit"`
	Total            int     `json:"total"`
	NextCursor       *string `json:"next_cursor"`
	TotalApproximate bool    `json:"total_approximate"`
}

func evalRow(sid string, at time.Time, coven ...string) soul.ScopeEvalRow {
	return soul.ScopeEvalRow{
		SID:          sid,
		Transport:    soul.TransportAgent,
		Status:       soul.StatusConnected,
		Coven:        coven,
		RegisteredAt: at,
	}
}

// evalRowFull — evalRow with explicit status/transport (for filter+keyset tests:
// need to distinguish connected/pending and agent/ssh within scope).
func evalRowFull(sid string, at time.Time, status soul.Status, transport soul.Transport, coven ...string) soul.ScopeEvalRow {
	return soul.ScopeEvalRow{
		SID:          sid,
		Transport:    transport,
		Status:       status,
		Coven:        coven,
		RegisteredAt: at,
	}
}

// TestSoulList_Keyset_FilterIntersectsScope_AND — BLOCKER fix (S3b-2a):
// the user filter (status/transport/coven) in keyset mode INTERSECTS with scope
// (AND), not ignored. regex-scope `^web-`, two web hosts with different
// status/coven/transport — each filter must return strictly its own subset of
// scope. Regression (filter silently ignored) = a non-requested host is visible too.
func TestSoulList_Keyset_FilterIntersectsScope_AND(t *testing.T) {
	at := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// web-01: connected, prod, agent. web-02: pending, dev, ssh. Both in scope ^web-.
	all := []soul.ScopeEvalRow{
		evalRowFull("web-01.example.com", at, soul.StatusConnected, soul.TransportAgent, "prod"),
		evalRowFull("web-02.example.com", at.Add(-time.Second), soul.StatusPending, soul.TransportSSH, "dev"),
	}

	cases := []struct {
		name  string
		query string
		want  string // the single expected SID
	}{
		{"status=connected", "status=connected", "web-01.example.com"},
		{"coven=prod", "coven=prod", "web-01.example.com"},
		{"coven=dev", "coven=dev", "web-02.example.com"},
		{"transport=ssh", "transport=ssh", "web-02.example.com"},
		{"transport=agent", "transport=agent", "web-01.example.com"},
		{"status=pending", "status=pending", "web-02.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := &fakeSoulPool{scopeEvalAll: all}
			h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
			rec := doList(t, h, tc.query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			var out keysetReplyDTO
			mustJSON(t, rec.Body.Bytes(), &out)
			if len(out.Items) != 1 {
				t.Fatalf("items = %d, want 1 (фильтр ∩ scope = ровно один хост); got %+v", len(out.Items), out.Items)
			}
			if out.Items[0].SID != tc.want {
				t.Errorf("got SID %q, want %q (фильтр должен сужать ВНУТРИ scope, не игнорироваться)", out.Items[0].SID, tc.want)
			}
		})
	}
}

// TestSoulList_Keyset_FilterNarrowsBelowScope — the filter narrows BELOW scope: both
// hosts are in scope, but the filter status=disconnected matches none → an empty
// result (AND, not OR). Regression = the filter is ignored, scoped hosts are returned.
func TestSoulList_Keyset_FilterNarrowsBelowScope(t *testing.T) {
	at := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	all := []soul.ScopeEvalRow{
		evalRowFull("web-01.example.com", at, soul.StatusConnected, soul.TransportAgent, "prod"),
		evalRowFull("web-02.example.com", at.Add(-time.Second), soul.StatusPending, soul.TransportAgent, "dev"),
	}
	pool := &fakeSoulPool{scopeEvalAll: all}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "status=disconnected")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	if len(out.Items) != 0 {
		t.Fatalf("items = %d, want 0 (фильтр ∩ scope пуст: ни один scoped-хост не disconnected)", len(out.Items))
	}
}

// TestSoulList_Keyset_RegexFilters_ORUnion — the MAIN union invariant at the
// handler level (S3b-2a): an operator with coven=prod + regex=^db- sees the UNION:
// a host in prod but not db-* is visible; a host db-* but not prod is visible; a host matching neither is hidden.
// Proves OR (not AND): a host matching just ONE dimension is still returned.
func TestSoulList_Keyset_RegexFilters_ORUnion(t *testing.T) {
	at := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{
			evalRow("web-01.example.com", at, "prod"),                        // coven-match (not db-*).
			evalRow("db-07.example.com", at.Add(-time.Second), "staging"),    // regex-match (not prod).
			evalRow("db-09.example.com", at.Add(-2*time.Second), "prod"),     // both.
			evalRow("app-01.example.com", at.Add(-3*time.Second), "staging"), // neither → hidden.
		},
	}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"prod"}, regexes: []string{"^db-"}}, nil, nil)

	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)

	got := map[string]bool{}
	for _, it := range out.Items {
		got[it.SID] = true
	}
	if !got["web-01.example.com"] {
		t.Error("web-01 (prod, не db-*) скрыт — union должен показать по coven")
	}
	if !got["db-07.example.com"] {
		t.Error("db-07 (db-*, не prod) скрыт — union должен показать по regex")
	}
	if !got["db-09.example.com"] {
		t.Error("db-09 (оба) скрыт")
	}
	if got["app-01.example.com"] {
		t.Error("app-01 (ни coven, ни regex) ВИДЕН — over-show за границу Purview (union ⊆ Purview нарушен)")
	}
	if !out.TotalApproximate {
		t.Error("keyset-режим: total_approximate=false; want true (total не точен)")
	}
}

// TestSoulList_Keyset_RegexOnly_HidesNonMatching — a regex-only operator `^web-`
// sees only matching SIDs, non-matching ones are hidden.
func TestSoulList_Keyset_RegexOnly_HidesNonMatching(t *testing.T) {
	at := time.Now().UTC()
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{
			evalRow("web-01.example.com", at),
			evalRow("db-01.example.com", at.Add(-time.Second)),
			evalRow("web-02.example.com", at.Add(-2*time.Second)),
		},
	}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2 (web-01, web-02)", len(out.Items))
	}
	for _, it := range out.Items {
		if it.SID == "db-01.example.com" {
			t.Error("db-01 виден — не матчит ^web-, должен быть скрыт")
		}
	}
}

// TestSoulList_Keyset_MultiPageNoDupesNoGaps — the fleet is larger than the client limit:
// a cursor traversal covers the whole scoped set without duplicates/gaps at the boundaries.
// page_size (limit) smaller than the fleet → next_cursor walks through all pages.
func TestSoulList_Keyset_MultiPageNoDupesNoGaps(t *testing.T) {
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// 5 web-* hosts with the same registered_at (a batch) — composite cursor.
	all := []soul.ScopeEvalRow{
		evalRow("web-01.example.com", base),
		evalRow("web-02.example.com", base),
		evalRow("web-03.example.com", base),
		evalRow("web-04.example.com", base),
		evalRow("web-05.example.com", base),
	}
	// The eval-window's inner pages are large (the whole batch at once); the top-up cuts
	// by the client limit. We model it: the first inner page = all 5, then
	// empty (exhausted).
	want := map[string]bool{}
	for _, r := range all {
		want[r.SID] = true
	}

	got := map[string]bool{}
	var cursorParam string
	pages := 0
	for {
		pool := &fakeSoulPool{scopeEvalAll: all}
		h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
		q := "limit=2"
		if cursorParam != "" {
			q += "&cursor=" + cursorParam
		}
		rec := doList(t, h, q)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d, body=%s", pages, rec.Code, rec.Body.String())
		}
		var out keysetReplyDTO
		mustJSON(t, rec.Body.Bytes(), &out)
		for _, it := range out.Items {
			if got[it.SID] {
				t.Fatalf("ДУБЛЬ %s на границе keyset-страницы", it.SID)
			}
			got[it.SID] = true
		}
		if out.NextCursor == nil {
			break
		}
		cursorParam = *out.NextCursor
		pages++
		if pages > 20 {
			t.Fatal("курсор не сходится")
		}
	}
	if len(got) != len(want) {
		t.Fatalf("собрано %d, want %d (пропуски/дубли в keyset-обходе)", len(got), len(want))
	}
	for sid := range want {
		if !got[sid] {
			t.Errorf("%s пропущен", sid)
		}
	}
}

// TestSoulList_Keyset_UnderFill — a narrow regex (1 match among many hosts), limit=50:
// the inner page contains 1 match + many non-matches; the top-up reads
// subsequent inner pages until it fills the limit or exhausts the DB. The response
// returns what was collected; next_cursor=nil when the DB is exhausted.
func TestSoulList_Keyset_UnderFill(t *testing.T) {
	at := time.Now().UTC()
	// 5 rows, a small inner page (3) → two full inner pages + a remainder.
	// Each inner page has exactly 1 match ^special-, the rest miss — the top-up
	// must read the next inner page until the DB is exhausted.
	all := []soul.ScopeEvalRow{
		evalRow("noise-01.example.com", at),
		evalRow("special-01.example.com", at.Add(-time.Second)),
		evalRow("noise-02.example.com", at.Add(-2*time.Second)),
		evalRow("noise-03.example.com", at.Add(-3*time.Second)),
		evalRow("special-02.example.com", at.Add(-4*time.Second)),
	}
	pool := &fakeSoulPool{scopeEvalAll: all}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^special-"}}, nil, nil)
	h.scopeEvalInnerPageSize = 3 // forces a multi-page top-up.
	rec := doList(t, h, "limit=50")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	// limit=50 not filled (only 2 matches total), DB exhausted → return both + next_cursor=nil.
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2 (special-01, special-02 — добор через внутр. страницы)", len(out.Items))
	}
	if out.NextCursor != nil {
		t.Errorf("next_cursor = %v, want nil (БД исчерпана при under-fill)", *out.NextCursor)
	}
	// At least 2 inner pages were read (the top-up worked).
	if pool.scopeEvalQueries < 2 {
		t.Errorf("scopeEvalQueries = %d, want >= 2 (добор должен читать след. внутр. страницы)", pool.scopeEvalQueries)
	}
}

// TestSoulList_Keyset_LimitExactFill_NextCursorPresent — more matches than the limit:
// the response has exactly limit elements + next_cursor (there's more). The cursor is the last
// SCANNED row; when filling by limit, the scan stops as soon as it's full,
// so it equals the last RETURNED row (web-02) — the cursor doesn't run ahead to the tail.
func TestSoulList_Keyset_LimitExactFill_NextCursorPresent(t *testing.T) {
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	all := []soul.ScopeEvalRow{
		evalRow("web-01.example.com", base),
		evalRow("web-02.example.com", base.Add(-time.Second)),
		evalRow("web-03.example.com", base.Add(-2*time.Second)),
	}
	pool := &fakeSoulPool{scopeEvalAll: all}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2 (limit)", len(out.Items))
	}
	if out.NextCursor == nil {
		t.Fatal("next_cursor=nil при наличии 3-го матча; want присутствует")
	}
	// The cursor encodes the last SCANNED row; when filling by limit, the scan
	// stopped exactly at web-02 (without scanning web-03) → cursor = web-02.
	cur, err := sharedapi.DecodeKeysetCursor(*out.NextCursor)
	if err != nil {
		t.Fatalf("декод курсора: %v", err)
	}
	if cur.SID != "web-02.example.com" {
		t.Errorf("курсор SID = %q, want web-02 (последняя просмотренная == последняя отданная при наборе по limit)", cur.SID)
	}
}

// TestSoulList_Keyset_BadRegex_FailClosed — a broken pattern in Purview → an empty
// list (fail-closed), NOT 500. Regression = 500 on eval-error or over-show.
func TestSoulList_Keyset_BadRegex_FailClosed(t *testing.T) {
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{evalRow("web-01.example.com", time.Now().UTC())},
	}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"([unclosed"}}, nil, nil)
	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (битый regex → пусто, НЕ 500); body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	if len(out.Items) != 0 {
		t.Fatalf("items = %d, want 0 (fail-closed на битом regex)", len(out.Items))
	}
	// fail-closed BEFORE the DB: scope-eval must not be invoked on a non-compilable scope.
	if pool.scopeEvalQueries != 0 {
		t.Errorf("scopeEvalQueries = %d, want 0 (fail-closed до запроса)", pool.scopeEvalQueries)
	}
}

// TestSoulList_Keyset_OffsetAndCursorConflict_422 — the client passed BOTH offset>0 AND
// cursor → 422 (the client bug is not masked).
func TestSoulList_Keyset_OffsetAndCursorConflict_422(t *testing.T) {
	enc := sharedapi.EncodeKeysetCursor(sharedapi.KeysetCursor{RegisteredAt: time.Now().UTC(), SID: "h.example.com"})
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "offset=10&cursor="+enc)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (offset+cursor конфликт); body=%s", rec.Code, rec.Body.String())
	}
}

// TestSoulList_Keyset_BadCursor_400 — a broken opaque cursor → 400.
func TestSoulList_Keyset_BadCursor_400(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "cursor=!!!not-base64!!!")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (битый курсор); body=%s", rec.Code, rec.Body.String())
	}
}

// TestSoulList_Keyset_CardHasStatusTransport — the keyset card carries
// status/transport (full projection, S3b-2a fix #2): without them, the presence-overlay
// would skip a card with an empty status, and `GET /v1/souls` would return a DIFFERENT
// card shape depending on the operator's Purview. Regression = an impoverished keyset card.
func TestSoulList_Keyset_CardHasStatusTransport(t *testing.T) {
	at := time.Now().UTC()
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{
			{SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: at},
		},
	}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(out.Items))
	}
	if out.Items[0].Transport != "agent" {
		t.Errorf("keyset-карточка transport = %q, want agent (полная проекция)", out.Items[0].Transport)
	}
	if out.Items[0].Status != "connected" {
		t.Errorf("keyset-карточка status = %q, want connected (полная проекция)", out.Items[0].Status)
	}
}

// TestSoulList_Keyset_PresenceOverlayFlipsStatus — the presence-overlay WORKS in
// keyset mode (S3b-2a fix #2): the card carries status (full projection), so the
// overlay derives presence from the lease. PG snapshot connected, but the lease is dead →
// the overlay flips it to disconnected. Before the fix, the keyset card carried an empty status,
// the overlay skipped it, and presence was NEVER applied in keyset mode.
func TestSoulList_Keyset_PresenceOverlayFlipsStatus(t *testing.T) {
	at := time.Now().UTC()
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{
			// PG snapshot connected, but the lease is NOT alive (alive is empty) → overlay → disconnected.
			{SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: at},
		},
	}
	presence := &fakePresence{alive: aliveSet()} // not a single live lease.
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, presence, nil)
	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(out.Items))
	}
	if out.Items[0].Status != "disconnected" {
		t.Errorf("keyset status = %q, want disconnected (presence-overlay флипнул мёртвый lease)", out.Items[0].Status)
	}
	// the overlay actually asked presence about this SID (the card carried a connected snapshot).
	if len(presence.gotSIDs) != 1 || presence.gotSIDs[0] != "web-01.example.com" {
		t.Errorf("presence.gotSIDs = %v, want [web-01] (overlay должен спросить lease в keyset-режиме)", presence.gotSIDs)
	}
}

// TestSoulList_Keyset_CapTruncation_NoLostMatches — BLOCKER-guard (S3b-2a fix #1):
// a narrow regex, matches at the VERY END of the keyset order; a small inner-page + cap →
// the first request hits the cap, not a single row passed the filter (matches are past the cap).
// Without the fix, next_cursor would NOT be issued (nothing to return → the client stops → matches
// lost forever). With the fix: the cursor = the last SCANNED row (bound),
// and cursor traversal eventually returns ALL matching hosts without loss.
func TestSoulList_Keyset_CapTruncation_NoLostMatches(t *testing.T) {
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// 10 hosts in keyset order (registered_at DESC): noise comes FIRST
	// (later ones), special is at the END (earlier ones). innerPageSize=2, cap=2 → one
	// request scans at most 4 rows (2 pages of 2) = noise only.
	var all []soul.ScopeEvalRow
	for i := 0; i < 8; i++ {
		all = append(all, evalRow(
			"noise-"+string(rune('a'+i))+".example.com",
			base.Add(-time.Duration(i)*time.Second)))
	}
	// 2 matches with the earliest registered_at (the tail of the keyset order).
	all = append(all,
		evalRow("special-01.example.com", base.Add(-100*time.Second)),
		evalRow("special-02.example.com", base.Add(-101*time.Second)))

	want := map[string]bool{"special-01.example.com": true, "special-02.example.com": true}

	got := map[string]bool{}
	var cursorParam string
	pages := 0
	sawEmptyButCursor := false
	for {
		pool := &fakeSoulPool{scopeEvalAll: all}
		h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^special-"}}, nil, nil)
		h.scopeEvalInnerPageSize = 2
		h.scopeEvalMaxInnerPages = 2 // cap: ≤4 scanned rows per request.
		q := "limit=50"
		if cursorParam != "" {
			q += "&cursor=" + cursorParam
		}
		rec := doList(t, h, q)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d, body=%s", pages, rec.Code, rec.Body.String())
		}
		var out keysetReplyDTO
		mustJSON(t, rec.Body.Bytes(), &out)
		for _, it := range out.Items {
			if got[it.SID] {
				t.Fatalf("ДУБЛЬ %s при cap-обходе", it.SID)
			}
			got[it.SID] = true
		}
		// First request: the cap is exhausted on noise, 0 matches, but next_cursor MUST
		// be present (otherwise the client would stop and lose special-*).
		if len(out.Items) == 0 && out.NextCursor != nil {
			sawEmptyButCursor = true
		}
		if out.NextCursor == nil {
			break
		}
		cursorParam = *out.NextCursor
		pages++
		if pages > 50 {
			t.Fatal("cap-обход не сходится (>50 страниц)")
		}
	}
	if !sawEmptyButCursor {
		t.Error("ни одной пусто-но-с-курсором страницы — cap-кейс не смоделирован (тест не проверяет фикс)")
	}
	if len(got) != len(want) {
		t.Fatalf("собрано %d матчей, want %d — cap-truncation ПОТЕРЯЛ матчи за первыми страницами", len(got), len(want))
	}
	for sid := range want {
		if !got[sid] {
			t.Errorf("%s потерян при cap-обходе (BLOCKER регресс)", sid)
		}
	}
}

// TestSoulList_Keyset_EmptyResult_NoCursor — the regex matches NOT A SINGLE host:
// the DB is exhausted, 0 collected → next_cursor is ABSENT (nil, not an empty string),
// the client doesn't loop forever. Regression = a cursor on an empty exhausted set →
// infinite traversal.
func TestSoulList_Keyset_EmptyResult_NoCursor(t *testing.T) {
	at := time.Now().UTC()
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{
			evalRow("db-01.example.com", at),
			evalRow("db-02.example.com", at.Add(-time.Second)),
		},
	}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	if len(out.Items) != 0 {
		t.Fatalf("items = %d, want 0 (^web- не матчит db-*)", len(out.Items))
	}
	if out.NextCursor != nil {
		t.Errorf("next_cursor = %q, want nil (БД исчерпана, набрано 0 — не зацикливаться)", *out.NextCursor)
	}
}

// TestSoulList_Keyset_LimitOne_FullTraversal — limit=1: at every step exactly 1
// element + next_cursor, cursor traversal covers the whole scoped set without duplicates.
// Boundary case "minimal limit".
func TestSoulList_Keyset_LimitOne_FullTraversal(t *testing.T) {
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	all := []soul.ScopeEvalRow{
		evalRow("web-01.example.com", base),
		evalRow("web-02.example.com", base.Add(-time.Second)),
		evalRow("web-03.example.com", base.Add(-2*time.Second)),
	}
	got := map[string]bool{}
	var cursorParam string
	steps := 0
	for {
		pool := &fakeSoulPool{scopeEvalAll: all}
		h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
		q := "limit=1"
		if cursorParam != "" {
			q += "&cursor=" + cursorParam
		}
		rec := doList(t, h, q)
		if rec.Code != http.StatusOK {
			t.Fatalf("step %d status = %d, body=%s", steps, rec.Code, rec.Body.String())
		}
		var out keysetReplyDTO
		mustJSON(t, rec.Body.Bytes(), &out)
		if len(out.Items) > 1 {
			t.Fatalf("step %d: items = %d, want ≤1 (limit=1)", steps, len(out.Items))
		}
		for _, it := range out.Items {
			if got[it.SID] {
				t.Fatalf("ДУБЛЬ %s при limit=1 обходе", it.SID)
			}
			got[it.SID] = true
		}
		if out.NextCursor == nil {
			break
		}
		cursorParam = *out.NextCursor
		steps++
		if steps > 20 {
			t.Fatal("limit=1 обход не сходится")
		}
	}
	if len(got) != 3 {
		t.Fatalf("собрано %d, want 3 (limit=1 пропустил/дублировал)", len(got))
	}
}

// TestSoulList_Keyset_LimitOverFleet_Exhausted — limit larger than the whole fleet:
// the whole scoped set is returned in one request, next_cursor is absent (exhausted).
// Boundary case "limit > N".
func TestSoulList_Keyset_LimitOverFleet_Exhausted(t *testing.T) {
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	all := []soul.ScopeEvalRow{
		evalRow("web-01.example.com", base),
		evalRow("web-02.example.com", base.Add(-time.Second)),
	}
	pool := &fakeSoulPool{scopeEvalAll: all}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "limit=100")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2 (limit > флота → весь scoped-набор)", len(out.Items))
	}
	if out.NextCursor != nil {
		t.Errorf("next_cursor = %q, want nil (limit>флота, исчерпано)", *out.NextCursor)
	}
}

// TestSoulList_Keyset_CursorPastDeletedHost_NoCrash — a host was deleted between
// pages: the client comes with a cursor pointing at the (registered_at, sid) of the deleted host.
// The keyset predicate "strictly AFTER" returns the rest without crashing or duplicates
// (best-effort, not 500). Regression = 500 or a duplicate/gap.
func TestSoulList_Keyset_CursorPastDeletedHost_NoCrash(t *testing.T) {
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	deletedAt := base.Add(-time.Second)
	// The fleet is now without web-02 (deleted); the client's cursor points at web-02.
	all := []soul.ScopeEvalRow{
		evalRow("web-01.example.com", base),
		evalRow("web-03.example.com", base.Add(-2*time.Second)),
	}
	pool := &fakeSoulPool{scopeEvalAll: all}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	// A cursor pointing at the deleted web-02 (it's not in the set).
	enc := sharedapi.EncodeKeysetCursor(sharedapi.KeysetCursor{RegisteredAt: deletedAt, SID: "web-02.example.com"})
	rec := doList(t, h, "cursor="+enc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort, не 500); body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	// keyset "strictly AFTER (deletedAt, web-02)" → only web-03 (web-01 is before the cursor).
	if len(out.Items) != 1 || out.Items[0].SID != "web-03.example.com" {
		t.Fatalf("items = %+v, want [web-03] (продолжение после удалённого без дублей)", out.Items)
	}
}

// TestSoulList_Keyset_PresenceOverlayAfterScope — presence is applied ONLY to
// hosts that passed scope; a Redis failure → the scoped set is preserved (presence fail-SAFE
// ON TOP OF fail-CLOSED scope). The two layers don't get confused.
func TestSoulList_Keyset_PresenceOverlayAfterScope(t *testing.T) {
	at := time.Now().UTC()
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{
			evalRow("web-01.example.com", at),                  // in regex-scope.
			evalRow("db-01.example.com", at.Add(-time.Second)), // outside regex → hidden by scope.
		},
	}
	// presence fails (Redis down) → fail-safe: the scoped set is returned as-is.
	presence := &fakePresence{err: errors.New("redis down")}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, presence, nil)
	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	// scope is preserved despite the presence failure: only web-01, db-01 is hidden.
	if len(out.Items) != 1 || out.Items[0].SID != "web-01.example.com" {
		t.Fatalf("items = %+v, want [web-01] (scope fail-closed устоял при сбое presence)", out.Items)
	}
}
