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

// fakeReadPool — узкий мок [SoulPool] для GET /v1/souls/{sid} и
// GET /v1/souls/{sid}/soulprint. От расширенного [fakeSoulPool] отделён, чтобы
// не плодить SQL-матчеры в общем fake — read-handler-ы лежат на двух запросах
// (SelectBySID и SelectSoulprint).
type fakeReadPool struct {
	soul         *soul.Soul
	soulFactsRaw []byte
	collectedAt  *time.Time
	receivedAt   *time.Time

	// Если задано, SelectSoulprint вернёт row, где `soulprint_facts IS NULL`
	// (т.е. запись Soul-а есть, фактов нет) — ловит ветку 410.
	soulprintEmpty bool

	// soulMissing=true → SelectBySID и SelectSoulprint отвечают pgx.ErrNoRows.
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
		// staticRow.Scan(*[]byte) делает r.values[i].([]byte) без nil-проверки:
		// для NULL-колонки эмулируем «пустые байты» (len==0), что эквивалентно
		// pgx-поведению для NULL bytea/jsonb (см. SelectSoulprint → проверка
		// len(factsJSON) == 0). Timestamp-указатели — nil допустимо (есть **time.Time
		// case).
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
		return staticRow{values: []any{
			s.SID, string(s.Transport), string(s.Status), s.Coven,
			s.RegisteredAt, lastSeenAt, lastSeenByKID, createdByAID, requestedAt, note,
		}}
	}
	return errRow{err: errors.New("fakeReadPool: unexpected SQL: " + sql)}
}

// recordGet вызывает GetTyped напрямую (handler-native T5d) и сериализует результат в
// recorder через те же writeJSON/writeProblemError, что прежний (w,r)-роут — сохраняя
// status/body-инварианты downstream-ассертов. claims=nil → fail-closed single-read.
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

// doGetSoul собирает GET /v1/souls/{sid} (без claims — fail-closed).
func doGetSoul(t *testing.T, h *SoulHandler, sid string) *httptest.ResponseRecorder {
	t.Helper()
	return recordGet(t, h, sid, "")
}

// doGetSoulScoped — doGetSoul с инжектом claims (scope-резолв single-read).
func doGetSoulScoped(t *testing.T, h *SoulHandler, sid, aid string) *httptest.ResponseRecorder {
	t.Helper()
	return recordGet(t, h, sid, aid)
}

// recordGetSoulprint вызывает GetSoulprintTyped напрямую и сериализует результат в recorder.
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

// doGetSoulprint собирает GET /v1/souls/{sid}/soulprint (без claims).
func doGetSoulprint(t *testing.T, h *SoulHandler, sid string) *httptest.ResponseRecorder {
	t.Helper()
	return recordGetSoulprint(t, h, sid, "")
}

// doGetSoulprintScoped — doGetSoulprint с инжектом claims (scope-резолв).
func doGetSoulprintScoped(t *testing.T, h *SoulHandler, sid, aid string) *httptest.ResponseRecorder {
	t.Helper()
	return recordGetSoulprint(t, h, sid, aid)
}

// soulListViewJSON проецирует доменный SoulListView в map с теми же json-ключами, что
// прежний wire (для downstream-ассертов теста по полям ответа GET /v1/souls/{sid}).
// Воспроизводит форму native SoulListEntry (covens non-null, nullable как null).
func soulListViewJSON(v SoulListView) map[string]any {
	m := map[string]any{
		"sid":              v.SID,
		"transport":        v.Transport,
		"status":           v.Status,
		"covens":           v.Covens,
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

// --- single-read scope-гейт (ADR-047 S3b-1): out-of-scope → 404 ---

// scopedReadPool — узкий read-pool с хостом в covens=[prod] для scope-тестов.
func scopedReadPool() *fakeReadPool {
	return &fakeReadPool{soul: &soul.Soul{
		SID:          "prod-01.example.com",
		Transport:    soul.TransportAgent,
		Status:       soul.StatusConnected,
		Coven:        []string{"prod"},
		RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}}
}

// TestGetSoul_InScope_CovenMatch — scoped-оператор (covens=[prod]) читает хост
// своего coven → 200.
func TestGetSoul_InScope_CovenMatch(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{covens: []string{"prod"}}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_OutOfScope_404 — ГЛАВНЫЙ security-инвариант S3b-1: scoped-оператор
// (covens=[staging]) читает ЧУЖОЙ хост (covens=[prod]) → 404 (НЕ 403, НЕ 200):
// не палим существование чужого хоста. Регресс = scoped-оператор читает чужой
// хост по прямому SID.
func TestGetSoul_OutOfScope_404(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{covens: []string{"staging"}}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-eve")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (out-of-scope не палит существование); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_InScope_RegexMatch_200 — regex-scoped оператор (regex=^prod-)
// читает матчащий хост (prod-01) → 200. Закрывает list↔get рассинхрон gate-fix-а:
// regex-видимый в List хост доступен и по прямому GET /{sid} (раньше InScope был
// coven-only → 404 на видимый). Регресс = рассинхрон вернулся.
func TestGetSoul_InScope_RegexMatch_200(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{regexes: []string{"^prod-"}}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-webops")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (regex ^prod- матчит prod-01); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_OutOfRegexScope_404 — regex-scoped оператор (regex=^web-) читает
// НЕ-матчащий хост (prod-01) → 404 (не палит существование вне regex-границы).
func TestGetSoul_OutOfRegexScope_404(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-webops")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (regex ^web- НЕ матчит prod-01); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoulprint_InScope_RegexMatch_200 — soulprint того же regex-предиката:
// матчащий хост → 200, факты раскрыты.
func TestGetSoulprint_InScope_RegexMatch_200(t *testing.T) {
	pool := scopedReadPool()
	pool.soulFactsRaw = []byte(`{"sid":"prod-01.example.com","os":{"family":"debian"}}`)
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^prod-"}}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "prod-01.example.com", "archon-webops")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (regex match); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_Unrestricted_200 — Unrestricted-оператор читает любой хост → 200.
func TestGetSoul_Unrestricted_200(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-root")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_EmptyPurview_404 — Purview{} (нет прав, fail-closed) → 404 (не 200,
// не 403): оператору не положено ни одного хоста.
func TestGetSoul_EmptyPurview_404(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{empty: true}, nil, nil)
	rec := doGetSoulScoped(t, h, "prod-01.example.com", "archon-nobody")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (fail-closed); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoul_NoClaims_404 — нет claims (защитный инвариант, штатно route под
// RequireJWT) → 404 (fail-closed, симметрично List no-claims).
func TestGetSoul_NoClaims_404(t *testing.T) {
	h := NewSoulHandler(scopedReadPool(), fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetSoul(t, h, "prod-01.example.com") // БЕЗ claims.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no claims → fail-closed); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoulprint_OutOfScope_404 — scope-гейт того же измерения на soulprint-read:
// чужой хост → 404 ДО раскрытия фактов.
func TestGetSoulprint_OutOfScope_404(t *testing.T) {
	pool := scopedReadPool()
	pool.soulFactsRaw = []byte(`{"sid":"prod-01.example.com","os":{"family":"debian"}}`)
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"staging"}}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "prod-01.example.com", "archon-eve")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (soulprint out-of-scope); body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoulprint_InScope_200 — soulprint своего coven → 200.
func TestGetSoulprint_InScope_200(t *testing.T) {
	pool := scopedReadPool()
	pool.soulFactsRaw = []byte(`{"sid":"prod-01.example.com","os":{"family":"debian"}}`)
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"prod"}}, nil, nil)
	rec := doGetSoulprintScoped(t, h, "prod-01.example.com", "archon-alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSoulprint_EmptyPurview_404 — нет прав → 404 (fail-closed).
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

// TestGetSoulprint_BytePassthrough_Exact — guard byte-passthrough (finding 2,
// категория D ADR-051): сырые JSONB-байты `souls.soulprint_facts` доезжают на
// wire в typed_facts БЕЗ unmarshal→map→re-marshal. Доказательство — байт-в-байт:
// raw несёт неалфавитный порядок ключей (`sid` перед `os`, внутри os `pkg_mgr`
// перед `init_system`); прежний map-путь пересортировал бы их лексикографически,
// passthrough — отдаёт ровно как лежит. Ассертим точную под-строку сырья в теле.
func TestGetSoulprint_BytePassthrough_Exact(t *testing.T) {
	// Неалфавитный порядок: map-re-marshal дал бы init_system перед pkg_mgr и
	// os перед sid — passthrough сохраняет исходный.
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
	// typed_facts в теле должен быть БАЙТ-В-БАЙТ равен raw (не пересортирован).
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

// TestGetSoulprint_ForwardCompat_UnknownKey — guard forward-compat (finding 2):
// extension-ключ, которого НЕТ в proto SoulprintFacts, ПРИСУТСТВУЕТ на wire.
// Доказывает, что Keeper не парсит/не фильтрует содержимое typed_facts —
// будущие proto-поля Soul-агента доезжают до клиента без рекомпиляции Keeper-а.
func TestGetSoulprint_ForwardCompat_UnknownKey(t *testing.T) {
	// `future_field` отсутствует в SoulprintFacts (proto/keeper/v1/soulprint.proto).
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

// fakeHistoryPool — мок [SoulPool] для GET /v1/souls/{sid}/history. QueryRow
// обслуживает ДВА запроса: scope-gate (SelectBySID — souls-row с covens, ADR-047
// G1 Фикс 3) и count-SQL (total). Query — page-SQL (historyRows). Захватывает
// SQL/args page-вызова для проверки фильтров (type/since/pagination).
type fakeHistoryPool struct {
	total    int
	items    []soul.HistoryItem
	queryErr error

	// hostCovens — covens хоста для scope-gate InScope (Фикс 3). nil → хост с
	// пустыми covens (unrestricted-оператор видит, coven-scoped — нет).
	hostCovens []string
	// soulMissing=true → SelectBySID отвечает pgx.ErrNoRows (404 до history-фетча).
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
	// scope-gate (SelectBySID) — souls-row с covens; идёт ПЕРЕД count-SQL history.
	if strings.Contains(sql, "FROM souls") && strings.Contains(sql, "WHERE sid = $1") {
		if f.soulMissing {
			return errRow{err: pgx.ErrNoRows}
		}
		return staticRow{values: []any{
			"soul.example.com", "agent", "connected", f.hostCovens,
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

// historyRows — pgx.Rows над []soul.HistoryItem, эмулирует 9-колоночную
// проекцию SelectHistory (type,id,incarnation,scenario,module,status,
// started_at,finished_at,voyage_id) с nullable-указателями.
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

// doGetHistory вызывает HistoryTyped напрямую (handler-native T5d), разбирая rawQuery
// (type[]/since/offset/limit) так же, как прежний (w,r)-роут, и сериализуя результат в
// recorder. scope-гейт History резолвит readScope из claims (ADR-047 G1 Фикс 3).
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

// soulHistoryViewJSON проецирует доменный SoulHistoryView в map с теми же json-ключами,
// что прежний wire (для downstream-ассертов теста). Воспроизводит форму native
// SoulHistoryReply (sid/items/offset/limit/total; per-item pointer-omitempty).
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
			// errand с back-link на Voyage (ADR-043) — voyage_id присутствует.
			{Type: soul.HistoryTypeErrand, ID: "e1", Module: "core.exec.run", Status: "success", StartedAt: now.Add(time.Hour), FinishedAt: &fin, VoyageID: &voyageID},
			// прямой scenario-run без Voyage — voyage_id опускается.
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
	// errand-строка: module есть, scenario-поля пусты (omitempty).
	if out.Items[0].Type != "errand" || out.Items[0].Module != "core.exec.run" {
		t.Errorf("item0 = %+v", out.Items[0])
	}
	if out.Items[0].Incarnation != "" || out.Items[0].Scenario != "" {
		t.Errorf("item0 scenario-поля просочились: %+v", out.Items[0])
	}
	if out.Items[0].FinishedAt == "" {
		t.Errorf("item0 terminal — finished_at должен быть: %+v", out.Items[0])
	}
	// item0 — Voyage back-link (ADR-043) проброшен.
	if out.Items[0].VoyageID != "vy-1" {
		t.Errorf("item0 voyage_id = %q, want vy-1", out.Items[0].VoyageID)
	}
	// scenario-строка: incarnation/scenario есть, module/finished_at (running) пусты.
	if out.Items[1].Type != "scenario" || out.Items[1].Incarnation != "web" ||
		out.Items[1].Scenario != "deploy" {
		t.Errorf("item1 = %+v", out.Items[1])
	}
	if out.Items[1].Module != "" || out.Items[1].FinishedAt != "" {
		t.Errorf("item1 errand-поля/finished просочились: %+v", out.Items[1])
	}
	// item1 — прямой run без Voyage: voyage_id опущен (omitempty).
	if out.Items[1].VoyageID != "" {
		t.Errorf("item1 voyage_id должен быть пуст (вне Voyage): %q", out.Items[1].VoyageID)
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
	// since → $2; LIMIT/OFFSET — последние два arg-а (5, 10).
	if !strings.Contains(pool.querySQL, "started_at > $2") {
		t.Errorf("since не проброшен в SQL: %s", pool.querySQL)
	}
	n := len(pool.queryArgs)
	if n < 4 {
		t.Fatalf("queryArgs = %v, want sid+since+limit+offset", pool.queryArgs)
	}
	if pool.queryArgs[n-2] != 5 || pool.queryArgs[n-1] != 10 {
		t.Errorf("limit/offset args = %v, want [...,5,10]", pool.queryArgs)
	}
	// Оба источника (type=scenario+errand) → UNION ALL.
	if !strings.Contains(pool.querySQL, "UNION ALL") {
		t.Errorf("оба типа должны давать UNION ALL: %s", pool.querySQL)
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

// TestHistory_InScope_CovenMatch_200 — guard (ADR-047 G1 Фикс 3): coven-scoped
// оператор читает timeline хоста СВОЕГО coven → 200 (scope-gate пропускает).
func TestHistory_InScope_CovenMatch_200(t *testing.T) {
	pool := &fakeHistoryPool{total: 0, hostCovens: []string{"prod"}}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"prod"}}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history своего coven = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHistory_OutOfScope_404 — guard (Фикс 3, закрытие утечки): coven-scoped
// оператор читает timeline ЧУЖОГО хоста (covens=[prod], scope=[staging]) → 404
// (не палит существование и не раскрывает timeline). До Фикс 3 History фетчила
// SelectHistory по SID напрямую без scope-проверки — утечка истории прогонов.
func TestHistory_OutOfScope_404(t *testing.T) {
	pool := &fakeHistoryPool{total: 5, hostCovens: []string{"prod"}}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"staging"}}, nil, nil)
	rec := doGetHistory(t, h, "soul.example.com", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("history чужого хоста = %d, want 404 (scope-gate режет утечку); body=%s", rec.Code, rec.Body.String())
	}
}

// TestHistory_OutOfRegexScope_404 — guard (Фикс 3, list↔get-консистентность по
// regex): regex-scoped оператор (^web-) читает не-матчащий хост → 404.
func TestHistory_OutOfRegexScope_404(t *testing.T) {
	pool := &fakeHistoryPool{total: 5, hostCovens: []string{"prod"}}
	h := NewSoulHandler(pool, fakeScoper{regexes: []string{"^web-"}}, nil, nil)
	rec := doGetHistory(t, h, "db-01.example.com", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("history не-матчащего хоста = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHistory_HostMissing_404 — guard (Фикс 3): несуществующий хост → 404 на
// scope-gate (SelectBySID ErrNoRows), не доходя до SelectHistory.
func TestHistory_HostMissing_404(t *testing.T) {
	pool := &fakeHistoryPool{soulMissing: true}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doGetHistory(t, h, "ghost.example.com", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("history несуществующего хоста = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
