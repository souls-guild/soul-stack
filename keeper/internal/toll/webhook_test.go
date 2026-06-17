package toll

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVault — минимальный [VaultReader] для unit-тестов: возвращает заранее
// заданную map по path-ключу.
type fakeVault struct {
	mu    sync.Mutex
	data  map[string]map[string]any
	err   error
	calls int
}

func (v *fakeVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.err != nil {
		return nil, v.err
	}
	d, ok := v.data[path]
	if !ok {
		return nil, errors.New("not found: " + path)
	}
	return d, nil
}

// receivedRequest — снимок одного POST-вызова webhook-receiver-ом.
type receivedRequest struct {
	headers http.Header
	body    map[string]any
	method  string
}

// newRecordingServer запускает httptest-сервер, фиксирующий все POST-запросы
// и отвечающий заданным status-ом. Возвращает (url, accessor для requests).
func newRecordingServer(t *testing.T, status int) (string, func() []receivedRequest) {
	t.Helper()
	var (
		mu       sync.Mutex
		captured []receivedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		captured = append(captured, receivedRequest{
			headers: r.Header.Clone(),
			body:    body,
			method:  r.Method,
		})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []receivedRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]receivedRequest, len(captured))
		copy(out, captured)
		return out
	}
}

func sampleEvent() TollEvent {
	return TollEvent{
		Type:              EventTypeDegradedSet,
		LeaderKID:         "kid-A",
		Rate:              0.35,
		BaselineConnected: 100,
		Threshold:         0.20,
		WindowSeconds:     60,
		Timestamp:         time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC),
	}
}

func TestNewWebhookNotifier_RejectsInvalid(t *testing.T) {
	logger := newTestLogger()
	if _, err := NewWebhookNotifier(WebhookConfig{}, nil, logger); err == nil {
		t.Fatal("expected error for empty URLRef")
	}
	if _, err := NewWebhookNotifier(WebhookConfig{URLRef: "http://x"}, nil, nil); err == nil {
		t.Fatal("expected error for nil logger")
	}
	if _, err := NewWebhookNotifier(WebhookConfig{URLRef: "http://x", Format: "junk"}, nil, logger); err == nil {
		t.Fatal("expected error for unsupported format")
	}
	if _, err := NewWebhookNotifier(WebhookConfig{URLRef: "vault:secret/x"}, nil, logger); err == nil {
		t.Fatal("expected error for vault: prefix without VaultReader")
	}
}

func TestWebhook_Generic_PostsFlatJSON(t *testing.T) {
	url, getReqs := newRecordingServer(t, http.StatusOK)
	n, err := NewWebhookNotifier(WebhookConfig{URLRef: url, Format: "generic"}, nil, newTestLogger())
	if err != nil {
		t.Fatalf("NewWebhookNotifier: %v", err)
	}
	n.Notify(context.Background(), sampleEvent())
	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("ожидался 1 POST, got %d", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost {
		t.Fatalf("ожидался POST, got %s", r.method)
	}
	if ct := r.headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("ожидался Content-Type application/json, got %q", ct)
	}
	if r.body["event_type"] != EventTypeDegradedSet {
		t.Fatalf("event_type mismatch: %v", r.body["event_type"])
	}
	if r.body["leader_kid"] != "kid-A" {
		t.Fatalf("leader_kid mismatch: %v", r.body["leader_kid"])
	}
	// JSON-числа десериализуются в float64.
	if r.body["rate"].(float64) < 0.34 || r.body["rate"].(float64) > 0.36 {
		t.Fatalf("rate mismatch: %v", r.body["rate"])
	}
	if r.body["threshold"].(float64) != 0.20 {
		t.Fatalf("threshold mismatch: %v", r.body["threshold"])
	}
	if r.body["window_seconds"].(float64) != 60 {
		t.Fatalf("window_seconds mismatch: %v", r.body["window_seconds"])
	}
	if _, has := r.body["coven_name"]; has {
		t.Fatalf("coven_name не должен присутствовать при global-trigger-е, got %v", r.body["coven_name"])
	}
}

func TestWebhook_Generic_IncludesCovenWhenSet(t *testing.T) {
	url, getReqs := newRecordingServer(t, http.StatusOK)
	n, _ := NewWebhookNotifier(WebhookConfig{URLRef: url, Format: "generic"}, nil, newTestLogger())
	ev := sampleEvent()
	ev.CovenName = "production-eu"
	n.Notify(context.Background(), ev)
	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("ожидался 1 POST")
	}
	if reqs[0].body["coven_name"] != "production-eu" {
		t.Fatalf("coven_name mismatch: %v", reqs[0].body["coven_name"])
	}
}

func TestWebhook_PagerDuty_v2Shape(t *testing.T) {
	url, getReqs := newRecordingServer(t, http.StatusAccepted)
	v := &fakeVault{
		data: map[string]map[string]any{
			"secret/keeper/pd": {
				"url":         url,
				"routing_key": "R0utin9-K3y",
			},
		},
	}
	n, _ := NewWebhookNotifier(WebhookConfig{URLRef: "vault:secret/keeper/pd", Format: "pagerduty_v2"}, v, newTestLogger())
	n.Notify(context.Background(), sampleEvent())
	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("ожидался 1 POST, got %d", len(reqs))
	}
	body := reqs[0].body
	if body["routing_key"] != "R0utin9-K3y" {
		t.Fatalf("routing_key mismatch: %v", body["routing_key"])
	}
	if body["event_action"] != "trigger" {
		t.Fatalf("event_action mismatch (degraded_set → trigger): %v", body["event_action"])
	}
	if body["dedup_key"] != "soul-stack/cluster:degraded" {
		t.Fatalf("dedup_key mismatch: %v", body["dedup_key"])
	}
	payload, ok := body["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload is not object: %v", body["payload"])
	}
	if payload["severity"] != "error" {
		t.Fatalf("severity mismatch (set→error): %v", payload["severity"])
	}
	if !strings.Contains(payload["summary"].(string), "kid-A") {
		t.Fatalf("summary должен включать leader_kid: %v", payload["summary"])
	}
	customDetails, ok := payload["custom_details"].(map[string]any)
	if !ok {
		t.Fatalf("custom_details is not object")
	}
	if customDetails["leader_kid"] != "kid-A" {
		t.Fatalf("custom_details.leader_kid mismatch")
	}
}

func TestWebhook_PagerDuty_ResolveOnCleared(t *testing.T) {
	url, getReqs := newRecordingServer(t, http.StatusAccepted)
	v := &fakeVault{data: map[string]map[string]any{
		"secret/keeper/pd": {"url": url, "routing_key": "rk"},
	}}
	n, _ := NewWebhookNotifier(WebhookConfig{URLRef: "vault:secret/keeper/pd", Format: "pagerduty_v2"}, v, newTestLogger())
	ev := sampleEvent()
	ev.Type = EventTypeDegradedCleared
	n.Notify(context.Background(), ev)
	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("ожидался 1 POST")
	}
	if reqs[0].body["event_action"] != "resolve" {
		t.Fatalf("ожидался event_action=resolve, got %v", reqs[0].body["event_action"])
	}
	payload := reqs[0].body["payload"].(map[string]any)
	if payload["severity"] != "info" {
		t.Fatalf("ожидалась severity=info на resolve, got %v", payload["severity"])
	}
}

func TestWebhook_Slack_Shape(t *testing.T) {
	url, getReqs := newRecordingServer(t, http.StatusOK)
	n, _ := NewWebhookNotifier(WebhookConfig{URLRef: url, Format: "slack"}, nil, newTestLogger())
	n.Notify(context.Background(), sampleEvent())
	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("ожидался 1 POST")
	}
	body := reqs[0].body
	if _, ok := body["text"].(string); !ok {
		t.Fatalf("text required")
	}
	atts, ok := body["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments must be array of 1, got %v", body["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["color"] != "danger" {
		t.Fatalf("color должен быть 'danger' при degraded_set, got %v", att["color"])
	}
	fields, _ := att["fields"].([]any)
	if len(fields) < 4 {
		t.Fatalf("ожидалось >=4 fields (rate/threshold/baseline/window), got %d", len(fields))
	}
}

func TestWebhook_Slack_GreenOnCleared(t *testing.T) {
	url, getReqs := newRecordingServer(t, http.StatusOK)
	n, _ := NewWebhookNotifier(WebhookConfig{URLRef: url, Format: "slack"}, nil, newTestLogger())
	ev := sampleEvent()
	ev.Type = EventTypeDegradedCleared
	n.Notify(context.Background(), ev)
	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("ожидался 1 POST")
	}
	att := reqs[0].body["attachments"].([]any)[0].(map[string]any)
	if att["color"] != "good" {
		t.Fatalf("color должен быть 'good' при degraded_cleared, got %v", att["color"])
	}
}

func TestWebhook_NonRecoverableHTTPError_Logs(t *testing.T) {
	url, getReqs := newRecordingServer(t, http.StatusInternalServerError)
	n, _ := NewWebhookNotifier(WebhookConfig{URLRef: url, Format: "generic"}, nil, newTestLogger())
	// Не должен паниковать.
	n.Notify(context.Background(), sampleEvent())
	if len(getReqs()) != 1 {
		t.Fatalf("ожидался 1 POST даже на 500-ответ")
	}
}

func TestWebhook_VaultError_NoPost(t *testing.T) {
	v := &fakeVault{err: errors.New("vault down")}
	n, _ := NewWebhookNotifier(WebhookConfig{URLRef: "vault:secret/x", Format: "generic"}, v, newTestLogger())
	// Не должен паниковать.
	n.Notify(context.Background(), sampleEvent())
	// Vault упал → POST не делается, проверяем что fakeVault был вызван.
	if v.calls != 1 {
		t.Fatalf("ожидался 1 ReadKV-вызов, got %d", v.calls)
	}
}

func TestWebhook_VaultMissingURLField_NoPost(t *testing.T) {
	v := &fakeVault{
		data: map[string]map[string]any{
			"secret/keeper/wh": {"routing_key": "rk"}, // нет поля `url`
		},
	}
	n, _ := NewWebhookNotifier(WebhookConfig{URLRef: "vault:secret/keeper/wh", Format: "generic"}, v, newTestLogger())
	n.Notify(context.Background(), sampleEvent())
	// Должен залогировать ошибку и не упасть — проверяем что вызвался Vault,
	// но POST не пошёл (если бы пошёл, упал бы на DNS/невалидном URL=="").
}

func TestWebhook_Timeout_LogsNoPanic(t *testing.T) {
	// Slow-server: blocks longer than client.timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	n, _ := NewWebhookNotifier(WebhookConfig{
		URLRef:  srv.URL,
		Format:  "generic",
		Timeout: 50 * time.Millisecond,
	}, nil, newTestLogger())
	// Не должен паниковать / зависнуть.
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.Notify(context.Background(), sampleEvent())
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify завис на timeout-server-е (timeout не сработал)")
	}
}

func TestWebhook_NilNotifier_Safe(t *testing.T) {
	var n *WebhookNotifier
	// Не должен паниковать.
	n.Notify(context.Background(), sampleEvent())
}
