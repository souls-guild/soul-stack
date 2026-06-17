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

// statusDoer — HTTPDoer, отдающий заданный статус (для 304-веток). Тело
// отдаётся только на 2xx.
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

// --- 1. opt-out флаги доезжают до фабрики клиента ---

// captureFactory — фабрика, фиксирующая переданные opts, и возвращающая fake.
func captureFactory(captured *util.HTTPClientOpts, doer util.HTTPDoer) func(util.HTTPClientOpts) util.HTTPDoer {
	return func(opts util.HTTPClientOpts) util.HTTPDoer {
		*captured = opts
		return doer
	}
}

// TestApply_OptOutTruthTable — полная истинностная таблица 2³=8 комбинаций
// (allow_http × insecure_skip_verify × allow_private) → ожидаемый
// util.HTTPClientOpts, фактически переданный в захватывающую фабрику. Регресс-гард
// маппинга param→opts (allow_http → AllowHTTPRedirect, остальные именные).
// URL всегда https:// (валиден при любом allow_http), чтобы каждая комбинация
// доходила до построения клиента и опции реально фиксировались.
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

// --- 2. allow_http: http:// принят в Validate и Apply ---

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

// TestValidate_AllowPrivate_DoesNotOpenHTTPScheme — обратная ортогональность:
// allow_private снимает ТОЛЬКО SSRF-dial-guard, но НЕ ослабляет проверку схемы.
// http:// без allow_http отвергается на Validate даже при allow_private=true.
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

// TestApply_AllowHTTP_DowngradeRedirect — реальная цепочка https→http при
// allow_http: downgrade-hop допустим, payload скачивается по http.
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

	// Клиент с allow_http (downgrade-redirect разрешён) + InsecureSkipVerify,
	// чтобы доверять httptest TLS-cert.
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
			"allow_private": true, // httptest слушает на 127.0.0.1
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

// --- 3. insecure_skip_verify: TLS не верифицируется ---

// TestApply_InsecureSkipVerify_AcceptsSelfSigned — без insecure_skip_verify
// httptest self-signed cert отвергается; с флагом — принимается.
func TestApply_InsecureSkipVerify_AcceptsSelfSigned(t *testing.T) {
	body := []byte("self-signed payload")
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer tlsSrv.Close()

	mkModule := func() *url.Module {
		m := url.New()
		// allow_private — httptest на 127.0.0.1; флаг insecure прокидывается в
		// реальный util.NewHTTPClient (а не подменяется доверяющим transport-ом),
		// чтобы проверить именно его эффект.
		m.NewClient = func(opts util.HTTPClientOpts) util.HTTPDoer { return util.NewHTTPClient(opts) }
		return m
	}

	// Без insecure: self-signed → ошибка верификации.
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

	// С insecure: скачивается.
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

// --- 4. allow_private: приватный IP разрешён ---

// TestApply_AllowPrivate_DialsLoopback — без allow_private dial в loopback
// блокируется SSRF-guard'ом; с флагом — проходит. Проверяется через реальный
// util.NewHTTPClient (с dial-guard), сервер на 127.0.0.1.
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

	// srv.URL — http://127.0.0.1:<port>; нужен allow_http для схемы.
	dir := t.TempDir()

	// Без allow_private: dial в loopback заблокирован.
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

	// С allow_private: проходит.
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

// TestApply_304_RealWire_IfNoneMatch — реальная wire-проверка conditional-GET:
// httptest-сервер ЧИТАЕТ request-header If-None-Match и отдаёт 304, если etag
// совпал (иначе 200 + тело, которое скачивать НЕ должны). Предыдущие 304-тесты
// используют statusDoer, игнорирующий заголовок; этот гарантирует, что модуль
// реально кладёт If-None-Match в запрос и обрабатывает 304 как no-op.
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

	// Реальный клиент модуля (с CheckRedirect/SSRF-guard); srv на 127.0.0.1 по
	// http → нужны allow_http + allow_private.
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
	// Файл не тронут.
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("файл изменён при 304: %q", got)
	}
	// output.sha256 — фактический sha существующего файла.
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
	// 304 → контент не качаем, но mode-drift правим (converge).
	if !ev.Changed {
		t.Fatal("changed=false при 304 + drift mode")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600", info.Mode().Perm())
	}
}

// TestApply_304_WithChecksum_NoOp_ShaCorrect — checksum задан + сервер 304 +
// локальный файл существует. Ранняя checksum-проверка НЕ срабатывает (etag
// заставляет сервер ответить 304 раньше, чем файл материализован под новый
// контент), download возвращает notModified=true → no-op по 304-ветке без паники,
// output.sha256 — фактический SHA-256 существующего файла (canonicalSHA256 с algo
// sha1 пересчитывает sha256). Регресс-гард на пересечение checksum-ветки и 304.
func TestApply_304_WithChecksum_NoOp_ShaCorrect(t *testing.T) {
	body := []byte("cached content with checksum")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// checksum по sha1 совпадает с существующим файлом → ранняя no-op (download не
	// зовётся), 304 не достигается. Чтобы проверить пересечение «checksum + 304»,
	// нужен путь download: используем checksum, НЕ совпадающий с файлом на ранней
	// проверке, при котором сервер всё равно ответит 304.
	d := &statusDoer{status: http.StatusNotModified}
	m := url.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return d }

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/f.bin",
			"path":     path,
			"checksum": "sha1:" + strings.Repeat("0", 40), // не совпадёт с файлом
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
	// output.sha256 — фактический SHA-256 существующего файла, несмотря на checksum
	// по sha1 (canonicalSHA256 пересчитывает sha256).
	if got := ev.Output.Fields["sha256"].GetStringValue(); got != sha256hex(body) {
		t.Fatalf("output.sha256=%q, ожидался sha256 файла %q", got, sha256hex(body))
	}
	// Файл не тронут.
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
	// Файл не создан.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("файл создан при 304 без кэша")
	}
}

// --- 6. warning в output при снятии guard ---

// TestApply_GuardWarning_OnGuardLowered — снятие каждого guard-флага кладёт
// warning в output финального ApplyEvent (а не в slog: оператор видит факт
// ослабления контура в RunResult). Паттерн TestApply_GuardWarning* из core.http:
// читаем warningsOf(stream.Last()). Проверяется host-only-маскинг — ни path/
// query, ни headers в warning не светятся.
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
			// allow_http нужен, чтобы http:// прошёл валидацию у не-allow_http кейсов.
			params["allow_http"] = true
			stream := &internaltest.ApplyStream{}
			_ = m.Apply(&pluginv1.ApplyRequest{State: "fetched", Params: mustStruct(t, params)}, stream)

			ws := warningsOf(stream.Last())
			if !anyWarningContains(ws, tc.substr) {
				t.Fatalf("нет warning про %s в output: %v", tc.substr, ws)
			}
			// host есть, но НЕ полный URL и НЕ headers.
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

// TestApply_NoWarning_WhenGuardsUp — при дефолтных guard-ах output без warnings.
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

// --- 7. bool тип-чек в Validate ---

func TestValidate_RejectsNonBoolFlag(t *testing.T) {
	for _, p := range []string{"allow_http", "insecure_skip_verify", "allow_private"} {
		m := url.New()
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "fetched",
			Params: mustStruct(t, map[string]any{
				"url":  "https://example.com/x",
				"path": "/tmp/x",
				p:      "yes", // строка вместо bool
			}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true для не-bool %s", p)
		}
	}
}
