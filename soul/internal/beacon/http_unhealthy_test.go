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

// fakeDoer — детерминированный HTTPDoer: отдаёт заданный статус (тело-заглушку
// beacon не читает), либо транспортную ошибку. Сети нет.
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

// newHTTPUnhealthyCapturing использует прод-фабрику util.NewHTTPClient (реальный
// dial / TLS), но захватывает переданные opts — регресс-гард маппинга
// param→HTTPClientOpts на прод-пути построения клиента.
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
		t.Error("data.status должно нести статус-код")
	}
	if _, hasBody := data.GetFields()["body"]; hasBody {
		t.Error("data НЕ должно нести тело ответа (sensitive)")
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
		t.Fatalf("state = %q, want unhealthy (503 вне [200])", state)
	}
	if int(data.GetFields()["status"].GetNumberValue()) != 503 {
		t.Error("data.status должно нести фактический код 503")
	}
}

func TestHTTPUnhealthyCustomStatusCodes(t *testing.T) {
	// 204 здоров при status_codes [200,204]; при дефолтном [200] был бы unhealthy.
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
	// Транспортная ошибка → unhealthy (status 0), а не ошибка Check.
	b := newHTTPUnhealthy(&fakeDoer{err: errors.New("connection refused")})
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"url": "https://down.internal/healthz",
	}))
	if err != nil {
		t.Fatalf("Check при транспортной ошибке не должен возвращать ошибку: %v", err)
	}
	if state != stateHTTPUnhealthy {
		t.Fatalf("state = %q, want unhealthy", state)
	}
	if int(data.GetFields()["status"].GetNumberValue()) != 0 {
		t.Error("data.status при транспортной ошибке должно быть 0")
	}
}

func TestHTTPUnhealthyRejectsHTTP(t *testing.T) {
	// https-only переиспользуется у core.http: http:// отвергается на Check.
	b := newHTTPUnhealthy(&fakeDoer{status: 200})
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"url": "http://service.internal/healthz",
	})); err == nil {
		t.Fatal("ожидали ошибку при http:// (https-only)")
	}
}

func TestHTTPUnhealthyMissingURL(t *testing.T) {
	b := newHTTPUnhealthy(&fakeDoer{status: 200})
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("ожидали ошибку при отсутствии param url")
	}
}

// --- opt-out-флаги (паттерн core.http): default secure, явный opt-out снимает контур ---

// allow_http:true → http:// принят на Check (ValidateFetchURL пропускает),
// и opt доезжает до фабрики как AllowHTTPRedirect (парность downgrade-hop).
// Дозвон герметичный (fakeDoer) — проверяем валидацию схемы и маппинг param→opts,
// а не реальный http-dial.
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
		t.Fatalf("Check при allow_http:true не должен падать на http://: %v", err)
	}
	if state != stateHTTPHealthy {
		t.Fatalf("state = %q, want healthy", state)
	}
	if !got.AllowHTTPRedirect {
		t.Fatal("allow_http не доехал до фабрики как AllowHTTPRedirect")
	}
	if got.AllowPrivate || got.InsecureSkipVerify {
		t.Fatalf("allow_http задел чужой контур: %+v", got)
	}
}

// allow_private:true → реальный dial к loopback-серверу (127.0.0.1) проходит
// SSRF-guard → healthy. Без флага тот же loopback блокируется на dial-фазе.
func TestHTTPUnhealthyAllowPrivateLoopback(t *testing.T) {
	// httptest.NewTLSServer слушает на 127.0.0.1 с самоподписанным cert — нужен
	// и allow_private (loopback), и insecure_skip_verify (self-signed).
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Run("allow_private+insecure -> dial проходит -> healthy", func(t *testing.T) {
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
			t.Fatalf("state = %q, want healthy (loopback при allow_private)", state)
		}
		if int(data.GetFields()["status"].GetNumberValue()) != 200 {
			t.Errorf("status = %v, want 200", data.GetFields()["status"].GetNumberValue())
		}
		if !got.AllowPrivate || !got.InsecureSkipVerify {
			t.Fatalf("opts не доехали до фабрики: %+v", got)
		}
	})

	t.Run("default -> SSRF-guard блокирует loopback -> unhealthy", func(t *testing.T) {
		// Без allow_private dial в 127.0.0.1 отвергается netguard → транспортная
		// ошибка → unhealthy (status 0), а не ошибка Check.
		b := NewHTTPUnhealthy() // прод-фабрика, нулевые opts
		state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
			"url":                  srv.URL + "/health",
			"insecure_skip_verify": true, // изолируем именно SSRF-контур, не TLS
		}))
		if err != nil {
			t.Fatalf("Check при заблокированном dial не должен падать: %v", err)
		}
		if state != stateHTTPUnhealthy {
			t.Fatalf("state = %q, want unhealthy (loopback без allow_private)", state)
		}
		if int(data.GetFields()["status"].GetNumberValue()) != 0 {
			t.Error("status при заблокированном dial должно быть 0")
		}
	})
}

// insecure_skip_verify:true → self-signed TLS-сервер принят (healthy). Без флага
// тот же cert не проходит верификацию → транспортная ошибка → unhealthy. Здесь
// фабрика прод (util.NewHTTPClient) — проверяем реальный TLS-контур.
func TestHTTPUnhealthyInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Run("insecure_skip_verify:true -> self-signed принят -> healthy", func(t *testing.T) {
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
			t.Fatalf("state = %q, want healthy (self-signed при insecure_skip_verify)", state)
		}
	})

	t.Run("default -> self-signed не доверяется -> unhealthy", func(t *testing.T) {
		b := NewHTTPUnhealthy()
		state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
			"url":           srv.URL + "/health",
			"allow_private": true, // loopback пропущен, изолируем TLS-контур
		}))
		if err != nil {
			t.Fatalf("Check при невалидном TLS не должен падать: %v", err)
		}
		if state != stateHTTPUnhealthy {
			t.Fatalf("state = %q, want unhealthy (self-signed без insecure_skip_verify)", state)
		}
	})
}

// Дефолт (без opt-out-флагов) → нулевой HTTPClientOpts (secure-by-default).
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
		t.Fatalf("default не secure-by-default: %+v", got)
	}
}

// Невалидный тип opt-out-флага (строка вместо bool) → ошибка Check.
func TestHTTPUnhealthyRejectsNonBoolFlag(t *testing.T) {
	for _, flag := range []string{"allow_http", "insecure_skip_verify", "allow_private"} {
		b := newHTTPUnhealthy(&fakeDoer{status: 200})
		if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
			"url": "https://service.internal/healthz",
			flag:  "yes",
		})); err == nil {
			t.Fatalf("ожидали ошибку при %s строкой (тип-чек)", flag)
		}
	}
}
