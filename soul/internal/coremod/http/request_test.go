package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	httpmod "github.com/souls-guild/soul-stack/soul/internal/coremod/http"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

func TestValidate_RequestMethodBoundary(t *testing.T) {
	m := httpmod.New()
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "post"} {
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "request",
			Params: mustStruct(t, map[string]any{
				"url":    "https://example.com/v1/resource",
				"method": method,
			}),
		})
		if !reply.Ok {
			t.Errorf("method %q rejected: %v", method, reply.Errors)
		}
	}

	for _, method := range []string{"", "GET", "HEAD", "OPTIONS"} {
		params := map[string]any{"url": "https://example.com/v1/resource"}
		if method != "" {
			params["method"] = method
		}
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "request", Params: mustStruct(t, params),
		})
		if reply.Ok {
			t.Errorf("method %q accepted by mutating request", method)
		}
	}
}

func TestApply_RequestMethods_SendBodyAndReportChanged(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			const token = "consul-token-must-not-leak"
			d := &fakeDoer{body: []byte(`{"accepted":true}`), status: 200}
			m := newModule(d)
			stream := &internaltest.ApplyStream{}
			err := m.Apply(&pluginv1.ApplyRequest{
				State: "request",
				Params: mustStruct(t, map[string]any{
					"url":          "https://example.com/v1/resource",
					"method":       method,
					"body":         `{"service":"redis"}`,
					"content_type": "application/json",
					"headers": map[string]any{
						"X-Consul-Token": token,
					},
				}),
			}, stream)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			ev := stream.Last()
			if ev.Failed || !ev.Changed || !ev.Output.Fields["changed"].GetBoolValue() {
				t.Fatalf("result failed=%v changed=%v output=%v", ev.Failed, ev.Changed, ev.Output)
			}
			if d.calls != 1 {
				t.Fatalf("HTTP calls=%d, want exactly 1 (retry belongs to scenario)", d.calls)
			}
			if d.gotMethod != method || d.gotBody != `{"service":"redis"}` {
				t.Fatalf("request = %s body %q", d.gotMethod, d.gotBody)
			}
			if got := d.gotHeaders.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type=%q", got)
			}
			if got := d.gotHeaders.Get("X-Consul-Token"); got != token {
				t.Errorf("X-Consul-Token was not sent: %q", got)
			}
			encoded, _ := json.Marshal(ev)
			if strings.Contains(string(encoded), token) {
				t.Fatalf("X-Consul-Token value leaked into ApplyEvent: %s", encoded)
			}
			keys := ev.Output.Fields["headers_keys"].GetListValue()
			if keys == nil || len(keys.Values) != 2 ||
				keys.Values[0].GetStringValue() != "Content-Type" ||
				keys.Values[1].GetStringValue() != "X-Consul-Token" {
				t.Fatalf("headers_keys=%v, want [Content-Type X-Consul-Token]", keys)
			}
		})
	}
}

// A single Module.Apply must mean one mutating wire request, not merely one
// call to http.Client.Do. In particular, net/http must not replay PUT+body on
// a 307/308 redirect.
func TestApply_RequestDoesNotFollowRedirect(t *testing.T) {
	var firstCalls, targetCalls int
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		switch r.URL.Path {
		case "/first":
			firstCalls++
			w.Header().Set("Location", "/target")
			w.WriteHeader(stdhttp.StatusTemporaryRedirect)
		case "/target":
			targetCalls++
			w.WriteHeader(stdhttp.StatusOK)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(stdhttp.StatusNotFound)
		}
	}))
	defer server.Close()

	stream := &internaltest.ApplyStream{}
	if err := httpmod.New().Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url":           server.URL + "/first",
			"method":        "PUT",
			"body":          "one-mutation",
			"status_codes":  []any{stdhttp.StatusTemporaryRedirect},
			"allow_http":    true,
			"allow_private": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if firstCalls != 1 || targetCalls != 0 {
		t.Fatalf("wire calls: first=%d target=%d, want first=1 target=0", firstCalls, targetCalls)
	}
	if ev := stream.Last(); ev.Failed || !ev.Changed {
		t.Fatalf("first 307 response failed=%v changed=%v message=%q", ev.Failed, ev.Changed, ev.Message)
	}
	if keys := stream.Last().Output.Fields["headers_keys"].GetListValue(); keys == nil || len(keys.Values) != 0 {
		t.Fatalf("request headers_keys=%v, want present empty list", keys)
	}
}

func TestApply_RequestRedactsEchoedHeaderValueFromOutput(t *testing.T) {
	const token = "consul-token-must-never-leak"
	d := &fakeDoer{status: 200, body: []byte(`{"echo":"` + token + `"}`)}
	stream := &internaltest.ApplyStream{}
	_ = newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url":     "https://example.com/v1/resource",
			"method":  "POST",
			"headers": map[string]any{"X-Consul-Token": token},
		}),
	}, stream)

	encoded, err := json.Marshal(stream.Last())
	if err != nil {
		t.Fatalf("marshal ApplyEvent: %v", err)
	}
	if strings.Contains(string(encoded), token) {
		t.Fatalf("echoed header value leaked into output/register/audit source: %s", encoded)
	}
	if body := stream.Last().Output.Fields["body"].GetStringValue(); !strings.Contains(body, "***MASKED***") {
		t.Fatalf("response body=%q, want header-value mask", body)
	}
}

func TestApply_RequestRedactsHeaderValueFromTransportDiagnostic(t *testing.T) {
	const token = "transport-echoed-consul-token"
	d := &fakeDoer{err: errors.New("upstream transport echoed " + token)}
	stream := &internaltest.ApplyStream{}
	_ = newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url":     "https://example.com/v1/resource",
			"method":  "DELETE",
			"headers": map[string]any{"X-Consul-Token": token},
		}),
	}, stream)

	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("transport failure did not fail request")
	}
	if strings.Contains(ev.Message, token) || !strings.Contains(ev.Message, "***MASKED***") {
		t.Fatalf("transport diagnostic was not redacted: %q", ev.Message)
	}
}

func TestApply_RequestContentTypeParamWins(t *testing.T) {
	d := &fakeDoer{status: 200}
	stream := &internaltest.ApplyStream{}
	_ = newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url":          "https://example.com/v1/resource",
			"method":       "POST",
			"content_type": "application/json",
			"headers":      map[string]any{"Content-Type": "text/plain"},
		}),
	}, stream)
	if got := d.gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q, want dedicated content_type to win", got)
	}
	keys := stream.Last().Output.Fields["headers_keys"].GetListValue()
	if keys == nil || len(keys.Values) != 1 || keys.Values[0].GetStringValue() != "Content-Type" {
		t.Fatalf("headers_keys=%v, want one de-duplicated Content-Type", keys)
	}
}

func TestApply_RequestStatusMismatchFailsOnceWithDiagnostics(t *testing.T) {
	d := &fakeDoer{status: 503, body: []byte("unavailable")}
	stream := &internaltest.ApplyStream{}
	_ = newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url":          "https://example.com/v1/resource",
			"method":       "PUT",
			"status_codes": []any{200, 204},
		}),
	}, stream)
	ev := stream.Last()
	if !ev.Failed || ev.Changed || ev.Output == nil {
		t.Fatalf("mismatch result failed=%v changed=%v output=%v", ev.Failed, ev.Changed, ev.Output)
	}
	if d.calls != 1 {
		t.Fatalf("HTTP calls=%d, want 1", d.calls)
	}
	if ev.Output.Fields["status"].GetNumberValue() != 503 ||
		ev.Output.Fields["body"].GetStringValue() != "unavailable" ||
		ev.Output.Fields["changed"].GetBoolValue() {
		t.Fatalf("diagnostic output=%v", ev.Output)
	}
}

func TestApply_RequestResponseBodyCap(t *testing.T) {
	d := &fakeDoer{status: 200, body: []byte(strings.Repeat("x", 70*1024))}
	stream := &internaltest.ApplyStream{}
	_ = newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url": "https://example.com/v1/resource", "method": "DELETE",
		}),
	}, stream)
	ev := stream.Last()
	if !ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=false for request response >64KiB")
	}
	if got := len(ev.Output.Fields["body"].GetStringValue()); got != 64*1024 {
		t.Fatalf("response body len=%d, want 64KiB", got)
	}
}

func TestApply_RequestResponseBodyCapAfterHeaderValueRedaction(t *testing.T) {
	const token = "secret"
	// Raw body is 63,000 bytes (<64 KiB); replacing every six-byte secret with
	// the twelve-byte mask expands it beyond the contract cap.
	//
	// The separator is what makes this test say anything. Occurrences packed
	// back to back are one covered RUN and collapse into a single mask, so a
	// bare repeat of the token shrinks instead of growing and would never reach
	// the cap at all.
	d := &fakeDoer{status: 200, body: []byte(strings.Repeat(token+".", 9_000))}
	stream := &internaltest.ApplyStream{}
	_ = newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url":     "https://example.com/v1/resource",
			"method":  "PATCH",
			"headers": map[string]any{"X-Consul-Token": token},
		}),
	}, stream)

	ev := stream.Last()
	if !ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=false after redaction expanded final body beyond 64 KiB")
	}
	body := ev.Output.Fields["body"].GetStringValue()
	if len(body) > 64*1024 {
		t.Fatalf("post-redaction body len=%d, exceeds 64 KiB", len(body))
	}
	if strings.Contains(body, token) {
		t.Fatal("header value survived final capped body")
	}
}

func TestApply_RequestResponseBodyCapAfterVaultMask(t *testing.T) {
	// Raw body is 64,000 bytes (<64 KiB); vault masking expands every marker.
	d := &fakeDoer{status: 200, body: []byte(strings.Repeat("vault:x ", 8_000))}
	stream := &internaltest.ApplyStream{}
	_ = newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url": "https://example.com/v1/resource", "method": "POST",
		}),
	}, stream)

	ev := stream.Last()
	body := ev.Output.Fields["body"].GetStringValue()
	if !ev.Output.Fields["truncated"].GetBoolValue() || len(body) > 64*1024 {
		t.Fatalf("post-mask truncated=%v len=%d, want true and <=64KiB", ev.Output.Fields["truncated"].GetBoolValue(), len(body))
	}
	if strings.Contains(body, "vault:x") {
		t.Fatal("vault ref survived final capped body")
	}
}

func TestApply_RequestResponseBodyCapAfterUTF8Sanitation(t *testing.T) {
	// One invalid byte is replaced by the three-byte U+FFFD, expanding a raw
	// body below the cap to 65,537 bytes.
	raw := append(bytes.Repeat([]byte{'x'}, 64*1024-2), 0xff)
	d := &fakeDoer{status: 200, body: raw}
	stream := &internaltest.ApplyStream{}
	_ = newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url": "https://example.com/v1/resource", "method": "POST",
		}),
	}, stream)

	ev := stream.Last()
	body := ev.Output.Fields["body"].GetStringValue()
	if !ev.Output.Fields["truncated"].GetBoolValue() || len(body) > 64*1024 || !utf8.ValidString(body) {
		t.Fatalf("post-sanitize truncated=%v len=%d valid=%v", ev.Output.Fields["truncated"].GetBoolValue(), len(body), utf8.ValidString(body))
	}
}

// TestApply_RequestConsulAgentLifecycle is the first-consumer integration
// contract. A real guarded client talks to a loopback HTTP server only when
// both independent opt-outs are explicit. The server sees the register,
// maintenance and deregister calls exactly once, with the token on the wire.
func TestApply_RequestConsulAgentLifecycle(t *testing.T) {
	const token = "consul-agent-token"
	wantPaths := []string{
		"/v1/agent/service/register?replace-existing-checks=true",
		"/v1/agent/service/maintenance/redis-exporter?enable=true",
		"/v1/agent/service/deregister/redis-exporter",
	}
	var gotPaths []string
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		gotPaths = append(gotPaths, r.URL.RequestURI())
		if r.Method != stdhttp.MethodPut {
			t.Errorf("method=%s, want PUT", r.Method)
		}
		if got := r.Header.Get("X-Consul-Token"); got != token {
			t.Errorf("X-Consul-Token=%q", got)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(stdhttp.StatusOK)
	}))
	defer server.Close()

	// allow_http lifts only the scheme guard. Without the independent
	// allow_private opt-out the real dial guard must reject loopback before the
	// Consul-shaped handler is reached.
	guarded := &internaltest.ApplyStream{}
	_ = httpmod.New().Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url": server.URL + wantPaths[0], "method": "PUT", "allow_http": true,
		}),
	}, guarded)
	if !guarded.Last().Failed {
		t.Fatal("loopback request succeeded without allow_private")
	}
	if len(gotPaths) != 0 {
		t.Fatalf("guarded request reached server: %v", gotPaths)
	}

	for i, path := range wantPaths {
		params := map[string]any{
			"url":           server.URL + path,
			"method":        "PUT",
			"allow_http":    true,
			"allow_private": true,
			"headers":       map[string]any{"X-Consul-Token": token},
		}
		if i == 0 {
			params["body"] = `{"Name":"redis-exporter","Port":9121}`
			params["content_type"] = "application/json"
		}
		stream := &internaltest.ApplyStream{}
		if err := httpmod.New().Apply(&pluginv1.ApplyRequest{
			State: "request", Params: mustStruct(t, params),
		}, stream); err != nil {
			t.Fatalf("Apply %s: %v", path, err)
		}
		if ev := stream.Last(); ev.Failed || !ev.Changed {
			t.Fatalf("Apply %s failed=%v changed=%v message=%q", path, ev.Failed, ev.Changed, ev.Message)
		}
	}
	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("requests=%v, want %v", gotPaths, wantPaths)
	}
	for i := range wantPaths {
		if gotPaths[i] != wantPaths[i] {
			t.Errorf("request[%d]=%q, want %q", i, gotPaths[i], wantPaths[i])
		}
	}
}

func TestApply_RequestGuardOptsRemainOrthogonal(t *testing.T) {
	for i := 0; i < 8; i++ {
		allowHTTP := i&1 != 0
		insecure := i&2 != 0
		allowPrivate := i&4 != 0
		var got util.HTTPClientOpts
		d := &fakeDoer{status: 200}
		stream := &internaltest.ApplyStream{}
		_ = newModuleCapturing(d, &got).Apply(&pluginv1.ApplyRequest{
			State: "request",
			Params: mustStruct(t, map[string]any{
				"url": "https://example.com/v1/resource", "method": "PATCH",
				"allow_http": allowHTTP, "insecure_skip_verify": insecure, "allow_private": allowPrivate,
			}),
		}, stream)
		want := util.HTTPClientOpts{
			AllowHTTPRedirect: allowHTTP, InsecureSkipVerify: insecure, AllowPrivate: allowPrivate,
			DisableRedirects: true,
		}
		if got != want {
			t.Errorf("flags %03b: opts=%+v, want %+v", i, got, want)
		}
	}
}
