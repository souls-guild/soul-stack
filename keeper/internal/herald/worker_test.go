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

// --- HMAC signature (unit) ---

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

// --- payload format (unit) ---

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

// TestBuildPayload_MasksSecretInPayload is defence in depth: even if job payload
// carries a vault-ref-like string, it leaves masked (invariant A ADR-027 +
// MaskSecrets on output).
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

// --- projection path resolver (unit, ADR-052(h) N3) ---

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
		{"top-level", "voyage_id", "v1", true},
		{"nested", "summary.succeeded", float64(3), true},
		{"missing top-level", "nope", nil, false},
		{"missing nested segment", "summary.missing", nil, false},
		{"descend through leaf (not object)", "kind.deeper", nil, false},
		{"descend through top-level leaf", "voyage_id.x", nil, false},
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

// TestProjectPayload_NestedShape verifies that projected body preserves the nested
// shape of the source payload (summary.succeeded -> {summary:{succeeded:N}}) and
// skips absent paths.
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

	// allow-list: only listed existing paths.
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
	// summary.failed is not in the allow-list and must be absent.
	if _, present := summary["failed"]; present {
		t.Errorf("summary.failed leaked (not in allow-list)")
	}
	// Field outside the allow-list must not leak.
	if _, present := out["secret_field"]; present {
		t.Errorf("secret_field leaked: projection is allow-list, not deny-list")
	}
}

// TestProjectPayload_PrefixCollision_OrderInvariant verifies that projection paths
// where one path prefixes another (`summary` and `summary.failed`) do not break
// insertPath and produce deterministic output regardless of path order. Regression
// invariant: changing merge/insert order must not change the projected body, or the
// same rule would produce different webhook payloads when allow-list order changes.
// Each order gets a fresh src: on collision, insertPath mutates the nested object,
// so sharing src between runs would distort the measurement.
func TestProjectPayload_PrefixCollision_OrderInvariant(t *testing.T) {
	newSrc := func() map[string]any {
		return map[string]any{
			"summary": map[string]any{"succeeded": float64(3), "failed": float64(1)},
		}
	}

	// panic-safe: both orders run without panic. insertPath must not assume an
	// intermediate segment is not already occupied by a leaf/object.
	broadFirst := projectPayload(newSrc(), []string{"summary", "summary.failed"})
	deepFirst := projectPayload(newSrc(), []string{"summary.failed", "summary"})

	if !reflect.DeepEqual(broadFirst, deepFirst) {
		t.Fatalf("prefix-collision projection depends on path order:\n  broad-first = %#v\n  deep-first  = %#v", broadFirst, deepFirst)
	}
	// And the result itself is a correct nested body: the broad path brings the
	// whole object, and the narrower path does not trim it.
	summary, ok := broadFirst["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary not nested object: %T", broadFirst["summary"])
	}
	if summary["succeeded"] != float64(3) || summary["failed"] != float64(1) {
		t.Errorf("prefix-collision summary = %v, want {succeeded:3, failed:1}", summary)
	}
}

// TestProjectPayload_DoesNotMutateSrc is the invariant that projectPayload(src,
// paths) never changes src for any path set, including prefix collisions. Latent
// side effect (N3): a broad path used to put a reference to a nested map from src
// into out, and a deep insertion would mutate it. Take a deep snapshot before the
// call and compare with deep-equal afterwards.
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
			before := deepCopyValue(src) // Independent snapshot of src shape.

			_ = projectPayload(src, tc.paths)

			if !reflect.DeepEqual(src, before) {
				t.Fatalf("projectPayload mutated src:\n  before = %#v\n  after  = %#v", before, src)
			}
		})
	}
}

// TestBuildPayload_RetryIdempotent verifies that buildPayload is called again for
// every retry attempt over the same job.PayloadCopy. Body must be identical across
// attempts, and PayloadCopy must stay unchanged. A latent projection side effect
// could distort body across retries. Projection with prefix collision is the worst
// case.
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
	// Retry simulation: same job, same PayloadCopy.
	second, err := buildPayload(job)
	if err != nil {
		t.Fatalf("buildPayload (retry): %v", err)
	}

	if string(first) != string(second) {
		t.Fatalf("retry produced a different body:\n  attempt 0 = %s\n  retry     = %s", first, second)
	}
	if !reflect.DeepEqual(job.PayloadCopy, payloadSnapshot) {
		t.Fatalf("buildPayload mutated job.PayloadCopy between retries:\n  before = %#v\n  after  = %#v", payloadSnapshot, job.PayloadCopy)
	}
}

// TestProjectPayload_AllPathsAbsent verifies that all missing paths produce an
// empty object, not nil or error: fully filtered payload.
func TestProjectPayload_AllPathsAbsent(t *testing.T) {
	out := projectPayload(map[string]any{"a": 1}, []string{"x", "y.z"})
	if out == nil {
		t.Fatal("projectPayload returned nil, want empty object")
	}
	if len(out) != 0 {
		t.Fatalf("projected = %v, want empty", out)
	}
}

// TestBuildPayload_Projection_NarrowsPayload verifies that non-empty projection
// makes body payload a subset by paths, with other default-form fields empty.
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

// TestBuildPayload_EmptyProjection_FullForm verifies that empty projection keeps
// the full payload (backward-compatible default).
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

// TestBuildPayload_Annotations_Added verifies that non-empty annotations add a
// top-level `annotations` key to the body (additive, not inside payload).
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
	// annotations must not get inside payload.
	p := got["payload"].(map[string]any)
	if _, present := p["team"]; present {
		t.Errorf("annotations leaked into payload")
	}
}

// TestBuildPayload_EmptyAnnotations_Omitted verifies that empty annotations omit
// the key (omitempty), so receivers without annotations support do not break.
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

// TestBuildPayload_AnnotationsAndProjection_Together covers both fields together:
// payload is narrowed by projection, annotations are added as a top-level key.
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

// TestBuildPayload_Projection_AfterMaskSecrets verifies that projection applies to
// the already masked payload: allow-listing a path into a masked field does not
// reveal the secret (secret hygiene, ADR-052(h)).
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

// TestBuildPayload_HMACFromFinalBody verifies that the signature is calculated
// from the final body (after projection+annotations), not from the original full
// payload. Guard: changing the order to sign before body assembly breaks this test.
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
	// Signature over the final body, matching what deliver does before POST.
	gotSig := signBody(secret, body)

	// Body contains annotations + narrowed payload, but not the original drop field.
	if strings.Contains(string(body), "should-not-be-signed") {
		t.Fatalf("final body must not carry dropped field: %s", body)
	}
	// Signature over the original full payload differs, proving we signed the final
	// body rather than the original body.
	full := &DeliveryJob{EventType: job.EventType, PayloadCopy: job.PayloadCopy}
	fullBody, _ := buildPayload(full)
	if signBody(secret, fullBody) == gotSig {
		t.Fatalf("signature must differ between full and projected/annotated body")
	}
}

// --- retry count and terminals (integration of worker logic, fake backend) ---

// fakeBackend is an in-memory queue backend for worker tests. It tracks pending,
// processing, and lease flags. Thread-safe.
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

// recordingHeralds is a heralds registry for tests: one webhook channel.
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

// recordingAudit collects written audit events.
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

// TestHandle_TransientFailure_RequeuesWithIncrementedAttempt verifies delivery
// failure retry counting through requeue on a transient error. The channel exists,
// but resolving secret_ref fails through Vault, which is retryable.
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
		KV:      failingKV{}, // Vault failure while resolving signing token: retryable.
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

// TestHandle_RetryExhausted_TerminalFailed verifies that failure on the last
// attempt (attempt reaches retryMax-1) produces terminal herald.failed without
// requeue.
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
	// Last attempt: attempt = retryMax-1. +1 >= retryMax means terminal.
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

// TestHandle_ChannelDisabled_TerminalNoRetry verifies that a disabled channel is
// terminal without retry even on the first attempt.
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

// TestHandle_ChannelNotFound_TerminalNoRetry verifies that a channel deleted
// between enqueue and delivery is terminal without retry.
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

// failingKV is a KVReader that always returns an error (Vault failure simulation).
type failingKV struct{}

func (failingKV) ReadKV(_ context.Context, _ string) (map[string]any, error) {
	return nil, errors.New("vault unavailable")
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fastBackoff is short delays between attempts for tests: 3 retries by 1ms, so
// retryMax=4 as in production but without real minutes of waiting.
var fastBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
