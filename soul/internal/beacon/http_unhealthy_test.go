package beacon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// fakeDoer is a deterministic HTTPDoer: returns a given status (beacon
// doesn't read the stub body), or a transport error. No network.
type fakeDoer struct {
	status int
	err    error
}

func (d *fakeDoer) Do(*http.Request) (*http.Response, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &http.Response{
		StatusCode: d.status,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

func newHTTPUnhealthy(d *fakeDoer) *HTTPUnhealthy {
	return &HTTPUnhealthy{NewClient: func(util.HTTPClientOpts) util.HTTPDoer { return d }}
}

// newHTTPUnhealthyCapturing uses the production util.NewHTTPClient factory
// (real dial / TLS), but captures the passed opts — a regression guard for
// param→HTTPClientOpts mapping on the production client-build path.
func newHTTPUnhealthyCapturing(got *util.HTTPClientOpts) *HTTPUnhealthy {
	return &HTTPUnhealthy{NewClient: func(opts util.HTTPClientOpts) util.HTTPDoer {
		*got = opts
		return util.NewHTTPClient(opts)
	}}
}

func TestHTTPUnhealthyHealthy(t *testing.T) {
	b := newHTTPUnhealthy(&fakeDoer{status: 200})
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"url": "https://service.internal/healthz",
	}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateHTTPHealthy {
		t.Fatalf("state = %q, want healthy", state)
	}
	if int(data.GetFields()["status"].GetNumberValue()) != 200 {
		t.Error("data.status should carry the status code")
	}
	if _, hasBody := data.GetFields()["body"]; hasBody {
		t.Error("data should NOT carry the response body (sensitive)")
	}
}

func TestHTTPUnhealthyBadStatus(t *testing.T) {
	b := newHTTPUnhealthy(&fakeDoer{status: 503})
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"url": "https://service.internal/healthz",
	}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateHTTPUnhealthy {
		t.Fatalf("state = %q, want unhealthy (503 outside [200])", state)
	}
	if int(data.GetFields()["status"].GetNumberValue()) != 503 {
		t.Error("data.status should carry the actual code 503")
	}
}

func TestHTTPUnhealthyCustomStatusCodes(t *testing.T) {
	// 204 is healthy given status_codes [200,204]; with the default [200] it'd be unhealthy.
	b := newHTTPUnhealthy(&fakeDoer{status: 204})
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"url":          "https://service.internal/ping",
		"status_codes": []any{200, 204},
	}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateHTTPHealthy {
		t.Fatalf("state = %q, want healthy (204 ∈ [200,204])", state)
	}
}

func TestHTTPUnhealthyTransportError(t *testing.T) {
	// Transport error → unhealthy (status 0), not a Check error.
	b := newHTTPUnhealthy(&fakeDoer{err: errors.New("connection refused")})
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"url": "https://down.internal/healthz",
	}))
	if err != nil {
		t.Fatalf("Check should not return an error on a transport error: %v", err)
	}
	if state != stateHTTPUnhealthy {
		t.Fatalf("state = %q, want unhealthy", state)
	}
	if int(data.GetFields()["status"].GetNumberValue()) != 0 {
		t.Error("data.status should be 0 on a transport error")
	}
}

func TestHTTPUnhealthyRejectsHTTP(t *testing.T) {
	// https-only is reused from core.http: http:// is rejected at Check.
	b := newHTTPUnhealthy(&fakeDoer{status: 200})
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"url": "http://service.internal/healthz",
	})); err == nil {
		t.Fatal("expected an error for http:// (https-only)")
	}
}

func TestHTTPUnhealthyMissingURL(t *testing.T) {
	b := newHTTPUnhealthy(&fakeDoer{status: 200})
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("expected an error when the url param is missing")
	}
}

// --- opt-out flags (core.http pattern): default secure, explicit opt-out lowers the contour ---

// allow_http:true → http:// is accepted at Check (ValidateFetchURL lets it
// through), and the opt reaches the factory as AllowHTTPRedirect (parity
// with the downgrade hop). Dial is hermetic (fakeDoer) — we check scheme
// validation and param→opts mapping, not a real http dial.
func TestHTTPUnhealthyAllowHTTP(t *testing.T) {
	var got util.HTTPClientOpts
	b := &HTTPUnhealthy{NewClient: func(opts util.HTTPClientOpts) util.HTTPDoer {
		got = opts
		return &fakeDoer{status: 200}
	}}
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"url":        "http://service.internal/healthz",
		"allow_http": true,
	}))
	if err != nil {
		t.Fatalf("Check with allow_http:true should not fail on http://: %v", err)
	}
	if state != stateHTTPHealthy {
		t.Fatalf("state = %q, want healthy", state)
	}
	if !got.AllowHTTPRedirect {
		t.Fatal("allow_http did not reach the factory as AllowHTTPRedirect")
	}
	if got.AllowPrivate || got.InsecureSkipVerify {
		t.Fatalf("allow_http affected an unrelated circuit: %+v", got)
	}
}

// allow_private:true → a real dial to a loopback server (127.0.0.1) passes
// the SSRF guard → healthy. Without the flag, the same loopback is blocked
// at the dial phase.
func TestHTTPUnhealthyAllowPrivateLoopback(t *testing.T) {
	// httptest.NewTLSServer listens on 127.0.0.1 with a self-signed cert —
	// needs both allow_private (loopback) and insecure_skip_verify (self-signed).
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Run("allow_private+insecure -> dial passes -> healthy", func(t *testing.T) {
		var got util.HTTPClientOpts
		b := newHTTPUnhealthyCapturing(&got)
		state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
			"url":                  srv.URL + "/health",
			"allow_private":        true,
			"insecure_skip_verify": true,
		}))
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if state != stateHTTPHealthy {
			t.Fatalf("state = %q, want healthy (loopback with allow_private)", state)
		}
		if int(data.GetFields()["status"].GetNumberValue()) != 200 {
			t.Errorf("status = %v, want 200", data.GetFields()["status"].GetNumberValue())
		}
		if !got.AllowPrivate || !got.InsecureSkipVerify {
			t.Fatalf("opts did not reach the factory: %+v", got)
		}
	})

	t.Run("default -> SSRF-guard blocks loopback -> unhealthy", func(t *testing.T) {
		// Without allow_private, dialing 127.0.0.1 is rejected by netguard →
		// transport error → unhealthy (status 0), not a Check error.
		b := NewHTTPUnhealthy() // production factory, zero opts
		state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
			"url":                  srv.URL + "/health",
			"insecure_skip_verify": true, // isolate the SSRF contour specifically, not TLS
		}))
		if err != nil {
			t.Fatalf("Check should not fail when dial is blocked: %v", err)
		}
		if state != stateHTTPUnhealthy {
			t.Fatalf("state = %q, want unhealthy (loopback without allow_private)", state)
		}
		if int(data.GetFields()["status"].GetNumberValue()) != 0 {
			t.Error("status should be 0 when dial is blocked")
		}
	})
}

// insecure_skip_verify:true → self-signed TLS server accepted (healthy).
// Without the flag, the same cert fails verification → transport error →
// unhealthy. Factory here is production (util.NewHTTPClient) — checking the
// real TLS contour.
func TestHTTPUnhealthyInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Run("insecure_skip_verify:true -> self-signed accepted -> healthy", func(t *testing.T) {
		b := NewHTTPUnhealthy()
		state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
			"url":                  srv.URL + "/health",
			"allow_private":        true, // loopback
			"insecure_skip_verify": true,
		}))
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if state != stateHTTPHealthy {
			t.Fatalf("state = %q, want healthy (self-signed with insecure_skip_verify)", state)
		}
	})

	t.Run("default -> self-signed is not trusted -> unhealthy", func(t *testing.T) {
		b := NewHTTPUnhealthy()
		state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
			"url":           srv.URL + "/health",
			"allow_private": true, // loopback allowed through, isolating the TLS contour
		}))
		if err != nil {
			t.Fatalf("Check should not fail on invalid TLS: %v", err)
		}
		if state != stateHTTPUnhealthy {
			t.Fatalf("state = %q, want unhealthy (self-signed without insecure_skip_verify)", state)
		}
	})
}

// Default (no opt-out flags) → zero HTTPClientOpts (secure-by-default).
func TestHTTPUnhealthyDefaultSecure(t *testing.T) {
	var got util.HTTPClientOpts
	b := &HTTPUnhealthy{NewClient: func(opts util.HTTPClientOpts) util.HTTPDoer {
		got = opts
		return &fakeDoer{status: 200}
	}}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"url": "https://service.internal/healthz",
	})); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.AllowPrivate || got.InsecureSkipVerify || got.AllowHTTPRedirect {
		t.Fatalf("default is not secure-by-default: %+v", got)
	}
}

// Invalid opt-out flag type (string instead of bool) → Check error.
func TestHTTPUnhealthyRejectsNonBoolFlag(t *testing.T) {
	for _, flag := range []string{"allow_http", "insecure_skip_verify", "allow_private"} {
		b := newHTTPUnhealthy(&fakeDoer{status: 200})
		if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
			"url": "https://service.internal/healthz",
			flag:  "yes",
		})); err == nil {
			t.Fatalf("expected an error for %s as a string (type check)", flag)
		}
	}
}
