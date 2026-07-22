package herald

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/netguard"
)

// privateResolver netguard.Resolver that resolves any name to private IP (10.x);
// mimics DNS rebind to internal address. dial-guard must reject.
type privateResolver struct{}

func (privateResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("10.0.0.7")}}, nil
}

// runWorkerOnce claims one job and handles it (without full Run loop: deterministic,
// without background goroutine spawns except renewLease which is stopped by handle defer).
func runWorkerOnce(t *testing.T, w *DeliveryWorker) {
	t.Helper()
	if err := w.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	claimed, err := w.Queue.Claim(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected a claimed job, got empty queue")
	}
	w.handle(context.Background(), claimed.Payload)
}

// TestDelivery_Success_PostsSignedPayload checks end-to-end success: httptest receiver
// gets POST with correct body and valid HMAC signature; terminal is
// herald.delivered.
func TestDelivery_Success_PostsSignedPayload(t *testing.T) {
	var gotBody []byte
	var gotSig string
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(SignatureHeader)
		gotCT = r.Header.Get("Content-Type")
		rw.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	backend := newFakeBackend()
	rec := &recordingAudit{}
	secretRef := "vault:secret/keeper/sign#token"
	h := &Herald{
		Name: "ok-webhook", Type: HeraldWebhook,
		Config:    map[string]any{"url": srv.URL, "allow_private": true, "http_allowed": true}, // httptest on 127.0.0.1
		SecretRef: &secretRef, Enabled: true,
	}
	w := &DeliveryWorker{
		Queue: backend, Heralds: recordingHeralds{herald: h},
		KV:    stubKV{data: map[string]any{"token": "sign-key"}},
		Audit: rec, Logger: discardLogger(), Resolver: netguard.DefaultResolver,
	}

	occurred := time.Date(2026, 6, 11, 10, 30, 0, 0, time.UTC)
	job := &DeliveryJob{
		ID: "j-ok", Herald: "ok-webhook", Tiding: "t",
		EventType: audit.EventScenarioRunFailed, OccurredAt: occurred,
		PayloadCopy: map[string]any{"voyage_id": "v1"},
	}
	payload, _ := marshalJob(job)
	_ = backend.Enqueue(context.Background(), payload)

	runWorkerOnce(t, w)

	if len(gotBody) == 0 {
		t.Fatal("webhook receiver got no body")
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	// occurred_at in body is non-zero valid RFC3339 (guard against live-smoke bug:
	// delivery must not send occurred_at=0001-01-01).
	var bodyOut webhookPayload
	if err := json.Unmarshal(gotBody, &bodyOut); err != nil {
		t.Fatalf("webhook body is not valid JSON: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339, bodyOut.OccurredAt)
	if err != nil {
		t.Fatalf("occurred_at = %q is not RFC3339: %v", bodyOut.OccurredAt, err)
	}
	if parsed.IsZero() {
		t.Fatalf("occurred_at is zero in webhook body (0001-01-01 bug): %q", bodyOut.OccurredAt)
	}
	if !parsed.Equal(occurred) {
		t.Errorf("occurred_at = %v, want %v", parsed, occurred)
	}
	// Signature is valid over the actual received body.
	mac := hmac.New(sha256.New, []byte("sign-key"))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("signature = %q, want %q", gotSig, want)
	}
	terms := rec.terminals()
	if len(terms) != 1 || terms[0].EventType != audit.EventHeraldDelivered {
		t.Fatalf("want one herald.delivered, got %+v", terms)
	}
	if backend.acks != 1 {
		t.Errorf("acks = %d, want 1", backend.acks)
	}
}

// TestDelivery_AnnotationsProjection_SignedFinalBody — end-to-end (ADR-052(h)/(i)
// N3): httptest receiver gets body with payload narrowed by projection +
// top-level annotations key, and HMAC signature is valid over THIS final
// body (not over original full payload).
func TestDelivery_AnnotationsProjection_SignedFinalBody(t *testing.T) {
	var gotBody []byte
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(SignatureHeader)
		rw.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	backend := newFakeBackend()
	rec := &recordingAudit{}
	secretRef := "vault:secret/keeper/sign#token"
	h := &Herald{
		Name: "shaped-webhook", Type: HeraldWebhook,
		Config:    map[string]any{"url": srv.URL, "allow_private": true, "http_allowed": true},
		SecretRef: &secretRef, Enabled: true,
	}
	w := &DeliveryWorker{
		Queue: backend, Heralds: recordingHeralds{herald: h},
		KV:    stubKV{data: map[string]any{"token": "sign-key"}},
		Audit: rec, Logger: discardLogger(), Resolver: netguard.DefaultResolver,
	}

	job := &DeliveryJob{
		ID: "j-shaped", Herald: "shaped-webhook", Tiding: "t",
		EventType:  audit.EventScenarioRunFailed,
		OccurredAt: time.Date(2026, 6, 11, 10, 30, 0, 0, time.UTC),
		PayloadCopy: map[string]any{
			"voyage_id": "v1",
			"summary":   map[string]any{"succeeded": float64(7), "failed": float64(1)},
			"drop":      "should-disappear",
		},
		Projection:  []string{"voyage_id", "summary.failed"},
		Annotations: map[string]any{"team": "ops", "severity": "high"},
	}
	payload, _ := marshalJob(job)
	_ = backend.Enqueue(context.Background(), payload)

	runWorkerOnce(t, w)

	if len(gotBody) == 0 {
		t.Fatal("webhook receiver got no body")
	}
	var body webhookPayload
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("webhook body is not parseable: %v", err)
	}
	// Projection: payload narrowed (voyage_id + summary.failed), original drop removed.
	if body.Payload["voyage_id"] != "v1" {
		t.Errorf("payload.voyage_id = %v", body.Payload["voyage_id"])
	}
	if _, present := body.Payload["drop"]; present {
		t.Errorf("dropped field leaked through projection allow-list")
	}
	summary, ok := body.Payload["summary"].(map[string]any)
	if !ok || summary["failed"] != float64(1) {
		t.Fatalf("projected summary.failed missing: %v", body.Payload)
	}
	if _, present := summary["succeeded"]; present {
		t.Errorf("summary.succeeded leaked (not in projection)")
	}
	// Annotations are top-level.
	if body.Annotations["team"] != "ops" || body.Annotations["severity"] != "high" {
		t.Errorf("annotations = %v", body.Annotations)
	}
	// Signature is valid over the actual (final) received body.
	mac := hmac.New(sha256.New, []byte("sign-key"))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("signature = %q, want %q (must sign final shaped body)", gotSig, want)
	}
	terms := rec.terminals()
	if len(terms) != 1 || terms[0].EventType != audit.EventHeraldDelivered {
		t.Fatalf("want one herald.delivered, got %+v", terms)
	}
}

// TestDelivery_5xx_Retries verifies receiver returning 500 means delivery does not
// finalize successfully; job is requeued for retry with incremented attempt.
func TestDelivery_5xx_Retries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	backend := newFakeBackend()
	rec := &recordingAudit{}
	h := &Herald{Name: "flaky", Type: HeraldWebhook, Config: map[string]any{"url": srv.URL, "allow_private": true, "http_allowed": true}, Enabled: true}
	w := &DeliveryWorker{Queue: backend, Heralds: recordingHeralds{herald: h}, Audit: rec, Logger: discardLogger()}

	job := &DeliveryJob{ID: "j-flaky", Attempt: 0, Herald: "flaky", EventType: audit.EventScenarioRunFailed}
	payload, _ := marshalJob(job)
	_ = backend.Enqueue(context.Background(), payload)

	runWorkerOnce(t, w)

	if hits.Load() != 1 {
		t.Fatalf("receiver hits = %d, want 1", hits.Load())
	}
	if backend.requeues != 1 {
		t.Fatalf("5xx must requeue, requeues=%d", backend.requeues)
	}
	pj := backend.pendingJobs(t)
	if len(pj) != 1 || pj[0].Attempt != 1 {
		t.Fatalf("requeued attempt = %v, want 1", pj)
	}
	if len(rec.terminals()) != 0 {
		t.Fatalf("5xx on non-final attempt must not write terminal audit yet")
	}
}

// TestDelivery_4xx_TerminalNoRetry verifies receiver returning 401 (stable client error)
// means terminal herald.failed immediately, no retry, even on first attempt.
func TestDelivery_4xx_TerminalNoRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		rw.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	backend := newFakeBackend()
	rec := &recordingAudit{}
	h := &Herald{Name: "authfail", Type: HeraldWebhook, Config: map[string]any{"url": srv.URL, "allow_private": true, "http_allowed": true}, Enabled: true}
	w := &DeliveryWorker{Queue: backend, Heralds: recordingHeralds{herald: h}, Audit: rec, Logger: discardLogger()}

	job := &DeliveryJob{ID: "j-401", Attempt: 0, Herald: "authfail", EventType: audit.EventScenarioRunFailed}
	payload, _ := marshalJob(job)
	_ = backend.Enqueue(context.Background(), payload)

	runWorkerOnce(t, w)

	if hits.Load() != 1 {
		t.Fatalf("receiver hits = %d, want 1", hits.Load())
	}
	if backend.requeues != 0 {
		t.Fatalf("4xx (non-408/429) is terminal, must not retry, requeues=%d", backend.requeues)
	}
	terms := rec.terminals()
	if len(terms) != 1 || terms[0].EventType != audit.EventHeraldFailed {
		t.Fatalf("4xx must write one herald.failed terminal, got %+v", terms)
	}
}

// TestDelivery_429_Retries verifies 429 Too Many Requests is transient rate-limit:
// retried (exception to "4xx→terminal"), job requeued.
func TestDelivery_429_Retries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		rw.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	backend := newFakeBackend()
	rec := &recordingAudit{}
	h := &Herald{Name: "throttled", Type: HeraldWebhook, Config: map[string]any{"url": srv.URL, "allow_private": true, "http_allowed": true}, Enabled: true}
	w := &DeliveryWorker{Queue: backend, Heralds: recordingHeralds{herald: h}, Audit: rec, Logger: discardLogger()}

	job := &DeliveryJob{ID: "j-429", Attempt: 0, Herald: "throttled", EventType: audit.EventScenarioRunFailed}
	payload, _ := marshalJob(job)
	_ = backend.Enqueue(context.Background(), payload)

	runWorkerOnce(t, w)

	if backend.requeues != 1 {
		t.Fatalf("429 must requeue (transient), requeues=%d", backend.requeues)
	}
	if len(rec.terminals()) != 0 {
		t.Fatalf("429 on non-final attempt must not write terminal audit yet")
	}
}

// TestDelivery_Timeout_Retries verifies receiver hanging longer than delivery timeout
// means delivery times out → retry.
func TestDelivery_Timeout_Retries(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		<-release // hold until end of test
		rw.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	backend := newFakeBackend()
	h := &Herald{Name: "slow", Type: HeraldWebhook, Config: map[string]any{"url": srv.URL, "allow_private": true, "http_allowed": true}, Enabled: true}
	w := &DeliveryWorker{
		Queue: backend, Heralds: recordingHeralds{herald: h},
		Logger: discardLogger(), Timeout: 150 * time.Millisecond,
	}

	job := &DeliveryJob{ID: "j-slow", Attempt: 0, Herald: "slow", EventType: audit.EventScenarioRunFailed}
	payload, _ := marshalJob(job)
	_ = backend.Enqueue(context.Background(), payload)

	start := time.Now()
	runWorkerOnce(t, w)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("delivery did not honour timeout (took %v)", elapsed)
	}
	if backend.requeues != 1 {
		t.Fatalf("timeout must requeue, requeues=%d", backend.requeues)
	}
}

// TestDelivery_SSRF_PrivateIPRejectedBeforeRequest verifies webhook to private-IP
// without allow_private is rejected by SSRF guard before HTTP request (terminal,
// no retry, receiver not called).
func TestDelivery_SSRF_PrivateIPRejectedBeforeRequest(t *testing.T) {
	var hits atomic.Int32
	// Spin up real server but swap URL to private-IP — guard must cut off before
	// connection, so server never receives request.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		rw.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	backend := newFakeBackend()
	rec := &recordingAudit{}
	// Literal private-IP, allow_private not set → guard rejects.
	h := &Herald{Name: "ssrf", Type: HeraldWebhook, Config: map[string]any{"url": "https://10.0.0.1/hook"}, Enabled: true}
	w := &DeliveryWorker{Queue: backend, Heralds: recordingHeralds{herald: h}, Audit: rec, Logger: discardLogger()}

	job := &DeliveryJob{ID: "j-ssrf", Attempt: 0, Herald: "ssrf", EventType: audit.EventScenarioRunFailed}
	payload, _ := marshalJob(job)
	_ = backend.Enqueue(context.Background(), payload)

	runWorkerOnce(t, w)

	if hits.Load() != 0 {
		t.Fatalf("SSRF target must not be contacted, hits=%d", hits.Load())
	}
	if backend.requeues != 0 {
		t.Fatalf("SSRF rejection is terminal (no retry), requeues=%d", backend.requeues)
	}
	terms := rec.terminals()
	if len(terms) != 1 || terms[0].EventType != audit.EventHeraldFailed {
		t.Fatalf("want one herald.failed terminal, got %+v", terms)
	}
}

// TestDelivery_SSRF_DNSResolvedToPrivate_RejectedAtDial verifies host resolving to
// private-IP (via injected resolver) means rejection at dial phase, receiver not called.
// Verifies dial-guard works not only on literal IP.
func TestDelivery_SSRF_DNSResolvedToPrivate_RejectedAtDial(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		rw.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	backend := newFakeBackend()
	rec := &recordingAudit{}
	h := &Herald{Name: "rebind", Type: HeraldWebhook, Config: map[string]any{"url": "https://evil.example.test/hook"}, Enabled: true}
	w := &DeliveryWorker{
		Queue: backend, Heralds: recordingHeralds{herald: h},
		Audit: rec, Logger: discardLogger(),
		Resolver: privateResolver{}, // name resolves to 10.x
	}

	job := &DeliveryJob{ID: "j-rebind", Attempt: 0, Herald: "rebind", EventType: audit.EventScenarioRunFailed}
	payload, _ := marshalJob(job)
	_ = backend.Enqueue(context.Background(), payload)

	runWorkerOnce(t, w)

	if hits.Load() != 0 {
		t.Fatalf("rebind target must not be contacted, hits=%d", hits.Load())
	}
	// DNS resolve to private looks like transient error (dial), so it retries,
	// not terminal-no-retry: guard rejected at dial, not pre-validation.
	if backend.requeues != 1 {
		t.Fatalf("dial-guard rejection retries, requeues=%d", backend.requeues)
	}
}
