package herald

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// --- HMAC-подпись (unit) ---

func TestSignBody_HMACSHA256(t *testing.T) {
	secret := []byte("s3cr3t-token")
	body := []byte(`{"event_type":"voyage.reclaimed"}`)

	got := signBody(secret, body)

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("signBody = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "sha256=") {
		t.Fatalf("signature must carry algorithm prefix, got %q", got)
	}
}

func TestSignBody_DiffersByBodyAndKey(t *testing.T) {
	a := signBody([]byte("k1"), []byte("body"))
	b := signBody([]byte("k2"), []byte("body"))
	c := signBody([]byte("k1"), []byte("BODY"))
	if a == b || a == c {
		t.Fatalf("signature must depend on both key and body: a=%q b=%q c=%q", a, b, c)
	}
}

// --- payload-формат (unit) ---

func TestBuildPayload_ShapeAndFields(t *testing.T) {
	occurred := time.Date(2026, 6, 11, 10, 30, 0, 0, time.UTC)
	job := &DeliveryJob{
		EventType:     audit.EventScenarioRunFailed,
		Herald:        "ops-webhook",
		Tiding:        "nightly-failures",
		CorrelationID: "voyage-123",
		OccurredAt:    occurred,
		PayloadCopy:   map[string]any{"voyage_id": "voyage-123", "kind": "scenario"},
	}

	body, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["event_type"] != "scenario_run.failed" {
		t.Errorf("event_type = %v", got["event_type"])
	}
	if got["herald"] != "ops-webhook" {
		t.Errorf("herald = %v", got["herald"])
	}
	if got["tiding"] != "nightly-failures" {
		t.Errorf("tiding = %v", got["tiding"])
	}
	if got["occurred_at"] != "2026-06-11T10:30:00Z" {
		t.Errorf("occurred_at = %v", got["occurred_at"])
	}
	p, ok := got["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not an object: %T", got["payload"])
	}
	if p["voyage_id"] != "voyage-123" {
		t.Errorf("payload.voyage_id = %v", p["voyage_id"])
	}
}

// TestBuildPayload_MasksSecretInPayload — defence-in-depth: даже если job-payload
// несёт vault-ref-подобную строку, наружу она уходит замаскированной (инвариант
// A ADR-027 + MaskSecrets на выходе).
func TestBuildPayload_MasksSecretInPayload(t *testing.T) {
	job := &DeliveryJob{
		EventType:   audit.EventCommandRunCompleted,
		PayloadCopy: map[string]any{"note": "see vault:secret/keeper/x for creds"},
	}
	body, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if strings.Contains(string(body), "vault:secret/keeper/x") {
		t.Fatalf("vault-ref leaked into webhook payload: %s", body)
	}
}

// --- резолвер путей projection (unit, ADR-052(h) N3) ---

func TestResolvePath(t *testing.T) {
	src := map[string]any{
		"voyage_id": "v1",
		"summary": map[string]any{
			"succeeded": float64(3),
			"failed":    float64(0),
		},
		"kind": "scenario",
	}
	tests := []struct {
		name    string
		path    string
		wantVal any
		wantOK  bool
	}{
		{"верхнеуровневый", "voyage_id", "v1", true},
		{"вложенный", "summary.succeeded", float64(3), true},
		{"отсутствующий верхнеуровневый", "nope", nil, false},
		{"отсутствующий вложенный сегмент", "summary.missing", nil, false},
		{"спуск сквозь лист (не объект)", "kind.deeper", nil, false},
		{"спуск сквозь лист верхнеуровневый", "voyage_id.x", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolvePath(src, strings.Split(tc.path, "."))
			if ok != tc.wantOK {
				t.Fatalf("resolvePath(%q) ok = %v, want %v", tc.path, ok, tc.wantOK)
			}
			if ok && got != tc.wantVal {
				t.Fatalf("resolvePath(%q) = %v, want %v", tc.path, got, tc.wantVal)
			}
		})
	}
}

// TestProjectPayload_NestedShape — спроецированное тело сохраняет ВЛОЖЕННУЮ форму
// исходного payload (summary.succeeded → {summary:{succeeded:N}}), отсутствующие
// пути пропускаются.
func TestProjectPayload_NestedShape(t *testing.T) {
	src := map[string]any{
		"voyage_id": "v1",
		"summary": map[string]any{
			"succeeded": float64(3),
			"failed":    float64(1),
		},
		"secret_field": "drop-me",
	}
	out := projectPayload(src, []string{"voyage_id", "summary.succeeded", "summary.missing", "absent.path"})

	// allow-list: только перечисленные существующие пути.
	if len(out) != 2 {
		t.Fatalf("projected top-level keys = %v, want voyage_id+summary", out)
	}
	if out["voyage_id"] != "v1" {
		t.Errorf("voyage_id = %v", out["voyage_id"])
	}
	summary, ok := out["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary not nested object: %T", out["summary"])
	}
	if summary["succeeded"] != float64(3) {
		t.Errorf("summary.succeeded = %v, want 3", summary["succeeded"])
	}
	// summary.failed НЕ в allow-list — должен отсутствовать.
	if _, present := summary["failed"]; present {
		t.Errorf("summary.failed leaked (not in allow-list)")
	}
	// поле не из allow-list не утекло.
	if _, present := out["secret_field"]; present {
		t.Errorf("secret_field leaked — projection is allow-list, not deny-list")
	}
}

// TestProjectPayload_PrefixCollision_OrderInvariant — projection-пути, где один
// путь является префиксом другого (`summary` И `summary.failed`), не валят
// insertPath и дают ДЕТЕРМИНИРОВАННЫЙ результат, не зависящий от порядка путей в
// списке. Инвариант от регресса: смена порядка merge/insert не должна менять
// спроецированное тело (иначе одинаковое правило давало бы разный webhook-payload
// от перестановки allow-list). Каждому порядку — свой свежий src: insertPath при
// коллизии мутирует вложенный объект, поэтому общий src между прогонами исказил бы
// замер.
func TestProjectPayload_PrefixCollision_OrderInvariant(t *testing.T) {
	newSrc := func() map[string]any {
		return map[string]any{
			"summary": map[string]any{"succeeded": float64(3), "failed": float64(1)},
		}
	}

	// panic-safe: оба порядка отрабатывают без паники (insertPath не должен
	// предполагать, что промежуточный сегмент ещё не занят листом/объектом).
	broadFirst := projectPayload(newSrc(), []string{"summary", "summary.failed"})
	deepFirst := projectPayload(newSrc(), []string{"summary.failed", "summary"})

	if !reflect.DeepEqual(broadFirst, deepFirst) {
		t.Fatalf("prefix-collision projection зависит от порядка путей:\n  broad-first = %#v\n  deep-first  = %#v", broadFirst, deepFirst)
	}
	// И сам результат — корректное вложенное тело (broad-путь подтягивает весь
	// объект, более узкий путь не «обрезает» его).
	summary, ok := broadFirst["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary not nested object: %T", broadFirst["summary"])
	}
	if summary["succeeded"] != float64(3) || summary["failed"] != float64(1) {
		t.Errorf("prefix-collision summary = %v, want {succeeded:3, failed:1}", summary)
	}
}

// TestProjectPayload_DoesNotMutateSrc — инвариант: projectPayload(src, paths) НЕ
// изменяет src ни при каком наборе paths, ВКЛЮЧАЯ коллизию префиксов. Latent
// side-effect (N3): широкий путь клал в out ссылку на вложенную map из src,
// глубокая вставка домутировала бы её. Снимаем deep-snapshot src до вызова,
// сверяем deep-equal после.
func TestProjectPayload_DoesNotMutateSrc(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
	}{
		{"prefix-collision broad-first", []string{"summary", "summary.failed"}},
		{"prefix-collision deep-first", []string{"summary.failed", "summary"}},
		{"deep insert into existing branch", []string{"summary.succeeded"}},
		{"top-level + nested", []string{"voyage_id", "summary.failed"}},
		{"absent paths", []string{"absent", "summary.missing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := map[string]any{
				"voyage_id": "v1",
				"summary":   map[string]any{"succeeded": float64(3), "failed": float64(1)},
			}
			before := deepCopyValue(src) // независимый snapshot формы src

			_ = projectPayload(src, tc.paths)

			if !reflect.DeepEqual(src, before) {
				t.Fatalf("projectPayload мутировал src:\n  before = %#v\n  after  = %#v", before, src)
			}
		})
	}
}

// TestBuildPayload_RetryIdempotent — buildPayload вызывается ПОВТОРНО на каждый
// retry-attempt над тем же job.PayloadCopy. Тело обязано быть идентичным между
// попытками, а сам PayloadCopy — неизменным (latent side-effect projection-а мог
// бы исказить тело при ретраях). Projection с коллизией префиксов — худший кейс.
func TestBuildPayload_RetryIdempotent(t *testing.T) {
	job := &DeliveryJob{
		EventType: audit.EventScenarioRunFailed,
		Herald:    "h", Tiding: "t",
		PayloadCopy: map[string]any{
			"voyage_id": "v1",
			"summary":   map[string]any{"succeeded": float64(3), "failed": float64(1)},
		},
		Projection:  []string{"summary", "summary.failed"},
		Annotations: map[string]any{"team": "ops"},
	}
	payloadSnapshot := deepCopyValue(job.PayloadCopy)

	first, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload (attempt 0): %v", err)
	}
	// Имитация retry: тот же job, тот же PayloadCopy.
	second, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload (retry): %v", err)
	}

	if string(first) != string(second) {
		t.Fatalf("retry дал иное тело:\n  attempt 0 = %s\n  retry     = %s", first, second)
	}
	if !reflect.DeepEqual(job.PayloadCopy, payloadSnapshot) {
		t.Fatalf("buildPayload мутировал job.PayloadCopy между ретраями:\n  before = %#v\n  after  = %#v", payloadSnapshot, job.PayloadCopy)
	}
}

// TestProjectPayload_AllPathsAbsent — все пути промахнулись → пустой объект (не
// nil, не ошибка): полностью отфильтрованный payload.
func TestProjectPayload_AllPathsAbsent(t *testing.T) {
	out := projectPayload(map[string]any{"a": 1}, []string{"x", "y.z"})
	if out == nil {
		t.Fatal("projectPayload returned nil, want empty object")
	}
	if len(out) != 0 {
		t.Fatalf("projected = %v, want empty", out)
	}
}

// TestBuildPayload_Projection_NarrowsPayload — projection непуст → payload в теле
// = подмножество по путям; пустые остальные поля в дефолт-форме.
func TestBuildPayload_Projection_NarrowsPayload(t *testing.T) {
	job := &DeliveryJob{
		EventType: audit.EventScenarioRunFailed,
		Herald:    "h", Tiding: "t",
		PayloadCopy: map[string]any{
			"voyage_id": "v1",
			"summary":   map[string]any{"succeeded": float64(3), "failed": float64(2)},
			"noise":     "ignored",
		},
		Projection: []string{"voyage_id", "summary.failed"},
	}
	body, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var got webhookPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Payload["voyage_id"] != "v1" {
		t.Errorf("payload.voyage_id = %v", got.Payload["voyage_id"])
	}
	if _, present := got.Payload["noise"]; present {
		t.Errorf("payload.noise leaked despite projection allow-list")
	}
	summary, ok := got.Payload["summary"].(map[string]any)
	if !ok {
		t.Fatalf("payload.summary not object: %T", got.Payload["summary"])
	}
	if summary["failed"] != float64(2) {
		t.Errorf("payload.summary.failed = %v, want 2", summary["failed"])
	}
	if _, present := summary["succeeded"]; present {
		t.Errorf("summary.succeeded leaked (not in projection)")
	}
}

// TestBuildPayload_EmptyProjection_FullForm — пустой projection → payload целиком
// (backward-compat дефолт).
func TestBuildPayload_EmptyProjection_FullForm(t *testing.T) {
	job := &DeliveryJob{
		EventType:   audit.EventScenarioRunFailed,
		PayloadCopy: map[string]any{"voyage_id": "v1", "kind": "scenario"},
		// Projection nil
	}
	body, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var got webhookPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Payload["voyage_id"] != "v1" || got.Payload["kind"] != "scenario" {
		t.Fatalf("empty projection must keep full payload, got %v", got.Payload)
	}
}

// TestBuildPayload_Annotations_Added — непустые annotations → верхнеуровневый
// ключ `annotations` в теле (additive, НЕ внутри payload).
func TestBuildPayload_Annotations_Added(t *testing.T) {
	job := &DeliveryJob{
		EventType:   audit.EventScenarioRunFailed,
		PayloadCopy: map[string]any{"voyage_id": "v1"},
		Annotations: map[string]any{"team": "ops", "severity": "high"},
	}
	body, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ann, ok := got["annotations"].(map[string]any)
	if !ok {
		t.Fatalf("annotations not a top-level object: %T", got["annotations"])
	}
	if ann["team"] != "ops" || ann["severity"] != "high" {
		t.Errorf("annotations = %v", ann)
	}
	// annotations НЕ должны попасть внутрь payload.
	p := got["payload"].(map[string]any)
	if _, present := p["team"]; present {
		t.Errorf("annotations leaked into payload")
	}
}

// TestBuildPayload_EmptyAnnotations_Omitted — пустые annotations → ключ опущен
// (omitempty), приёмники без поддержки annotations не ломаются.
func TestBuildPayload_EmptyAnnotations_Omitted(t *testing.T) {
	job := &DeliveryJob{
		EventType:   audit.EventScenarioRunFailed,
		PayloadCopy: map[string]any{"voyage_id": "v1"},
		// Annotations nil
	}
	body, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if strings.Contains(string(body), "annotations") {
		t.Fatalf("empty annotations must be omitted, got: %s", body)
	}
}

// TestBuildPayload_AnnotationsAndProjection_Together — оба поля вместе: payload
// сужен projection-ом, annotations добавлены верхнеуровневым ключом.
func TestBuildPayload_AnnotationsAndProjection_Together(t *testing.T) {
	job := &DeliveryJob{
		EventType: audit.EventScenarioRunFailed,
		PayloadCopy: map[string]any{
			"voyage_id": "v1",
			"summary":   map[string]any{"succeeded": float64(5)},
			"drop":      "x",
		},
		Projection:  []string{"summary.succeeded"},
		Annotations: map[string]any{"runbook": "https://wiki/x"},
	}
	body, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var got webhookPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got.Payload["voyage_id"]; present {
		t.Errorf("voyage_id not in projection, must be dropped")
	}
	summary, ok := got.Payload["summary"].(map[string]any)
	if !ok || summary["succeeded"] != float64(5) {
		t.Fatalf("projected summary.succeeded missing: %v", got.Payload)
	}
	if got.Annotations["runbook"] != "https://wiki/x" {
		t.Errorf("annotations.runbook = %v", got.Annotations["runbook"])
	}
}

// TestBuildPayload_Projection_AfterMaskSecrets — projection применяется к
// УЖЕ-замаскированному payload: allow-list пути в замаскированное поле не
// разворачивает секрет (секрет-гигиена, ADR-052(h)).
func TestBuildPayload_Projection_AfterMaskSecrets(t *testing.T) {
	job := &DeliveryJob{
		EventType: audit.EventCommandRunCompleted,
		PayloadCopy: map[string]any{
			"note": "creds at vault:secret/keeper/x",
		},
		Projection: []string{"note"},
	}
	body, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if strings.Contains(string(body), "vault:secret/keeper/x") {
		t.Fatalf("projection over masked payload leaked vault-ref: %s", body)
	}
}

// TestBuildPayload_HMACFromFinalBody — подпись считается от ФИНАЛЬНОГО тела
// (после projection+annotations), а не от исходного полного payload. Guard:
// смена порядка (подпись до сборки тела) сломает этот тест.
func TestBuildPayload_HMACFromFinalBody(t *testing.T) {
	secret := []byte("sign-key")
	job := &DeliveryJob{
		EventType: audit.EventScenarioRunFailed,
		PayloadCopy: map[string]any{
			"voyage_id": "v1",
			"drop":      "should-not-be-signed",
		},
		Projection:  []string{"voyage_id"},
		Annotations: map[string]any{"team": "ops"},
	}
	body, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	// Подпись над финальным телом — то, что делает deliver перед POST.
	gotSig := signBody(secret, body)

	// Тело содержит annotations + суженный payload, но НЕ исходное drop-поле.
	if strings.Contains(string(body), "should-not-be-signed") {
		t.Fatalf("final body must not carry dropped field: %s", body)
	}
	// Подпись над ИСХОДНЫМ полным payload отличается (доказывает, что подписали
	// финальное, а не исходное тело).
	full := &DeliveryJob{EventType: job.EventType, PayloadCopy: job.PayloadCopy}
	fullBody, _ := buildPayload(full)
	if signBody(secret, fullBody) == gotSig {
		t.Fatalf("signature must differ between full and projected/annotated body")
	}
}

// --- retry-счёт и терминалы (integration of worker logic, fake backend) ---

// fakeBackend — in-memory backend очереди для тестов worker-а. Список pending +
// processing + lease-флаги. Потокобезопасен.
type fakeBackend struct {
	mu         sync.Mutex
	pending    [][]byte
	processing [][]byte
	leases     map[string]bool
	acks       int
	requeues   int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{leases: map[string]bool{}}
}

func (b *fakeBackend) Enqueue(_ context.Context, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, payload)
	return nil
}

func (b *fakeBackend) Claim(_ context.Context, _ time.Duration) (*ClaimedJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return nil, nil
	}
	p := b.pending[0]
	b.pending = b.pending[1:]
	b.processing = append(b.processing, p)
	return &ClaimedJob{Payload: p}, nil
}

func (b *fakeBackend) SetLease(_ context.Context, jobID string, _ time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.leases[jobID] = true
	return nil
}

func (b *fakeBackend) Ack(_ context.Context, jobID string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acks++
	b.removeProcessing(payload)
	delete(b.leases, jobID)
	return nil
}

func (b *fakeBackend) Requeue(_ context.Context, jobID string, oldPayload, newPayload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requeues++
	b.removeProcessing(oldPayload)
	b.pending = append(b.pending, newPayload)
	delete(b.leases, jobID)
	return nil
}

func (b *fakeBackend) RequeueExpired(_ context.Context, _ func([]byte) (string, bool)) (int, error) {
	return 0, nil
}

func (b *fakeBackend) removeProcessing(payload []byte) {
	for i, p := range b.processing {
		if string(p) == string(payload) {
			b.processing = append(b.processing[:i], b.processing[i+1:]...)
			return
		}
	}
}

func (b *fakeBackend) pendingJobs(t *testing.T) []*DeliveryJob {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*DeliveryJob, 0, len(b.pending))
	for _, p := range b.pending {
		j, err := unmarshalJob(p)
		if err != nil {
			t.Fatalf("unmarshalJob: %v", err)
		}
		out = append(out, j)
	}
	return out
}

// recordingHeralds — реестр heralds для теста: один webhook-канал.
type recordingHeralds struct {
	herald *Herald
	err    error
}

func (r recordingHeralds) HeraldByName(_ context.Context, _ string) (*Herald, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.herald, nil
}

// recordingAudit — собирает записанные audit-события.
type recordingAudit struct {
	mu     sync.Mutex
	events []*audit.Event
}

func (a *recordingAudit) Write(_ context.Context, ev *audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

func (a *recordingAudit) terminals() []*audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*audit.Event(nil), a.events...)
}

// TestHandle_TransientFailure_RequeuesWithIncrementedAttempt — сбой доставки
// (канал есть, но URL ведёт в никуда → SSRF-guard пропускает literal-IP? нет:
// используем no-retry vs retry). Здесь проверяем retry-СЧЁТ через requeue на
// транзиентной ошибке (Vault-сбой при резолве secret_ref — retryable).
func TestHandle_TransientFailure_RequeuesWithIncrementedAttempt(t *testing.T) {
	backend := newFakeBackend()
	secretRef := "vault:secret/keeper/herald-sign"
	h := &Herald{
		Name:      "sign-webhook",
		Type:      HeraldWebhook,
		Config:    map[string]any{"url": "https://example.test/hook"},
		SecretRef: &secretRef,
		Enabled:   true,
	}
	w := &DeliveryWorker{
		Queue:   backend,
		Heralds: recordingHeralds{herald: h},
		KV:      failingKV{}, // Vault-сбой при резолве signing-token → retryable
		Logger:  discardLogger(),
		Backoff: fastBackoff,
	}
	job := &DeliveryJob{ID: "j1", Attempt: 0, Herald: "sign-webhook", EventType: audit.EventScenarioRunFailed}
	payload, _ := marshalJob(job)

	w.handle(context.Background(), payload)

	if backend.requeues != 1 {
		t.Fatalf("requeues = %d, want 1 (transient failure must retry)", backend.requeues)
	}
	pj := backend.pendingJobs(t)
	if len(pj) != 1 || pj[0].Attempt != 1 {
		t.Fatalf("requeued job attempt = %v, want 1", pj)
	}
}

// TestHandle_RetryExhausted_TerminalFailed — на последней попытке (attempt
// достигает retryMax-1) сбой даёт терминальный herald.failed, без перепостановки.
func TestHandle_RetryExhausted_TerminalFailed(t *testing.T) {
	backend := newFakeBackend()
	rec := &recordingAudit{}
	secretRef := "vault:secret/keeper/herald-sign"
	h := &Herald{
		Name: "sign-webhook", Type: HeraldWebhook,
		Config: map[string]any{"url": "https://example.test/hook"}, SecretRef: &secretRef, Enabled: true,
	}
	w := &DeliveryWorker{
		Queue: backend, Heralds: recordingHeralds{herald: h},
		KV: failingKV{}, Audit: rec, Logger: discardLogger(), Backoff: fastBackoff,
	}
	// Последняя попытка: attempt = retryMax-1. +1 >= retryMax → терминал.
	lastAttempt := w.retryMax() - 1
	job := &DeliveryJob{ID: "jX", Attempt: lastAttempt, Herald: "sign-webhook", EventType: audit.EventScenarioRunFailed}
	payload, _ := marshalJob(job)

	w.handle(context.Background(), payload)

	if backend.requeues != 0 {
		t.Fatalf("requeues = %d, want 0 (retry must be exhausted)", backend.requeues)
	}
	if backend.acks != 1 {
		t.Fatalf("acks = %d, want 1 (terminal acks the job)", backend.acks)
	}
	terms := rec.terminals()
	if len(terms) != 1 || terms[0].EventType != audit.EventHeraldFailed {
		t.Fatalf("terminal events = %+v, want one herald.failed", terms)
	}
	if terms[0].Source != audit.SourceKeeperInternal {
		t.Errorf("terminal source = %q, want keeper_internal", terms[0].Source)
	}
}

// TestHandle_ChannelDisabled_TerminalNoRetry — выключенный канал → терминал без
// retry даже на первой попытке.
func TestHandle_ChannelDisabled_TerminalNoRetry(t *testing.T) {
	backend := newFakeBackend()
	rec := &recordingAudit{}
	h := &Herald{Name: "off", Type: HeraldWebhook, Config: map[string]any{"url": "https://example.test/h"}, Enabled: false}
	w := &DeliveryWorker{Queue: backend, Heralds: recordingHeralds{herald: h}, Audit: rec, Logger: discardLogger()}
	job := &DeliveryJob{ID: "j0", Attempt: 0, Herald: "off", EventType: audit.EventVoyageReclaimed}
	payload, _ := marshalJob(job)

	w.handle(context.Background(), payload)

	if backend.requeues != 0 {
		t.Fatalf("disabled channel must not retry, requeues=%d", backend.requeues)
	}
	terms := rec.terminals()
	if len(terms) != 1 || terms[0].EventType != audit.EventHeraldFailed {
		t.Fatalf("want one herald.failed terminal, got %+v", terms)
	}
}

// TestHandle_ChannelNotFound_TerminalNoRetry — канал снесён между постановкой и
// доставкой → терминал без retry.
func TestHandle_ChannelNotFound_TerminalNoRetry(t *testing.T) {
	backend := newFakeBackend()
	rec := &recordingAudit{}
	w := &DeliveryWorker{
		Queue:   backend,
		Heralds: recordingHeralds{err: ErrHeraldNotFound},
		Audit:   rec, Logger: discardLogger(),
	}
	job := &DeliveryJob{ID: "jg", Attempt: 0, Herald: "ghost", EventType: audit.EventScenarioRunFailed}
	payload, _ := marshalJob(job)

	w.handle(context.Background(), payload)

	if backend.requeues != 0 {
		t.Fatalf("missing channel must not retry, requeues=%d", backend.requeues)
	}
	terms := rec.terminals()
	if len(terms) != 1 || terms[0].EventType != audit.EventHeraldFailed {
		t.Fatalf("want one herald.failed terminal, got %+v", terms)
	}
}

// --- maskErr (unit) ---

func TestMaskErr_MasksVaultRef(t *testing.T) {
	err := fmt.Errorf("read failed for vault:secret/keeper/sign")
	got := maskErr(err)
	if strings.Contains(got, "vault:secret/keeper/sign") {
		t.Fatalf("maskErr leaked vault-ref: %q", got)
	}
}

func TestMaskErr_Nil(t *testing.T) {
	if got := maskErr(nil); got != "" {
		t.Fatalf("maskErr(nil) = %q, want empty", got)
	}
}

// --- helpers ---

// failingKV — KVReader, всегда возвращающий ошибку (имитация Vault-сбоя).
type failingKV struct{}

func (failingKV) ReadKV(_ context.Context, _ string) (map[string]any, error) {
	return nil, errors.New("vault unavailable")
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fastBackoff — короткие задержки между попытками для тестов (3 повтора по 1ms;
// retryMax=4 как в проде, но без реального ожидания минут).
var fastBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
