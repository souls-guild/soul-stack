package url_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/url"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// statusDoer is an HTTPDoer that returns a given status (for 304 branches).
// Body is returned only on 2xx.
type statusDoer struct {
	status int
	body   []byte
	calls  int
}

func (d *statusDoer) Do(_ *http.Request) (*http.Response, error) {
	d.calls++
	body := d.body
	if d.status == http.StatusNotModified {
		body = nil
	}
	return &http.Response{
		StatusCode: d.status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

// --- 1. opt-out flags reach the client factory ---

// captureFactory is a factory that records the passed opts and returns a fake.
func captureFactory(captured *util.HTTPClientOpts, doer util.HTTPDoer) func(util.HTTPClientOpts) util.HTTPDoer {
	return func(opts util.HTTPClientOpts) util.HTTPDoer {
		*captured = opts
		return doer
	}
}

// TestApply_OptOutTruthTable — full 2³=8 truth table
// (allow_http × insecure_skip_verify × allow_private) → the expected
// util.HTTPClientOpts actually passed to the capturing factory. Regression
// guard for param→opts mapping (allow_http → AllowHTTPRedirect, rest are
// named 1:1). URL is always https:// (valid regardless of allow_http) so
// every combination reaches client construction and opts actually get
// captured.
func TestApply_OptOutTruthTable(t *testing.T) {
	for i := 0; i < 8; i++ {
		allowHTTP := i&1 != 0
		insecure := i&2 != 0
		allowPrivate := i&4 != 0
		name := fmt.Sprintf("allow_http=%v/insecure=%v/allow_private=%v", allowHTTP, insecure, allowPrivate)
		t.Run(name, func(t *testing.T) {
			var got util.HTTPClientOpts
			m := url.New()
			m.NewClient = captureFactory(&got, &statusDoer{status: http.StatusOK, body: []byte("x")})

			stream := &internaltest.ApplyStream{}
			if err := m.Apply(&pluginv1.ApplyRequest{
				State: "fetched",
				Params: mustStruct(t, map[string]any{
					"url":                  "https://example.com/x",
					"path":                 filepath.Join(t.TempDir(), "f.bin"),
					"allow_http":           allowHTTP,
					"insecure_skip_verify": insecure,
					"allow_private":        allowPrivate,
				}),
			}, stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if stream.Last().Failed {
				t.Fatalf("failed=true: %s", stream.Last().Message)
			}
			want := util.HTTPClientOpts{
				AllowHTTPRedirect:  allowHTTP,
				InsecureSkipVerify: insecure,
				AllowPrivate:       allowPrivate,
			}
			if got != want {
				t.Fatalf("opts в фабрике = %+v, ожидалось %+v", got, want)
			}
		})
	}
}

func TestApply_FlagsThreadedToFactory(t *testing.T) {
	body := []byte("payload")
	var got util.HTTPClientOpts
	m := url.New()
	m.NewClient = captureFactory(&got, &statusDoer{status: http.StatusOK, body: body})

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":                  "http://internal.example/x",
			"path":                 filepath.Join(t.TempDir(), "f.bin"),
			"allow_http":           true,
			"insecure_skip_verify": true,
			"allow_private":        true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true: %s", stream.Last().Message)
	}
	if !got.AllowHTTPRedirect || !got.InsecureSkipVerify || !got.AllowPrivate {
		t.Fatalf("флаги не доехали до фабрики: %+v", got)
	}
}

func TestApply_NoFlags_FactoryGetsSecureDefault(t *testing.T) {
	var got util.HTTPClientOpts
	m := url.New()
	m.NewClient = captureFactory(&got, &statusDoer{status: http.StatusOK, body: []byte("x")})

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "https://example.com/x",
			"path": filepath.Join(t.TempDir(), "f.bin"),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.AllowHTTPRedirect || got.InsecureSkipVerify || got.AllowPrivate {
		t.Fatalf("дефолт не безопасный: %+v", got)
	}
}

// --- 2. allow_http: http:// is accepted in Validate and Apply ---

func TestValidate_AllowHTTP_AcceptsHTTP(t *testing.T) {
	m := url.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":        "http://example.com/x",
			"path":       "/tmp/x",
			"allow_http": true,
		}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok=false для http:// при allow_http=true: %v", reply.Errors)
	}
}

func TestValidate_AllowHTTP_StillRejectsFile(t *testing.T) {
	m := url.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":        "file:///etc/passwd",
			"path":       "/tmp/x",
			"allow_http": true,
		}),
	})
	if reply.Ok {
		t.Fatal("allow_http пропустил file:// (ожидался отказ)")
	}
}

// TestValidate_AllowPrivate_DoesNotOpenHTTPScheme — reverse orthogonality:
// allow_private lifts ONLY the SSRF dial guard, it does NOT weaken the scheme
// check. http:// without allow_http is rejected by Validate even with
// allow_private=true.
func TestValidate_AllowPrivate_DoesNotOpenHTTPScheme(t *testing.T) {
	m := url.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":           "http://10.0.0.5/x",
			"path":          "/tmp/x",
			"allow_private": true,
		}),
	})
	if reply.Ok {
		t.Fatal("allow_private пропустил http:// без allow_http (схема ослаблена не тем флагом)")
	}
}

func TestApply_AllowHTTP_DownloadsOverHTTP(t *testing.T) {
	body := []byte("plaintext payload")
	d := &fakeDoer{body: body}
	m := newModule(d)
	path := filepath.Join(t.TempDir(), "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":        "http://example.com/f.bin",
			"path":       path,
			"allow_http": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true для http:// при allow_http: %s", ev.Message)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("content=%q", got)
	}
}

// TestApply_AllowHTTP_DowngradeRedirect — a real https→http chain under
// allow_http: the downgrade hop is allowed, payload is downloaded over http.
func TestApply_AllowHTTP_DowngradeRedirect(t *testing.T) {
	body := []byte("downgraded ok")
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer httpSrv.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpSrv.URL+"/payload", http.StatusFound)
	}))
	defer tlsSrv.Close()

	// Client with allow_http (downgrade redirect allowed) + InsecureSkipVerify,
	// to trust the httptest TLS cert.
	m := url.New()
	m.NewClient = func(opts util.HTTPClientOpts) util.HTTPDoer {
		c := util.NewHTTPClient(opts)
		c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		return c
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":           tlsSrv.URL + "/start",
			"path":          path,
			"allow_http":    true,
			"allow_private": true, // httptest listens on 127.0.0.1
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true для downgrade при allow_http: %s", ev.Message)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("payload не скачан по http: %q", got)
	}
}

// --- 3. insecure_skip_verify: TLS is not verified ---

// TestApply_InsecureSkipVerify_AcceptsSelfSigned — without
// insecure_skip_verify the httptest self-signed cert is rejected; with the
// flag it's accepted.
func TestApply_InsecureSkipVerify_AcceptsSelfSigned(t *testing.T) {
	body := []byte("self-signed payload")
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer tlsSrv.Close()

	mkModule := func() *url.Module {
		m := url.New()
		// allow_private — httptest is on 127.0.0.1; the insecure flag is threaded
		// into the real util.NewHTTPClient (not substituted with a trusting
		// transport), to test its actual effect.
		m.NewClient = func(opts util.HTTPClientOpts) util.HTTPDoer { return util.NewHTTPClient(opts) }
		return m
	}

	// Without insecure: self-signed → verification error.
	dir := t.TempDir()
	strictPath := filepath.Join(dir, "strict.bin")
	stream := &internaltest.ApplyStream{}
	_ = mkModule().Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":           tlsSrv.URL + "/x",
			"path":          strictPath,
			"allow_private": true,
		}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false для self-signed без insecure_skip_verify")
	}

	// With insecure: downloads successfully.
	okPath := filepath.Join(dir, "ok.bin")
	stream2 := &internaltest.ApplyStream{}
	if err := mkModule().Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":                  tlsSrv.URL + "/x",
			"path":                 okPath,
			"allow_private":        true,
			"insecure_skip_verify": true,
		}),
	}, stream2); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream2.Last().Failed {
		t.Fatalf("failed=true с insecure_skip_verify: %s", stream2.Last().Message)
	}
	got, _ := os.ReadFile(okPath)
	if string(got) != string(body) {
		t.Fatalf("content=%q", got)
	}
}

// --- 4. allow_private: private IP is allowed ---

// TestApply_AllowPrivate_DialsLoopback — without allow_private, dialing
// loopback is blocked by the SSRF guard; with the flag, it goes through.
// Checked via the real util.NewHTTPClient (with dial guard), server on
// 127.0.0.1.
func TestApply_AllowPrivate_DialsLoopback(t *testing.T) {
	body := []byte("internal payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	mkModule := func() *url.Module {
		m := url.New()
		m.NewClient = func(opts util.HTTPClientOpts) util.HTTPDoer { return util.NewHTTPClient(opts) }
		return m
	}

	// srv.URL is http://127.0.0.1:<port>; needs allow_http for the scheme.
	dir := t.TempDir()

	// Without allow_private: dialing loopback is blocked.
	blockedPath := filepath.Join(dir, "blocked.bin")
	stream := &internaltest.ApplyStream{}
	_ = mkModule().Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":        srv.URL + "/x",
			"path":       blockedPath,
			"allow_http": true,
		}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false: SSRF-guard пропустил loopback без allow_private")
	}
	if _, err := os.Stat(blockedPath); !os.IsNotExist(err) {
		t.Fatal("файл создан при заблокированном dial")
	}

	// With allow_private: goes through.
	okPath := filepath.Join(dir, "ok.bin")
	stream2 := &internaltest.ApplyStream{}
	if err := mkModule().Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":           srv.URL + "/x",
			"path":          okPath,
			"allow_http":    true,
			"allow_private": true,
		}),
	}, stream2); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream2.Last().Failed {
		t.Fatalf("failed=true с allow_private для loopback: %s", stream2.Last().Message)
	}
	got, _ := os.ReadFile(okPath)
	if string(got) != string(body) {
		t.Fatalf("content=%q", got)
	}
}

// --- 5. 304 conditional-GET ---

// TestApply_304_RealWire_IfNoneMatch — a real wire check of conditional-GET:
// the httptest server READS the If-None-Match request header and returns 304
// if the etag matches (otherwise 200 + body, which must NOT be downloaded).
// Earlier 304 tests use statusDoer, which ignores the header; this one
// guarantees the module actually puts If-None-Match on the request and
// handles 304 as a no-op.
func TestApply_304_RealWire_IfNoneMatch(t *testing.T) {
	const etag = `"etag-real-wire"`
	body := []byte("cached content")
	var sawIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawIfNoneMatch = r.Header.Get("If-None-Match")
		if sawIfNoneMatch == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Real module client (with CheckRedirect/SSRF-guard); srv is on 127.0.0.1
	// over http → needs allow_http + allow_private.
	m := url.New()
	m.NewClient = func(opts util.HTTPClientOpts) util.HTTPDoer { return util.NewHTTPClient(opts) }

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":           srv.URL + "/f.bin",
			"path":          path,
			"allow_http":    true,
			"allow_private": true,
			"headers":       map[string]any{"If-None-Match": etag},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true при реальном 304: %s", ev.Message)
	}
	if ev.Changed {
		t.Fatal("changed=true при 304 (ожидался no-op)")
	}
	if sawIfNoneMatch != etag {
		t.Fatalf("сервер получил If-None-Match=%q, ожидался %q", sawIfNoneMatch, etag)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("файл изменён при 304: %q", got)
	}
}

func TestApply_304_LocalFileExists_NoOp(t *testing.T) {
	body := []byte("cached content")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	d := &statusDoer{status: http.StatusNotModified}
	m := url.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return d }

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":     "https://example.com/f.bin",
			"path":    path,
			"headers": map[string]any{"If-None-Match": "\"etag-123\""},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true при 304 + локальный файл: %s", ev.Message)
	}
	if ev.Changed {
		t.Fatal("changed=true при 304 без изменения атрибутов")
	}
	// File is untouched.
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("файл изменён при 304: %q", got)
	}
	// output.sha256 is the actual sha of the existing file.
	if ev.Output.Fields["sha256"].GetStringValue() != sha256hex(body) {
		t.Fatal("output.sha256 != sha существующего файла при 304")
	}
}

func TestApply_304_LocalFileExists_AppliesModeDrift(t *testing.T) {
	body := []byte("cached content")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	d := &statusDoer{status: http.StatusNotModified}
	m := url.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return d }

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":     "https://example.com/f.bin",
			"path":    path,
			"mode":    "0600",
			"headers": map[string]any{"If-None-Match": "\"etag-123\""},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	// 304 → content isn't downloaded, but mode drift is corrected (converge).
	if !ev.Changed {
		t.Fatal("changed=false при 304 + drift mode")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600", info.Mode().Perm())
	}
}

// TestApply_304_WithChecksum_NoOp_ShaCorrect — checksum is set + server
// returns 304 + local file exists. The early checksum check does NOT trigger
// (the etag makes the server answer 304 before the file would be
// materialized under new content); download returns notModified=true →
// no-op on the 304 branch without panicking, output.sha256 is the actual
// SHA-256 of the existing file (canonicalSHA256 recomputes sha256 even when
// the checksum algo is sha1). Regression guard for the checksum-branch ×
// 304 intersection.
func TestApply_304_WithChecksum_NoOp_ShaCorrect(t *testing.T) {
	body := []byte("cached content with checksum")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A sha1 checksum matching the existing file → early no-op (download isn't
	// called), 304 is never reached. To exercise the "checksum + 304"
	// intersection we need the download path: use a checksum that does NOT
	// match the file on the early check, where the server still answers 304.
	d := &statusDoer{status: http.StatusNotModified}
	m := url.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return d }

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/f.bin",
			"path":     path,
			"checksum": "sha1:" + strings.Repeat("0", 40), // won't match the file
			"headers":  map[string]any{"If-None-Match": "\"etag-cs\""},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true при checksum + 304 + локальный файл: %s", ev.Message)
	}
	if ev.Changed {
		t.Fatal("changed=true при 304 (ожидался no-op)")
	}
	if d.calls != 1 {
		t.Fatalf("HTTP вызван %d раз (ожидался 1: conditional-GET → 304)", d.calls)
	}
	// output.sha256 is the actual SHA-256 of the existing file, regardless of
	// the sha1 checksum (canonicalSHA256 recomputes sha256).
	if got := ev.Output.Fields["sha256"].GetStringValue(); got != sha256hex(body) {
		t.Fatalf("output.sha256=%q, ожидался sha256 файла %q", got, sha256hex(body))
	}
	// File is untouched.
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("файл изменён при 304: %q", got)
	}
}

func TestApply_304_NoLocalFile_FailsFast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.bin")
	d := &statusDoer{status: http.StatusNotModified}
	m := url.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return d }

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":     "https://example.com/f.bin",
			"path":    path,
			"headers": map[string]any{"If-None-Match": "\"stale-etag\""},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("failed=false при 304 без локального файла")
	}
	if !strings.Contains(ev.Message, "304") || !strings.Contains(ev.Message, "stale If-None-Match") {
		t.Fatalf("неинформативное сообщение об ошибке 304: %q", ev.Message)
	}
	// File wasn't created.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("файл создан при 304 без кэша")
	}
}

// --- 6. warning in output when a guard is lowered ---

// TestApply_GuardWarning_OnGuardLowered — lowering each guard flag puts a
// warning in the final ApplyEvent's output (not slog: the operator sees the
// contour being weakened in RunResult). Same pattern as
// TestApply_GuardWarning* in core.http: read warningsOf(stream.Last()).
// Checks host-only masking — neither path/query nor headers leak into the
// warning.
func TestApply_GuardWarning_OnGuardLowered(t *testing.T) {
	cases := []struct {
		name   string
		param  string
		substr string
	}{
		{"insecure_skip_verify", "insecure_skip_verify", "insecure_skip_verify"},
		{"allow_http", "allow_http", "allow_http"},
		{"allow_private", "allow_private", "allow_private"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDoer{body: []byte("x")}
			m := newModule(d)
			params := map[string]any{
				"url":     "http://host.example/secret-path?token=leak",
				"path":    filepath.Join(t.TempDir(), "f.bin"),
				tc.param:  true,
				"headers": map[string]any{"Authorization": "Bearer leak"},
			}
			// allow_http is needed so http:// passes validation for the non-allow_http cases.
			params["allow_http"] = true
			stream := &internaltest.ApplyStream{}
			_ = m.Apply(&pluginv1.ApplyRequest{State: "fetched", Params: mustStruct(t, params)}, stream)

			ws := warningsOf(stream.Last())
			if !anyWarningContains(ws, tc.substr) {
				t.Fatalf("нет warning про %s в output: %v", tc.substr, ws)
			}
			// host is present, but NOT the full URL and NOT headers.
			if !anyWarningContains(ws, "host.example") {
				t.Fatalf("в warning нет host: %v", ws)
			}
			for _, w := range ws {
				if strings.Contains(w, "secret-path") || strings.Contains(w, "token=leak") || strings.Contains(w, "Bearer") {
					t.Fatalf("warning раскрыл секрет (path/query/header): %q", w)
				}
			}
		})
	}
}

// TestApply_NoWarning_WhenGuardsUp — with default guards, output has no warnings.
func TestApply_NoWarning_WhenGuardsUp(t *testing.T) {
	d := &fakeDoer{body: []byte("x")}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "https://example.com/x",
			"path": filepath.Join(t.TempDir(), "f.bin"),
		}),
	}, stream)
	if w := warningsOf(stream.Last()); len(w) != 0 {
		t.Fatalf("warnings при дефолтных guard-ах: %v", w)
	}
}

// --- 7. bool type check in Validate ---

func TestValidate_RejectsNonBoolFlag(t *testing.T) {
	for _, p := range []string{"allow_http", "insecure_skip_verify", "allow_private"} {
		m := url.New()
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "fetched",
			Params: mustStruct(t, map[string]any{
				"url":  "https://example.com/x",
				"path": "/tmp/x",
				p:      "yes", // string instead of bool
			}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true для не-bool %s", p)
		}
	}
}
