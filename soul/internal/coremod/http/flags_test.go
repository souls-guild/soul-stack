package http_test

import (
	"context"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httpmod "github.com/souls-guild/soul-stack/soul/internal/coremod/http"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// warningsOf извлекает список warnings из output последнего события (или nil,
// если поля нет).
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

// --- allow_http: Validate ---

// По умолчанию http:// отвергается; с allow_http:true — принимается.
func TestValidate_AllowHTTP_AcceptsHTTP(t *testing.T) {
	m := httpmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":        "http://example.com/health",
			"allow_http": true,
		}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok=false для http:// при allow_http:true: %v", reply.Errors)
	}
}

// allow_http НЕ открывает file:// (ослабляет только https-only, не любую схему).
func TestValidate_AllowHTTP_StillRejectsFileScheme(t *testing.T) {
	m := httpmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":        "file:///etc/passwd",
			"allow_http": true,
		}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для file:// даже с allow_http:true")
	}
}

func TestValidate_RejectsNonBoolFlags(t *testing.T) {
	for _, flag := range []string{"allow_http", "insecure_skip_verify"} {
		m := httpmod.New()
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "probe",
			Params: mustStruct(t, map[string]any{
				"url": "https://example.com/health",
				flag:  "yes",
			}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true для %s строкой (ожидался тип-чек)", flag)
		}
	}
}

// --- allow_http: Apply ---

// allow_http:true → http://-URL принят в Apply (вызов реально дошёл до клиента).
func TestApply_AllowHTTP_HTTPAccepted(t *testing.T) {
	d := &fakeDoer{body: []byte("ok"), status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":        "http://example.com/health",
			"allow_http": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true для http:// при allow_http:true: %s", stream.Last().Message)
	}
	if d.calls != 1 {
		t.Fatalf("клиент вызван %d раз (ожидалось 1)", d.calls)
	}
}

// allow_http:true → AllowHTTPRedirect доезжает до фабрики (downgrade-hop парно),
// прочие контуры не задеты (ортогональность).
func TestApply_AllowHTTP_PropagatesRedirectOpt(t *testing.T) {
	var got util.HTTPClientOpts
	d := &fakeDoer{status: 200}
	m := newModuleCapturing(d, &got)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":        "http://example.com/health",
			"allow_http": true,
		}),
	}, stream)
	if !got.AllowHTTPRedirect {
		t.Fatal("AllowHTTPRedirect=false при allow_http:true")
	}
	if got.AllowPrivate || got.InsecureSkipVerify {
		t.Fatalf("allow_http задел чужой контур: %+v", got)
	}
}

// --- insecure_skip_verify: Apply opts + реальная TLS-проверка ---

func TestApply_InsecureSkipVerify_PropagatesToClientOpts(t *testing.T) {
	var got util.HTTPClientOpts
	d := &fakeDoer{status: 200}
	m := newModuleCapturing(d, &got)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":                  "https://example.com/health",
			"insecure_skip_verify": true,
		}),
	}, stream)
	if !got.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify=false при insecure_skip_verify:true")
	}
	if got.AllowPrivate || got.AllowHTTPRedirect {
		t.Fatalf("insecure_skip_verify задел чужой контур: %+v", got)
	}
}

// Сквозной TLS-тест: probe к httptest-TLS-серверу с самоподписанным cert. Без
// insecure_skip_verify — клиент через настоящую фабрику util.NewHTTPClient НЕ
// доверяет cert → failed. С insecure_skip_verify:true — TLS не верифицируется →
// ответ принят. Здесь фабрика НЕ подменяется (проверяем прод-путь построения).
func TestApply_InsecureSkipVerify_EndToEndTLS(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer tlsSrv.Close()
	// allow_private:true нужен, т.к. httptest слушает на 127.0.0.1 (SSRF-guard
	// иначе заблокирует dial). Это изолирует именно TLS-контур.
	base := map[string]any{
		"url":           tlsSrv.URL + "/health",
		"allow_private": true,
	}

	t.Run("без insecure_skip_verify -> TLS не доверяет -> failed", func(t *testing.T) {
		m := httpmod.New() // настоящая фабрика util.NewHTTPClient
		stream := &internaltest.ApplyStream{}
		_ = m.Apply(&pluginv1.ApplyRequest{
			State:  "probe",
			Params: mustStruct(t, base),
		}, stream)
		if !stream.Last().Failed {
			t.Fatal("failed=false для самоподписанного TLS без insecure_skip_verify")
		}
	})

	t.Run("insecure_skip_verify:true -> TLS не верифицируется -> ok", func(t *testing.T) {
		params := map[string]any{}
		for k, v := range base {
			params[k] = v
		}
		params["insecure_skip_verify"] = true
		m := httpmod.New()
		stream := &internaltest.ApplyStream{}
		if err := m.Apply(&pluginv1.ApplyRequest{
			State:  "probe",
			Params: mustStruct(t, params),
		}, stream); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		ev := stream.Last()
		if ev.Failed {
			t.Fatalf("failed=true с insecure_skip_verify на самоподписанном TLS: %s", ev.Message)
		}
		if ev.Output.Fields["status"].GetNumberValue() != 200 {
			t.Fatalf("status=%v want 200", ev.Output.Fields["status"].GetNumberValue())
		}
	})
}

// TestApply_OptOutTruthTable — полная истинностная таблица 2³=8 комбинаций
// (allow_http × insecure_skip_verify × allow_private) → ожидаемый
// util.HTTPClientOpts, фактически переданный в захватывающую фабрику. Регресс-гард
// маппинга param→opts (allow_http → AllowHTTPRedirect, остальные именные).
// URL всегда https:// (валиден при любом allow_http), чтобы каждая комбинация
// доходила до построения клиента.
func TestApply_OptOutTruthTable(t *testing.T) {
	for i := 0; i < 8; i++ {
		allowHTTP := i&1 != 0
		insecure := i&2 != 0
		allowPrivate := i&4 != 0
		name := fmt.Sprintf("allow_http=%v/insecure=%v/allow_private=%v", allowHTTP, insecure, allowPrivate)
		t.Run(name, func(t *testing.T) {
			var got util.HTTPClientOpts
			d := &fakeDoer{status: 200}
			m := newModuleCapturing(d, &got)
			stream := &internaltest.ApplyStream{}
			if err := m.Apply(&pluginv1.ApplyRequest{
				State: "probe",
				Params: mustStruct(t, map[string]any{
					"url":                  "https://example.com/health",
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

// --- комбинации флагов: все три ортогонально доезжают ---

func TestApply_AllFlags_PropagateOrthogonally(t *testing.T) {
	var got util.HTTPClientOpts
	d := &fakeDoer{status: 200}
	m := newModuleCapturing(d, &got)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":                  "http://internal.svc/health",
			"allow_private":        true,
			"allow_http":           true,
			"insecure_skip_verify": true,
		}),
	}, stream)
	if !got.AllowPrivate || !got.AllowHTTPRedirect || !got.InsecureSkipVerify {
		t.Fatalf("не все флаги доехали до фабрики: %+v", got)
	}
}

// Дефолт (без флагов) → все контуры взведены (нулевой HTTPClientOpts).
func TestApply_NoFlags_SecureByDefault(t *testing.T) {
	var got util.HTTPClientOpts
	d := &fakeDoer{status: 200}
	m := newModuleCapturing(d, &got)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/health"}),
	}, stream)
	if got.AllowPrivate || got.AllowHTTPRedirect || got.InsecureSkipVerify {
		t.Fatalf("default не secure-by-default: %+v", got)
	}
}

// --- warnings при снятии guard ---

// Без флагов — никаких warnings (чистый output).
func TestApply_NoFlags_NoWarnings(t *testing.T) {
	d := &fakeDoer{status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "probe",
		Params: mustStruct(t, map[string]any{"url": "https://example.com/health"}),
	}, stream)
	if w := warningsOf(stream.Last()); len(w) != 0 {
		t.Fatalf("warnings без снятых guard-ов: %v", w)
	}
}

func TestApply_GuardWarnings(t *testing.T) {
	cases := []struct {
		name     string
		params   map[string]any
		wantSub  string
		wantHost string
	}{
		{
			name:     "insecure_skip_verify",
			params:   map[string]any{"url": "https://svc.internal:8443/h", "insecure_skip_verify": true},
			wantSub:  "TLS verification disabled (insecure_skip_verify)",
			wantHost: "svc.internal:8443",
		},
		{
			name:     "allow_http",
			params:   map[string]any{"url": "http://svc.internal/h", "allow_http": true},
			wantSub:  "plaintext http allowed (allow_http)",
			wantHost: "svc.internal",
		},
		{
			name:     "allow_private",
			params:   map[string]any{"url": "https://10.0.0.5/h", "allow_private": true},
			wantSub:  "SSRF-guard disabled (allow_private)",
			wantHost: "10.0.0.5",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeDoer{status: 200}
			m := newModule(d)
			stream := &internaltest.ApplyStream{}
			_ = m.Apply(&pluginv1.ApplyRequest{
				State:  "probe",
				Params: mustStruct(t, c.params),
			}, stream)
			ws := warningsOf(stream.Last())
			if !anyWarningContains(ws, c.wantSub) {
				t.Fatalf("нет warning %q в %v", c.wantSub, ws)
			}
			if !anyWarningContains(ws, c.wantHost) {
				t.Fatalf("host %q не в warning %v", c.wantHost, ws)
			}
		})
	}
}

// Warning несёт ТОЛЬКО host: ни query/path URL, ни значения headers не светятся.
func TestApply_GuardWarning_NoURLPathNoHeaders(t *testing.T) {
	d := &fakeDoer{status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":           "https://svc.internal/secret-path?token=leak123",
			"allow_private": true,
			"headers": map[string]any{
				"Authorization": "Bearer super-secret-token",
			},
		}),
	}, stream)
	ws := warningsOf(stream.Last())
	if len(ws) == 0 {
		t.Fatal("нет warnings при allow_private:true")
	}
	for _, w := range ws {
		if strings.Contains(w, "secret-path") || strings.Contains(w, "leak123") {
			t.Fatalf("URL path/query просочились в warning: %q", w)
		}
		if strings.Contains(w, "super-secret-token") || strings.Contains(w, "Authorization") {
			t.Fatalf("headers просочились в warning: %q", w)
		}
	}
}

// Несколько снятых guard-ов → несколько warnings разом.
func TestApply_GuardWarnings_Multiple(t *testing.T) {
	d := &fakeDoer{status: 200}
	m := newModule(d)
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "probe",
		Params: mustStruct(t, map[string]any{
			"url":                  "http://svc.internal/h",
			"allow_private":        true,
			"allow_http":           true,
			"insecure_skip_verify": true,
		}),
	}, stream)
	ws := warningsOf(stream.Last())
	if len(ws) != 3 {
		t.Fatalf("ожидалось 3 warning-а, получено %d: %v", len(ws), ws)
	}
}
