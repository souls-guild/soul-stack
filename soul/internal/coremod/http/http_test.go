package http_test

import (
	"context"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	httpmod "github.com/souls-guild/soul-stack/soul/internal/coremod/http"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeDoer is a deterministic HTTPDoer: returns a fixed body/status for any
// request, records the actually received method and headers. No network.
type fakeDoer struct {
	body       []byte
	status     int
	gotHeaders stdhttp.Header
	gotMethod  string
	calls      int
	err        error
}

func (d *fakeDoer) Do(req *stdhttp.Request) (*stdhttp.Response, error) {
	d.calls++
	d.gotMethod = req.Method
	d.gotHeaders = req.Header.Clone()
	if d.err != nil {
		return nil, d.err
	}
	status := d.status
	if status == 0 {
		status = stdhttp.StatusOK
	}
	return &stdhttp.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(string(d.body))),
		Header:     make(stdhttp.Header),
	}, nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// newModule swaps the client factory to always return a single fakeDoer,
// ignoring opts (for tests that only care about body/status/call routing).
func newModule(d *fakeDoer) *httpmod.Module {
	m := httpmod.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return d }
	return m
}

// newModuleCapturing swaps the factory to return d and records the
// HTTPClientOpts the module called it with into *got. Used by tests that
// check task flags (allow_private / allow_http / insecure_skip_verify)
// propagate independently into client construction.
func newModuleCapturing(d *fakeDoer, got *util.HTTPClientOpts) *httpmod.Module {
	m := httpmod.New()
	m.NewClient = func(opts util.HTTPClientOpts) util.HTTPDoer {
		*got = opts
		return d
	}
	return m
}

// --- Validate ---

func TestValidate_RejectsUnknownVerb(t *testing.T) {
	m := httpmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "checked",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/health"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true for an unknown verb")
	}
}

func TestValidate_RequiresURL(t *testing.T) {
	m := httpmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true without url")
	}
}

func TestValidate_RejectsHTTP(t *testing.T) {
	m := httpmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "http://example.com/health"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true for http:// URL")
	}
}

func TestValidate_RejectsFileScheme(t *testing.T) {
	m := httpmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "file:///etc/passwd"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true for file:// URL")
	}
}

func TestValidate_RejectsMutatingMethod(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "post"} {
		m := httpmod.New()
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "probe",
			Params: mustStruct(t, map[string]any{
				"url":    "https://example.com/health",
				"method": method,
			}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true for mutating method %q", method)
		}
	}
}

func TestValidate_AcceptsGETHEAD(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "get", "head", ""} {
		m := httpmod.New()
		params := map[string]any{"url": "https://example.com/health"}
		if method != "" {
			params["method"] = method
		}
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State:  "probe",
			Params: mustStruct(t, params),
		})
		if !reply.Ok {
			t.Fatalf("Validate ok=false for method %q: %v", method, reply.Errors)
		}
	}
}

func TestValidate_AcceptsValidFull(t *testing.T) {
	m := httpmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":          "https://example.com/health",
			"method":       "GET",
			"status_codes": []any{200, 204},
			"timeout":      "5s",
		}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok=false for a valid probe: %v", reply.Errors)
	}
}

// --- Apply: probe GET 200 ---

func TestApply_GET_200_ReturnsRegister_ChangedFalse(t *testing.T) {
	body := []byte(`{"status":"ok"}`)
	d := &fakeDoer{body: body, status: 200}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/health"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	// changed=false by construction.
	if ev.Changed {
		t.Fatal("changed=true (read-probe must be changed=false)")
	}
	if ev.Output.Fields["changed"].GetBoolValue() != false {
		t.Fatal("output.changed != false")
	}
	if ev.Output.Fields["status"].GetNumberValue() != 200 {
		t.Fatalf("status=%v want 200", ev.Output.Fields["status"].GetNumberValue())
	}
	if ev.Output.Fields["body"].GetStringValue() != string(body) {
		t.Fatalf("body=%q want %q", ev.Output.Fields["body"].GetStringValue(), body)
	}
	if _, ok := ev.Output.Fields["elapsed_ms"]; !ok {
		t.Fatal("elapsed_ms missing from output")
	}
	if d.gotMethod != "GET" {
		t.Fatalf("method=%q want GET", d.gotMethod)
	}
}

// --- status_codes mismatch → failed (but with output) ---

func TestApply_StatusMismatch_Fails_WithOutput(t *testing.T) {
	d := &fakeDoer{body: []byte("error page"), status: 500}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":          "https://example.com/health",
			"status_codes": []any{200},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("failed=false for status 500 outside the expected [200]")
	}
	// Output is attached for diagnostics: actual status/body.
	if ev.Output == nil {
		t.Fatal("output missing on mismatch (needed for diagnostics)")
	}
	if ev.Output.Fields["status"].GetNumberValue() != 500 {
		t.Fatalf("output.status=%v want 500", ev.Output.Fields["status"].GetNumberValue())
	}
}

func TestApply_DefaultStatusCodes_200(t *testing.T) {
	d := &fakeDoer{status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/health"}),
	}, stream)
	if stream.Last().Failed {
		t.Fatal("failed=true for 200 with default status_codes")
	}

	d2 := &fakeDoer{status: 201}
	m2 := newModule(d2)
	stream2 := &internaltest.ApplyStream{}
	_ = m2.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/health"}),
	}, stream2)
	if !stream2.Last().Failed {
		t.Fatal("failed=false for 201 with default status_codes [200]")
	}
}

// --- HEAD: body not read ---

func TestApply_HEAD_NoBody(t *testing.T) {
	d := &fakeDoer{body: []byte("should not be read"), status: 200}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":    "https://example.com/health",
			"method": "HEAD",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	if d.gotMethod != "HEAD" {
		t.Fatalf("method=%q want HEAD", d.gotMethod)
	}
	if ev.Output.Fields["body"].GetStringValue() != "" {
		t.Fatalf("HEAD returned a body: %q", ev.Output.Fields["body"].GetStringValue())
	}
}

// --- headers: sent, output carries only keys (values masked) ---

func TestApply_Headers_Sent_OnlyKeysInOutput(t *testing.T) {
	d := &fakeDoer{body: []byte("ok"), status: 200}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url": "https://example.com/health",
			"headers": map[string]any{
				"Authorization": "Bearer super-secret-token",
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	// Header actually sent.
	if got := d.gotHeaders.Get("Authorization"); got != "Bearer super-secret-token" {
		t.Fatalf("Authorization not sent: %q", got)
	}
	// output carries headers_keys (name only), no value.
	keys := ev.Output.Fields["headers_keys"].GetListValue()
	if keys == nil || len(keys.Values) != 1 || keys.Values[0].GetStringValue() != "Authorization" {
		t.Fatalf("headers_keys=%v want [Authorization]", keys)
	}
	if _, ok := ev.Output.Fields["headers"]; ok {
		t.Fatal("raw headers block present in output")
	}
	// Secret value didn't leak into any output field.
	for k, v := range ev.Output.Fields {
		if strings.Contains(v.GetStringValue(), "super-secret-token") {
			t.Fatalf("header value leaked into output[%q]", k)
		}
	}
}

// --- body cap/truncate ---

func TestApply_BodyCap_Truncated(t *testing.T) {
	// Body deliberately exceeds the cap (64 KiB).
	big := strings.Repeat("a", 70*1024)
	d := &fakeDoer{body: []byte(big), status: 200}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/big"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	if !ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=false for a body > cap")
	}
	gotBody := ev.Output.Fields["body"].GetStringValue()
	if len(gotBody) != 64*1024 {
		t.Fatalf("len(body)=%d want %d (cap)", len(gotBody), 64*1024)
	}
}

func TestApply_BodyUnderCap_NotTruncated(t *testing.T) {
	d := &fakeDoer{body: []byte("small"), status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/small"}),
	}, stream)
	if stream.Last().Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=true for a small body")
	}
}

// --- body masking: vault-ref in the body is masked by the usual MaskSecrets ---

func TestApply_BodyMasked_VaultRef(t *testing.T) {
	d := &fakeDoer{body: []byte("vault:secret/data/x"), status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/v"}),
	}, stream)
	got := stream.Last().Output.Fields["body"].GetStringValue()
	if strings.Contains(got, "vault:secret") {
		t.Fatalf("vault-ref in body not masked: %q", got)
	}
}

// --- transport error → failed ---

func TestApply_TransportError_Fails(t *testing.T) {
	d := &fakeDoer{err: io.ErrUnexpectedEOF}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/x"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false on a transport error")
	}
}

func TestApply_RejectsHTTPScheme_NoCall(t *testing.T) {
	d := &fakeDoer{status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "http://example.com/x"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false for http:// in Apply")
	}
	if d.calls != 0 {
		t.Fatalf("HTTP called %d times for http:// (expected 0)", d.calls)
	}
}

// --- SSRF opt-in: allow_private reaches the factory as HTTPClientOpts.AllowPrivate ---

// probe builds a client per call via the NewClient factory, driven by task
// flags. Checks that allow_private maps independently to
// HTTPClientOpts.AllowPrivate.
func TestApply_AllowPrivate_PropagatesToClientOpts(t *testing.T) {
	t.Run("default (no param) -> AllowPrivate=false", func(t *testing.T) {
		var got util.HTTPClientOpts
		d := &fakeDoer{status: 200}
		m := newModuleCapturing(d, &got)

		stream := &internaltest.ApplyStream{}
		_ = m.Apply(&pluginv1.ApplyRequest{
			State:  "probe",
			Params: mustStruct(t, map[string]any{"url": "https://example.com/health"}),
		}, stream)

		if d.calls != 1 {
			t.Fatalf("client called %d times (expected 1)", d.calls)
		}
		if got.AllowPrivate {
			t.Fatal("AllowPrivate=true without param (default must be guarded)")
		}
	})

	t.Run("allow_private:true -> AllowPrivate=true", func(t *testing.T) {
		var got util.HTTPClientOpts
		d := &fakeDoer{status: 200}
		m := newModuleCapturing(d, &got)

		stream := &internaltest.ApplyStream{}
		_ = m.Apply(&pluginv1.ApplyRequest{
			State: "probe",
			Params: mustStruct(t, map[string]any{
				"url":           "https://internal.svc/health",
				"allow_private": true,
			}),
		}, stream)

		if !got.AllowPrivate {
			t.Fatal("AllowPrivate=false with allow_private:true")
		}
		// Other flags unaffected (orthogonality).
		if got.AllowHTTPRedirect || got.InsecureSkipVerify {
			t.Fatalf("allow_private affected an unrelated flag: %+v", got)
		}
	})

	t.Run("allow_private not bool -> failed before call", func(t *testing.T) {
		var got util.HTTPClientOpts
		d := &fakeDoer{status: 200}
		m := newModuleCapturing(d, &got)

		stream := &internaltest.ApplyStream{}
		_ = m.Apply(&pluginv1.ApplyRequest{
			State: "probe",
			Params: mustStruct(t, map[string]any{
				"url":           "https://example.com/x",
				"allow_private": "yes",
			}),
		}, stream)

		if !stream.Last().Failed {
			t.Fatal("allow_private as a string did not produce failed")
		}
		if d.calls != 0 {
			t.Fatalf("client called with a malformed allow_private (calls=%d)", d.calls)
		}
	})
}

// --- downgrade protection: a real https→http redirect chain is rejected ---

func TestApply_Redirect_HTTPS_to_HTTP_Blocked(t *testing.T) {
	var httpHit bool
	httpSrv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		httpHit = true
		_, _ = w.Write([]byte("downgraded body"))
	}))
	defer httpSrv.Close()

	tlsSrv := httptest.NewTLSServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		stdhttp.Redirect(w, r, httpSrv.URL+"/health", stdhttp.StatusFound)
	}))
	defer tlsSrv.Close()

	client := httpmod.NewRealClient()
	client.Transport = tlsSrv.Client().Transport

	m := httpmod.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return client }

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": tlsSrv.URL + "/start"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false on an https→http redirect")
	}
	if httpHit {
		t.Fatal("downgrade redirect reached the http server")
	}
}

// --- BUG-2: non-UTF8 / split rune doesn't break structpb serialization ---

// applyProbe calls stream.Send; on the success path with a non-UTF8 body it
// used to return a raw gRPC error from structpb.NewStruct. The body is now
// sanitized → clean result, no Apply error.
func TestApply_NonUTF8Body_Success_CleanResult(t *testing.T) {
	d := &fakeDoer{body: []byte{0xff, 0xfe, 'o', 'k'}, status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/bin"}),
	}, stream); err != nil {
		t.Fatalf("Apply returned a gRPC error for a non-UTF8 body: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	got := ev.Output.Fields["body"].GetStringValue()
	if !utf8.ValidString(got) {
		t.Fatalf("sanitized body invalid as UTF-8: %q", got)
	}
}

// A multi-byte rune split exactly at the cap boundary must roll back to the
// last complete rune → valid UTF-8, truncated=true.
func TestApply_RuneSplitAtCap_RuneAware(t *testing.T) {
	// 65535 ASCII bytes + a 2-byte rune 'é' (0xc3 0xa9) = 65537 bytes.
	// Slicing [:65536] would leave the first byte 0xc3 (a broken prefix) — used to fail.
	body := append([]byte(strings.Repeat("a", 64*1024-1)), 0xc3, 0xa9)
	d := &fakeDoer{body: body, status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/rune"}),
	}, stream); err != nil {
		t.Fatalf("Apply returned an error for a split rune: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	got := ev.Output.Fields["body"].GetStringValue()
	if !utf8.ValidString(got) {
		t.Fatalf("body after rune-aware cap invalid: invalid UTF-8")
	}
	if !ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=false when truncating at cap")
	}
	// Rolled back the partial rune → length < cap, no trailing broken byte.
	if strings.HasSuffix(got, "\xc3") {
		t.Fatal("partial rune left at the end of the body")
	}
}

// The mismatch path must still deliver output even with a non-UTF8 body
// (previously structpb.NewStruct err → Output=nil, diagnostics silently lost).
func TestApply_StatusMismatch_NonUTF8Body_OutputDelivered(t *testing.T) {
	d := &fakeDoer{body: []byte{0xff, 0xfe, 'e', 'r', 'r'}, status: 500}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":          "https://example.com/health",
			"status_codes": []any{200},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("failed=false for status 500 outside [200]")
	}
	if ev.Output == nil {
		t.Fatal("output lost on mismatch with a non-UTF8 body (needed for diagnostics)")
	}
	if ev.Output.Fields["status"].GetNumberValue() != 500 {
		t.Fatalf("output.status=%v want 500", ev.Output.Fields["status"].GetNumberValue())
	}
	// changed=false on mismatch-failed (regression guard).
	if ev.Changed {
		t.Fatal("changed=true on mismatch (read-probe must be changed=false)")
	}
	if ev.Output.Fields["changed"].GetBoolValue() {
		t.Fatal("output.changed=true on mismatch")
	}
}

// A body exactly at the 64KiB boundary isn't truncated (off-by-one: read cap+1, len==cap).
func TestApply_BodyExactlyAtCap_NotTruncated(t *testing.T) {
	body := strings.Repeat("a", 64*1024)
	d := &fakeDoer{body: []byte(body), status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/exact"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=true for a body of exactly 64KiB (off-by-one)")
	}
	if got := len(ev.Output.Fields["body"].GetStringValue()); got != 64*1024 {
		t.Fatalf("len(body)=%d want %d", got, 64*1024)
	}
}

// --- BUG-1: vault-ref substring masking in body ---

// A vault-ref INSIDE a JSON body (not a whole-string prefix) is now masked.
func TestApply_BodyMasked_EmbeddedVaultRef(t *testing.T) {
	d := &fakeDoer{body: []byte(`{"token":"vault:secret/data/x"}`), status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/v"}),
	}, stream)
	got := stream.Last().Output.Fields["body"].GetStringValue()
	if strings.Contains(got, "vault:secret") {
		t.Fatalf("embedded vault-ref not masked: %q", got)
	}
	if !strings.Contains(got, "***MASKED***") {
		t.Fatalf("mask missing from body: %q", got)
	}
}

// A body without a vault-ref is left untouched.
func TestApply_BodyNoVaultRef_Untouched(t *testing.T) {
	d := &fakeDoer{body: []byte(`{"status":"ok","up":true}`), status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/h"}),
	}, stream)
	got := stream.Last().Output.Fields["body"].GetStringValue()
	if got != `{"status":"ok","up":true}` {
		t.Fatalf("body without vault-ref was modified: %q", got)
	}
}

// An arbitrary plaintext secret is NOT masked — a documented limitation
// (body is semi-trusted). This test pins the behavior as intentional.
func TestApply_BodyPlaintextSecret_NotMasked(t *testing.T) {
	d := &fakeDoer{body: []byte(`{"password":"hunter2"}`), status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/p"}),
	}, stream)
	got := stream.Last().Output.Fields["body"].GetStringValue()
	if !strings.Contains(got, "hunter2") {
		t.Fatalf("plaintext secret unexpectedly masked (limitation violated): %q", got)
	}
}

// --- coverage (qa gaps) ---

func TestApply_EmptyBody_200(t *testing.T) {
	d := &fakeDoer{body: []byte{}, status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/empty"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true for an empty body 200: %s", ev.Message)
	}
	if ev.Output.Fields["body"].GetStringValue() != "" {
		t.Fatalf("body not empty: %q", ev.Output.Fields["body"].GetStringValue())
	}
	if ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=true for an empty body")
	}
}

func TestValidate_InvalidTimeout(t *testing.T) {
	for _, ts := range []string{"abc", "0s", "-5s", "0"} {
		m := httpmod.New()
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "probe",
			Params: mustStruct(t, map[string]any{
				"url":     "https://example.com/health",
				"timeout": ts,
			}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true for an invalid timeout %q", ts)
		}
	}
}

// Custom status_codes [200,204] actually matches 204 in Apply (not just Validate).
func TestApply_CustomStatusCodes_Matches204(t *testing.T) {
	d := &fakeDoer{status: 204}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":          "https://example.com/health",
			"status_codes": []any{200, 204},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatal("failed=true for 204 with status_codes [200,204]")
	}
}

// status_codes [] → fallback [200] (pin the behavior).
func TestApply_EmptyStatusCodes_FallbackTo200(t *testing.T) {
	d200 := &fakeDoer{status: 200}
	s := &internaltest.ApplyStream{}
	_ = newModule(d200).Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":          "https://example.com/h",
			"status_codes": []any{},
		}),
	}, s)
	if s.Last().Failed {
		t.Fatal("failed=true for 200 with status_codes [] (expected fallback [200])")
	}

	d201 := &fakeDoer{status: 201}
	s2 := &internaltest.ApplyStream{}
	_ = newModule(d201).Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":          "https://example.com/h",
			"status_codes": []any{},
		}),
	}, s2)
	if !s2.Last().Failed {
		t.Fatal("failed=false for 201 with status_codes [] (fallback [200] did not kick in)")
	}
}

// headers_keys are deterministic (sorted) regardless of insertion order.
func TestApply_HeaderKeys_Sorted(t *testing.T) {
	d := &fakeDoer{body: []byte("ok"), status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url": "https://example.com/h",
			"headers": map[string]any{
				"X-Zebra":       "1",
				"Authorization": "2",
				"Accept":        "3",
			},
		}),
	}, stream)
	keys := stream.Last().Output.Fields["headers_keys"].GetListValue()
	want := []string{"Accept", "Authorization", "X-Zebra"}
	if keys == nil || len(keys.Values) != len(want) {
		t.Fatalf("headers_keys=%v want %v", keys, want)
	}
	for i, w := range want {
		if got := keys.Values[i].GetStringValue(); got != w {
			t.Fatalf("headers_keys[%d]=%q want %q (not sorted)", i, got, w)
		}
	}
}

// --- timeout: a server slower than timeout → failed, doesn't hang ---

func TestApply_Timeout_Fails(t *testing.T) {
	release := make(chan struct{})
	tlsSrv := httptest.NewTLSServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write([]byte("late"))
	}))
	defer tlsSrv.Close()
	defer close(release)

	client := httpmod.NewRealClient()
	client.Transport = tlsSrv.Client().Transport
	m := httpmod.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return client }

	done := make(chan struct{})
	stream := &internaltest.ApplyStream{}
	go func() {
		_ = m.Apply(&pluginv1.ApplyRequest{
			State: "probe",
			Params: mustStruct(t, map[string]any{
				"url":     tlsSrv.URL + "/slow",
				"timeout": "200ms",
			}),
		}, stream)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Apply hung: timeout did not trigger")
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false when exceeding timeout")
	}
}
