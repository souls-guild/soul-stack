package url_test

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	stdurl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/url"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeDoer — детерминированный HTTPDoer: возвращает body для любого запроса,
// записывает реально полученные заголовки для проверки. Сетевых вызовов нет.
type fakeDoer struct {
	body       []byte
	status     int
	gotHeaders http.Header
	calls      int
	err        error
}

func (d *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls++
	d.gotHeaders = req.Header.Clone()
	if d.err != nil {
		return nil, d.err
	}
	status := d.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(string(d.body))),
		Header:     make(http.Header),
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

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// warningsOf извлекает список warnings из output последнего события (или nil,
// если поля нет). Симметрично warningsOf в core.http.
func warningsOf(ev *pluginv1.ApplyEvent) []string {
	if ev.Output == nil {
		return nil
	}
	lv := ev.Output.Fields["warnings"].GetListValue()
	if lv == nil {
		return nil
	}
	out := make([]string, 0, len(lv.Values))
	for _, v := range lv.Values {
		out = append(out, v.GetStringValue())
	}
	return out
}

func anyWarningContains(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func sha1hex(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}

// newModule подменяет фабрику клиента на возврат единственного fakeDoer,
// игнорируя opts (единый test-seam NewClient, симметрично core.http).
func newModule(d *fakeDoer) *url.Module {
	m := url.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return d }
	return m
}

func TestValidate_RejectsUnknownState(t *testing.T) {
	m := url.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "downloaded",
		Params: mustStruct(t, map[string]any{
			"url":  "https://example.com/x",
			"path": "/tmp/x",
		}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для неизвестного state")
	}
}

func TestValidate_RejectsHTTP(t *testing.T) {
	m := url.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "http://example.com/x",
			"path": "/tmp/x",
		}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для http:// URL")
	}
}

func TestValidate_RejectsFileScheme(t *testing.T) {
	m := url.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "file:///etc/passwd",
			"path": "/tmp/x",
		}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для file:// URL")
	}
}

func TestValidate_RejectsMD5Checksum(t *testing.T) {
	m := url.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/x",
			"path":     "/tmp/x",
			"checksum": "md5:" + strings.Repeat("a", 32),
		}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для md5-checksum")
	}
}

func TestValidate_AcceptsValid(t *testing.T) {
	m := url.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/x",
			"path":     "/tmp/x",
			"checksum": "sha256:" + strings.Repeat("a", 64),
			"timeout":  "30s",
		}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok=false для валидного fetched: %v", reply.Errors)
	}
}

func TestApply_RejectsHTTPScheme(t *testing.T) {
	d := &fakeDoer{body: []byte("data")}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "http://example.com/x",
			"path": filepath.Join(t.TempDir(), "x"),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false для http:// в Apply")
	}
	if d.calls != 0 {
		t.Fatalf("HTTP вызван %d раз для http:// (ожидалось 0)", d.calls)
	}
}

func TestApply_RejectsFileScheme(t *testing.T) {
	d := &fakeDoer{body: []byte("data")}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "file:///etc/passwd",
			"path": filepath.Join(t.TempDir(), "x"),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false для file:// в Apply")
	}
	if d.calls != 0 {
		t.Fatalf("HTTP вызван %d раз для file:// (ожидалось 0)", d.calls)
	}
}

func TestApply_Download_NoChecksum_CreatesFile(t *testing.T) {
	body := []byte("payload\n")
	d := &fakeDoer{body: body}
	m := newModule(d)
	path := filepath.Join(t.TempDir(), "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "https://example.com/f.bin",
			"path": path,
			"mode": "0640",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("failed=%v changed=%v", ev.Failed, ev.Changed)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("content=%q want %q", got, body)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v want 0640", info.Mode().Perm())
	}
	if ev.Output.Fields["sha256"].GetStringValue() != sha256hex(body) {
		t.Fatal("sha256 mismatch в output")
	}
	if ev.Output.Fields["size"].GetNumberValue() != float64(len(body)) {
		t.Fatalf("size=%v want %d", ev.Output.Fields["size"].GetNumberValue(), len(body))
	}
}

func TestApply_Checksum_SHA256_VerifyPasses(t *testing.T) {
	body := []byte("verified content")
	d := &fakeDoer{body: body}
	m := newModule(d)
	path := filepath.Join(t.TempDir(), "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/f.bin",
			"path":     path,
			"checksum": "sha256:" + sha256hex(body),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true при совпадающем sha256: %s", stream.Last().Message)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("content=%q", got)
	}
}

func TestApply_Checksum_SHA1_VerifyPasses(t *testing.T) {
	body := []byte("sha1 verified content")
	d := &fakeDoer{body: body}
	m := newModule(d)
	path := filepath.Join(t.TempDir(), "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/f.bin",
			"path":     path,
			"checksum": "sha1:" + sha1hex(body),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true при совпадающем sha1: %s", ev.Message)
	}
	// output.sha256 — всегда SHA-256 фактического содержимого, даже когда
	// checksum задан по sha1.
	if ev.Output.Fields["sha256"].GetStringValue() != sha256hex(body) {
		t.Fatal("output.sha256 не SHA-256 содержимого")
	}
}

func TestApply_Checksum_Mismatch_Fails_NoMaterialize(t *testing.T) {
	body := []byte("malicious payload")
	d := &fakeDoer{body: body}
	m := newModule(d)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/f.bin",
			"path":     path,
			"checksum": "sha256:" + strings.Repeat("0", 64),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false при mismatch checksum")
	}
	// Целевой файл не создан.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("целевой файл материализован при mismatch: err=%v", err)
	}
	// Никаких temp-остатков в директории.
	assertNoTempLeftovers(t, dir)
}

func TestApply_Checksum_Idempotent_SkipsDownload(t *testing.T) {
	body := []byte("already here")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	d := &fakeDoer{body: []byte("DIFFERENT")}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/f.bin",
			"path":     path,
			"checksum": "sha256:" + sha256hex(body),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	if ev.Changed {
		t.Fatal("changed=true при совпадающем checksum существующего файла")
	}
	if d.calls != 0 {
		t.Fatalf("HTTP вызван %d раз при совпадающем checksum (ожидалось 0)", d.calls)
	}
	// Файл не перезаписан мусором из fakeDoer.
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("файл перезаписан: %q", got)
	}
}

func TestApply_Checksum_NoOp_AppliesModeDrift(t *testing.T) {
	body := []byte("already here")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	d := &fakeDoer{body: []byte("DIFFERENT")}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/f.bin",
			"path":     path,
			"checksum": "sha256:" + sha256hex(body),
			"mode":     "0600",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	// Контент совпал → скачивания нет, но mode должен быть приведён к декларации.
	if !ev.Changed {
		t.Fatal("changed=false при drift mode на совпавшем checksum")
	}
	if d.calls != 0 {
		t.Fatalf("HTTP вызван %d раз при совпавшем checksum (ожидалось 0)", d.calls)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600", info.Mode().Perm())
	}
	// Файл не перезаписан мусором из fakeDoer.
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("файл перезаписан: %q", got)
	}
}

func TestApply_Checksum_NoOp_ModeMatches_TrueNoOp(t *testing.T) {
	body := []byte("already here")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	d := &fakeDoer{body: []byte("DIFFERENT")}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/f.bin",
			"path":     path,
			"checksum": "sha256:" + sha256hex(body),
			"mode":     "0600",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	// Контент и mode совпали → истинный no-op.
	if ev.Changed {
		t.Fatal("changed=true при совпавших checksum и mode")
	}
	if d.calls != 0 {
		t.Fatalf("HTTP вызван %d раз (ожидалось 0)", d.calls)
	}
}

func TestApply_NoChecksum_NoOp_AppliesModeDrift(t *testing.T) {
	body := []byte("identical")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	d := &fakeDoer{body: body}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "https://example.com/f.bin",
			"path": path,
			"mode": "0600",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	// Контент совпал (бесчексумная ветка) → запись не нужна, но mode-drift правим.
	if !ev.Changed {
		t.Fatal("changed=false при drift mode на совпавшем содержимом")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600", info.Mode().Perm())
	}
	assertNoTempLeftovers(t, dir)
}

func TestApply_NoChecksum_Idempotent_SameContent(t *testing.T) {
	body := []byte("identical")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	d := &fakeDoer{body: body}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "https://example.com/f.bin",
			"path": path,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	if ev.Changed {
		t.Fatal("changed=true при идентичном содержимом (no checksum)")
	}
	// Без checksum скачивание всё равно происходит (для сравнения), но diff нет.
	if d.calls != 1 {
		t.Fatalf("HTTP вызван %d раз (ожидался 1: скачать-и-сравнить)", d.calls)
	}
	assertNoTempLeftovers(t, dir)
}

func TestApply_NoChecksum_Changed_OnDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	newBody := []byte("new content")
	d := &fakeDoer{body: newBody}
	m := newModule(d)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "https://example.com/f.bin",
			"path": path,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false при diff содержимого")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(newBody) {
		t.Fatalf("content=%q", got)
	}
}

func TestApply_HeadersSent_NotInOutput(t *testing.T) {
	body := []byte("payload")
	d := &fakeDoer{body: body}
	m := newModule(d)
	path := filepath.Join(t.TempDir(), "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "https://example.com/f.bin",
			"path": path,
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
	// Заголовок действительно отправлен в запросе.
	if got := d.gotHeaders.Get("Authorization"); got != "Bearer super-secret-token" {
		t.Fatalf("Authorization не отправлен: %q", got)
	}
	// headers нет в output ни ключом, ни значением.
	if _, ok := ev.Output.Fields["headers"]; ok {
		t.Fatal("headers присутствует в output")
	}
	for k, v := range ev.Output.Fields {
		if strings.Contains(v.GetStringValue(), "super-secret-token") {
			t.Fatalf("значение заголовка просочилось в output[%q]", k)
		}
	}
}

func TestApply_HTTPError_Fails_NoFile(t *testing.T) {
	d := &fakeDoer{status: http.StatusNotFound, body: []byte("nope")}
	m := newModule(d)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "https://example.com/f.bin",
			"path": path,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false при HTTP 404")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("файл создан при HTTP-ошибке")
	}
	assertNoTempLeftovers(t, dir)
}

func TestApply_MissingURL_Fails(t *testing.T) {
	m := url.New()
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "fetched",
		Params: mustStruct(t, map[string]any{"path": "/tmp/x"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false при отсутствии url")
	}
}

// --- downgrade-защита: CheckRedirect ---

// TestCheckRedirect_BlocksNonHTTPS проверяет саму CheckRedirect в изоляции:
// hop на http/любую не-https схему → ошибка; https → nil. Это покрывает
// downgrade-защиту независимо от fake-инъекции (fake HTTPDoer не прогоняет
// CheckRedirect реального клиента).
func TestCheckRedirect_BlocksNonHTTPS(t *testing.T) {
	mkReq := func(raw string) *http.Request {
		u, err := stdurl.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return &http.Request{URL: u}
	}

	for _, raw := range []string{
		"http://evil.example/x",
		"HTTP://evil.example/x",
		"ftp://evil.example/x",
		"file:///etc/passwd",
	} {
		if err := url.CheckRedirect(mkReq(raw), nil); err == nil {
			t.Fatalf("CheckRedirect пропустил downgrade на %q", raw)
		}
	}

	for _, raw := range []string{
		"https://ok.example/x",
		"HTTPS://ok.example/x",
	} {
		if err := url.CheckRedirect(mkReq(raw), nil); err != nil {
			t.Fatalf("CheckRedirect отверг валидный https %q: %v", raw, err)
		}
	}

	// Лимит редиректов: цепочка длиной util.MaxRedirects → ошибка даже на https.
	via := make([]*http.Request, url.MaxRedirects)
	if err := url.CheckRedirect(mkReq("https://ok.example/x"), via); err == nil {
		t.Fatal("CheckRedirect не остановил цепочку на лимите редиректов")
	}
}

// TestApply_Redirect_HTTPS_to_HTTP_Blocked прогоняет реальную redirect-цепочку
// через httptest: https-сервер отдаёт 302 Location: http://… — fetch обязан
// упасть (downgrade заблокирован), целевой файл не создаётся.
func TestApply_Redirect_HTTPS_to_HTTP_Blocked(t *testing.T) {
	// http-сервер, куда указывает downgrade-редирект; до него дойти НЕ должны.
	var httpHit bool
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpHit = true
		_, _ = w.Write([]byte("downgraded payload"))
	}))
	defer httpSrv.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, nil, httpSrv.URL+"/payload", http.StatusFound)
	}))
	defer tlsSrv.Close()

	// Реальный клиент модуля (с CheckRedirect), Transport — доверяющий
	// самоподписанному httptest-cert. CheckRedirect сохраняется.
	client := url.NewRealClient()
	client.Transport = tlsSrv.Client().Transport

	m := url.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return client }
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  tlsSrv.URL + "/start",
			"path": path,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false при редиректе https→http")
	}
	if httpHit {
		t.Fatal("downgrade-редирект достиг http-сервера (payload скачан по http)")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("файл создан при заблокированном редиректе")
	}
	assertNoTempLeftovers(t, dir)
}

// TestValidate_AcceptsUppercaseScheme — `HTTPS://` валиден (схема сверяется
// регистронезависимо через url.Parse, не строковым префиксом).
func TestValidate_AcceptsUppercaseScheme(t *testing.T) {
	m := url.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  "HTTPS://example.com/x",
			"path": "/tmp/x",
		}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok=false для HTTPS:// (uppercase): %v", reply.Errors)
	}
}

// TestValidate_RejectsControlCharScheme — мусорная/control-char схема и
// «склейка» через перевод строки отвергаются (наивный HasPrefix их пропускал).
func TestValidate_RejectsControlCharScheme(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/\nhttp://evil.example/x",
		"ht\x00tps://example.com/x",
		"://example.com/x",
		"https ://example.com/x",
	} {
		reply, _ := m_validate(t, raw)
		if reply.Ok {
			t.Fatalf("Validate ok=true для мусорной схемы %q", raw)
		}
	}
}

func m_validate(t *testing.T, rawURL string) (*pluginv1.ValidateReply, error) {
	t.Helper()
	m := url.New()
	return m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":  rawURL,
			"path": "/tmp/x",
		}),
	})
}

// --- checksum: невалидные форматы ---

func TestValidate_RejectsBadChecksums(t *testing.T) {
	cases := map[string]string{
		"без двоеточия":      "abc123",
		"sha512 unsupported": "sha512:" + strings.Repeat("a", 128),
		"md5 unsupported":    "md5:" + strings.Repeat("a", 32),
		"нечётный hex":       "sha256:" + strings.Repeat("a", 63),
		"не-hex символы":     "sha256:" + strings.Repeat("z", 64),
		"короткая длина":     "sha256:" + strings.Repeat("a", 63),
		"sha1 кривая длина":  "sha1:" + strings.Repeat("a", 39),
	}
	for name, cs := range cases {
		t.Run(name, func(t *testing.T) {
			m := url.New()
			reply, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State: "fetched",
				Params: mustStruct(t, map[string]any{
					"url":      "https://example.com/x",
					"path":     "/tmp/x",
					"checksum": cs,
				}),
			})
			if err != nil {
				t.Fatalf("Validate вернул error (ожидалась штатная валидация): %v", err)
			}
			if reply.Ok {
				t.Fatalf("Validate ok=true для невалидного checksum %q", cs)
			}
		})
	}
}

// --- timeout: дефолт срабатывает ---

// TestApply_Timeout_Fails — медленный сервер дольше заданного timeout → fetch
// падает по контексту, не виснет. Через httptest с задержкой и явный короткий
// timeout (дефолт 300s в тесте ждать нельзя).
func TestApply_Timeout_Fails(t *testing.T) {
	release := make(chan struct{})
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write([]byte("late"))
	}))
	defer tlsSrv.Close()
	defer close(release)

	client := url.NewRealClient()
	client.Transport = tlsSrv.Client().Transport
	m := url.New()
	m.NewClient = func(util.HTTPClientOpts) util.HTTPDoer { return client }
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")

	done := make(chan struct{})
	stream := &internaltest.ApplyStream{}
	go func() {
		_ = m.Apply(&pluginv1.ApplyRequest{
			State: "fetched",
			Params: mustStruct(t, map[string]any{
				"url":     tlsSrv.URL + "/slow",
				"path":    path,
				"timeout": "200ms",
			}),
		}, stream)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Apply завис: timeout не сработал")
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false при превышении timeout")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("файл создан при timeout")
	}
	assertNoTempLeftovers(t, dir)
}

// --- пустой ответ ---

// TestApply_EmptyBody_CreatesEmptyFile — ответ 0 байт: файл создаётся пустым,
// output.sha256 = sha256 пустой строки, checksum-ветка сверяет корректно.
func TestApply_EmptyBody_CreatesEmptyFile(t *testing.T) {
	empty := []byte{}
	d := &fakeDoer{body: empty}
	m := newModule(d)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "fetched",
		Params: mustStruct(t, map[string]any{
			"url":      "https://example.com/empty",
			"path":     path,
			"checksum": "sha256:" + sha256hex(empty),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true для пустого тела: %s", ev.Message)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("файл не создан: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("size=%d want 0", info.Size())
	}
	if ev.Output.Fields["sha256"].GetStringValue() != sha256hex(empty) {
		t.Fatal("output.sha256 != sha256 пустой строки")
	}
	assertNoTempLeftovers(t, dir)
}

// --- параллельная запись в один path ---

// TestApply_ParallelSamePath_NoCorruption — несколько одновременных fetch в
// одну path: temp-имена уникальны, материализация — atomic rename, поэтому
// результат — валидное содержимое (last-writer-wins), без повреждения и без
// temp-остатков.
func TestApply_ParallelSamePath_NoCorruption(t *testing.T) {
	body := []byte("concurrent payload")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			d := &fakeDoer{body: body}
			m := newModule(d)
			stream := &internaltest.ApplyStream{}
			_ = m.Apply(&pluginv1.ApplyRequest{
				State: "fetched",
				Params: mustStruct(t, map[string]any{
					"url":  "https://example.com/f.bin",
					"path": path,
				}),
			}, stream)
		}()
	}
	wg.Wait()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("файл не создан после параллельных fetch: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("повреждённое содержимое: %q want %q", got, body)
	}
	assertNoTempLeftovers(t, dir)
}

// assertNoTempLeftovers проверяет, что в директории не осталось temp-файлов
// модуля (паттерн ".<base>.tmp-*").
func assertNoTempLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("остался temp-файл: %s", e.Name())
		}
	}
}
