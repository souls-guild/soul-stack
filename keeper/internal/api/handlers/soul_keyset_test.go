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

// mustJSON декодирует тело ответа в dst, fail на ошибке.
func mustJSON(t *testing.T, body []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
}

// keysetReplyDTO — конверт ответа keyset-режима List (next_cursor/
// total_approximate). Items несёт transport — keyset-карточка обязана быть
// формо-идентична offset-карточке (presence-overlay требует status).
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

// evalRowFull — evalRow с явными status/transport (для filter+keyset-тестов:
// нужно различать connected/pending и agent/ssh внутри scope).
func evalRowFull(sid string, at time.Time, status soul.Status, transport soul.Transport, coven ...string) soul.ScopeEvalRow {
	return soul.ScopeEvalRow{
		SID:          sid,
		Transport:    transport,
		Status:       status,
		Coven:        coven,
		RegisteredAt: at,
	}
}

// TestSoulList_Keyset_FilterIntersectsScope_AND — BLOCKER-фикс (S3b-2a):
// user-filter (status/transport/coven) в keyset-режиме ПЕРЕСЕКАЕТСЯ со scope
// (AND), а не игнорируется. regex-scope `^web-`, два web-хоста с разными
// status/coven/transport — каждый фильтр обязан отдать строго своё подмножество
// scope. Регресс (фильтр молчаливо проигнорирован) = виден И не-запрошенный хост.
func TestSoulList_Keyset_FilterIntersectsScope_AND(t *testing.T) {
	at := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// web-01: connected, prod, agent. web-02: pending, dev, ssh. Оба в scope ^web-.
	all := []soul.ScopeEvalRow{
		evalRowFull("web-01.example.com", at, soul.StatusConnected, soul.TransportAgent, "prod"),
		evalRowFull("web-02.example.com", at.Add(-time.Second), soul.StatusPending, soul.TransportSSH, "dev"),
	}

	cases := []struct {
		name  string
		query string
		want  string // единственный ожидаемый SID
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

// TestSoulList_Keyset_FilterNarrowsBelowScope — фильтр режет НИЖЕ scope: оба
// хоста в scope, но фильтр status=disconnected не матчит ни одного → пустой
// результат (AND, не OR). Регресс = фильтр проигнорирован, отданы scoped-хосты.
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

// TestSoulList_Keyset_RegexFilters_ORUnion — ГЛАВНЫЙ union-инвариант на
// handler-уровне (S3b-2a): оператор с coven=prod + regex=^db- видит ОБЪЕДИНЕНИЕ:
// хост в prod но не db-* виден; хост db-* но не prod виден; хост ни-ни скрыт.
// Доказывает OR (не AND): хост, матчащий лишь ОДНО измерение, отдан.
func TestSoulList_Keyset_RegexFilters_ORUnion(t *testing.T) {
	at := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{
			evalRow("web-01.example.com", at, "prod"),                        // coven-match (не db-*).
			evalRow("db-07.example.com", at.Add(-time.Second), "staging"),    // regex-match (не prod).
			evalRow("db-09.example.com", at.Add(-2*time.Second), "prod"),     // оба.
			evalRow("app-01.example.com", at.Add(-3*time.Second), "staging"), // ни-ни → скрыт.
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

// TestSoulList_Keyset_RegexOnly_HidesNonMatching — regex-only оператор `^web-`
// видит только матчащие SID, не-матчащие скрыты.
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

// TestSoulList_Keyset_MultiPageNoDupesNoGaps — флот больше клиентского limit:
// обход курсором покрывает весь scoped-набор без дублей/пропусков на границах.
// page_size (limit) меньше флота → next_cursor проводит по всем страницам.
func TestSoulList_Keyset_MultiPageNoDupesNoGaps(t *testing.T) {
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// 5 web-* хостов с одинаковым registered_at (пачка) — composite-курсор.
	all := []soul.ScopeEvalRow{
		evalRow("web-01.example.com", base),
		evalRow("web-02.example.com", base),
		evalRow("web-03.example.com", base),
		evalRow("web-04.example.com", base),
		evalRow("web-05.example.com", base),
	}
	// Внутренние страницы eval-окна крупные (вся пачка за раз); добор режет
	// по клиентскому limit. Моделируем: первая внутр. страница = все 5, далее
	// пусто (исчерпано).
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

// TestSoulList_Keyset_UnderFill — узкий regex (1 матч на много хостов), limit=50:
// внутренняя страница содержит 1 матч + много не-матчей; добор вычитывает
// следующие внутр. страницы, пока не наберёт limit или не исчерпает БД. Ответ
// отдаёт собранное; next_cursor=nil когда БД исчерпана.
func TestSoulList_Keyset_UnderFill(t *testing.T) {
	at := time.Now().UTC()
	// 5 строк, малый внутр. page (3) → две полные внутр. страницы + остаток.
	// В каждой внутр. странице ровно 1 матч ^special-, остальные мимо — добор
	// обязан читать следующую внутр. страницу, пока БД не исчерпана.
	all := []soul.ScopeEvalRow{
		evalRow("noise-01.example.com", at),
		evalRow("special-01.example.com", at.Add(-time.Second)),
		evalRow("noise-02.example.com", at.Add(-2*time.Second)),
		evalRow("noise-03.example.com", at.Add(-3*time.Second)),
		evalRow("special-02.example.com", at.Add(-4*time.Second)),
	}
	pool := &fakeSoulPool{scopeEvalAll: all}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^special-"}}, nil, nil)
	h.scopeEvalInnerPageSize = 3 // вынуждает многостраничный добор.
	rec := doList(t, h, "limit=50")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	// limit=50 не набран (всего 2 матча), БД исчерпана → отдать оба + next_cursor=nil.
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2 (special-01, special-02 — добор через внутр. страницы)", len(out.Items))
	}
	if out.NextCursor != nil {
		t.Errorf("next_cursor = %v, want nil (БД исчерпана при under-fill)", *out.NextCursor)
	}
	// Минимум 2 внутренних страницы прочитаны (добор сработал).
	if pool.scopeEvalQueries < 2 {
		t.Errorf("scopeEvalQueries = %d, want >= 2 (добор должен читать след. внутр. страницы)", pool.scopeEvalQueries)
	}
}

// TestSoulList_Keyset_LimitExactFill_NextCursorPresent — матчей больше limit:
// ответ ровно limit элементов + next_cursor (есть ещё). Курсор = последняя
// ПРОСМОТРЕННАЯ строка; при наборе по limit скан останавливается на заполнении,
// поэтому она == последней ОТДАННОЙ (web-02), курсор не убегает на хвост.
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
	// Курсор кодирует последнюю ПРОСМОТРЕННУЮ строку; при наборе по limit скан
	// встал ровно на web-02 (не просматривая web-03) → курсор = web-02.
	cur, err := sharedapi.DecodeKeysetCursor(*out.NextCursor)
	if err != nil {
		t.Fatalf("декод курсора: %v", err)
	}
	if cur.SID != "web-02.example.com" {
		t.Errorf("курсор SID = %q, want web-02 (последняя просмотренная == последняя отданная при наборе по limit)", cur.SID)
	}
}

// TestSoulList_Keyset_BadRegex_FailClosed — битый паттерн в Purview → пустой
// список (fail-closed), НЕ 500. Регресс = 500 на eval-error или over-show.
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
	// fail-closed ДО БД: scope-eval не должен вызываться на неконпилируемом scope.
	if pool.scopeEvalQueries != 0 {
		t.Errorf("scopeEvalQueries = %d, want 0 (fail-closed до запроса)", pool.scopeEvalQueries)
	}
}

// TestSoulList_Keyset_OffsetAndCursorConflict_422 — клиент передал И offset>0 И
// cursor → 422 (клиентский баг не маскируется).
func TestSoulList_Keyset_OffsetAndCursorConflict_422(t *testing.T) {
	enc := sharedapi.EncodeKeysetCursor(sharedapi.KeysetCursor{RegisteredAt: time.Now().UTC(), SID: "h.example.com"})
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "offset=10&cursor="+enc)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (offset+cursor конфликт); body=%s", rec.Code, rec.Body.String())
	}
}

// TestSoulList_Keyset_BadCursor_400 — битый opaque-курсор → 400.
func TestSoulList_Keyset_BadCursor_400(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doList(t, h, "cursor=!!!not-base64!!!")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (битый курсор); body=%s", rec.Code, rec.Body.String())
	}
}

// TestSoulList_Keyset_CardHasStatusTransport — keyset-карточка несёт
// status/transport (полная проекция S3b-2a fix #2): без них presence-overlay
// пропустил бы карточку с пустым статусом и `GET /v1/souls` отдавал бы РАЗНУЮ
// форму карточки по Purview оператора. Регресс = обеднённая keyset-карточка.
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

// TestSoulList_Keyset_PresenceOverlayFlipsStatus — presence-overlay РАБОТАЕТ в
// keyset-режиме (S3b-2a fix #2): карточка несёт status (полная проекция), поэтому
// overlay деривирует presence из lease. PG-снимок connected, но lease мёртв →
// overlay флипает в disconnected. До фикса keyset-карточка несла пустой status,
// overlay её пропускал и presence НИКОГДА не навешивался в keyset-режиме.
func TestSoulList_Keyset_PresenceOverlayFlipsStatus(t *testing.T) {
	at := time.Now().UTC()
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{
			// PG-снимок connected, но lease НЕ живой (alive пуст) → overlay → disconnected.
			{SID: "web-01.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected, RegisteredAt: at},
		},
	}
	presence := &fakePresence{alive: aliveSet()} // ни одного живого lease.
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
	// overlay реально спросил presence про этот SID (карточка несла connected-снимок).
	if len(presence.gotSIDs) != 1 || presence.gotSIDs[0] != "web-01.example.com" {
		t.Errorf("presence.gotSIDs = %v, want [web-01] (overlay должен спросить lease в keyset-режиме)", presence.gotSIDs)
	}
}

// TestSoulList_Keyset_CapTruncation_NoLostMatches — BLOCKER-guard (S3b-2a fix #1):
// узкий regex, матчи в САМОМ КОНЦЕ keyset-порядка; малые inner-page + cap →
// первый запрос упирается в cap, ни одна строка не прошла фильтр (матчи за cap).
// Без фикса next_cursor НЕ выдавался бы (нечего отдать → клиент стоп → матчи
// потеряны навсегда). С фиксом: курсор = последняя ПРОСМОТРЕННАЯ строка (bound),
// обход курсором в итоге возвращает ВСЕ матчащие хосты без потерь.
func TestSoulList_Keyset_CapTruncation_NoLostMatches(t *testing.T) {
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// 10 хостов в keyset-порядке (registered_at DESC): noise идут ПЕРВЫМИ
	// (поздние), special — В КОНЦЕ (ранние). innerPageSize=2, cap=2 → один
	// запрос просматривает максимум 4 строки (2 страницы по 2) = только noise.
	var all []soul.ScopeEvalRow
	for i := 0; i < 8; i++ {
		all = append(all, evalRow(
			"noise-"+string(rune('a'+i))+".example.com",
			base.Add(-time.Duration(i)*time.Second)))
	}
	// 2 матча с самыми ранними registered_at (хвост keyset-порядка).
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
		h.scopeEvalMaxInnerPages = 2 // cap: ≤4 просмотренных строк за запрос.
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
		// Первый запрос: cap исчерпан на noise, матчей 0, но next_cursor ОБЯЗАН
		// присутствовать (иначе клиент остановится и потеряет special-*).
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

// TestSoulList_Keyset_EmptyResult_NoCursor — regex не матчит НИ ОДНОГО хоста:
// БД исчерпана, набрано 0 → next_cursor ОТСУТСТВУЕТ (nil, не пустая строка),
// клиент не зацикливается. Регресс = курсор на пустом исчерпанном наборе →
// бесконечный обход.
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

// TestSoulList_Keyset_LimitOne_FullTraversal — limit=1: на каждом шаге ровно 1
// элемент + next_cursor, обход курсором покрывает весь scoped-набор без дублей.
// Граница «минимальный limit».
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

// TestSoulList_Keyset_LimitOverFleet_Exhausted — limit больше всего флота:
// весь scoped-набор отдан за один запрос, next_cursor отсутствует (исчерпано).
// Граница «limit > N».
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

// TestSoulList_Keyset_CursorPastDeletedHost_NoCrash — хост удалён между
// страницами: клиент пришёл с курсором на (registered_at, sid) удалённого хоста.
// keyset-предикат «строго ПОСЛЕ» отдаёт оставшиеся без падения/дублей
// (best-effort, не 500). Регресс = 500 или дубль/пропуск.
func TestSoulList_Keyset_CursorPastDeletedHost_NoCrash(t *testing.T) {
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	deletedAt := base.Add(-time.Second)
	// Флот теперь без web-02 (удалён); курсор клиента указывает на web-02.
	all := []soul.ScopeEvalRow{
		evalRow("web-01.example.com", base),
		evalRow("web-03.example.com", base.Add(-2*time.Second)),
	}
	pool := &fakeSoulPool{scopeEvalAll: all}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	// Курсор на удалённый web-02 (его в наборе нет).
	enc := sharedapi.EncodeKeysetCursor(sharedapi.KeysetCursor{RegisteredAt: deletedAt, SID: "web-02.example.com"})
	rec := doList(t, h, "cursor="+enc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort, не 500); body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	// keyset «строго ПОСЛЕ (deletedAt, web-02)» → только web-03 (web-01 раньше курсора).
	if len(out.Items) != 1 || out.Items[0].SID != "web-03.example.com" {
		t.Fatalf("items = %+v, want [web-03] (продолжение после удалённого без дублей)", out.Items)
	}
}

// TestSoulList_Keyset_PresenceOverlayAfterScope — presence навешан ТОЛЬКО на
// прошедшие scope; сбой Redis → scoped-набор сохраняется (presence fail-SAFE
// ПОВЕРХ fail-CLOSED scope). Два слоя не путаются.
func TestSoulList_Keyset_PresenceOverlayAfterScope(t *testing.T) {
	at := time.Now().UTC()
	pool := &fakeSoulPool{
		scopeEvalAll: []soul.ScopeEvalRow{
			evalRow("web-01.example.com", at),                  // в regex-scope.
			evalRow("db-01.example.com", at.Add(-time.Second)), // вне regex → скрыт scope.
		},
	}
	// presence падает (Redis down) → fail-safe: scoped-набор отдаётся как есть.
	presence := &fakePresence{err: errors.New("redis down")}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, presence, nil)
	rec := doList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out keysetReplyDTO
	mustJSON(t, rec.Body.Bytes(), &out)
	// scope сохранён несмотря на сбой presence: только web-01, db-01 скрыт.
	if len(out.Items) != 1 || out.Items[0].SID != "web-01.example.com" {
		t.Fatalf("items = %+v, want [web-01] (scope fail-closed устоял при сбое presence)", out.Items)
	}
}
