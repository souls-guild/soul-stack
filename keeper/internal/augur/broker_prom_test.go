package augur

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const pubPromEndpoint = "https://prom.example.com:9090"

func TestBrokerPrometheus_OK_InlineData(t *testing.T) {
	doer := &recordingDoer{respBody: `{"status":"success","data":{"resultType":"vector","result":[]}}`}
	kv := staticKV{data: map[string]any{"token": "tkn-123"}}

	s, err := BrokerPrometheus(context.Background(), kv, doer, pubPromEndpoint, "vault:secret/keeper/prom", "up")
	if err != nil {
		t.Fatalf("BrokerPrometheus: %v", err)
	}
	if s.GetFields()["status"].GetStringValue() != "success" {
		t.Errorf("inline_data status not carried: %v", s.AsMap())
	}
	// promQL went out as a query param (not concatenation).
	if got := doer.gotReq.URL.Query().Get("query"); got != "up" {
		t.Errorf("query param = %q, want up", got)
	}
	if !strings.HasSuffix(doer.gotReq.URL.Path, promQueryPath) {
		t.Errorf("path = %q, want suffix %q", doer.gotReq.URL.Path, promQueryPath)
	}
	// credential went out as Bearer.
	if doer.gotAuth != "Bearer tkn-123" {
		t.Errorf("Authorization = %q, want Bearer tkn-123", doer.gotAuth)
	}
}

func TestBrokerPrometheus_BasicAuth(t *testing.T) {
	doer := &recordingDoer{respBody: `{"data":{}}`}
	kv := staticKV{data: map[string]any{"username": "u", "password": "p"}}
	_, err := BrokerPrometheus(context.Background(), kv, doer, pubPromEndpoint, "vault:secret/keeper/prom", "up")
	if err != nil {
		t.Fatalf("BrokerPrometheus: %v", err)
	}
	if !strings.HasPrefix(doer.gotAuth, "Basic ") {
		t.Errorf("Authorization = %q, want Basic", doer.gotAuth)
	}
}

// TestBrokerPrometheus_HTTPEndpointDenied — an http:// endpoint is rejected
// before the request (SSRF/downgrade guard).
func TestBrokerPrometheus_HTTPEndpointDenied(t *testing.T) {
	doer := &recordingDoer{respBody: `{}`}
	kv := staticKV{data: map[string]any{}}
	_, err := BrokerPrometheus(context.Background(), kv, doer, "http://prom.example.com:9090", "vault:secret/keeper/prom", "up")
	if err == nil {
		t.Fatalf("expected denial of http endpoint")
	}
	if doer.gotReq != nil {
		t.Errorf("запрос не должен был уйти при http-endpoint")
	}
}

// TestBrokerPrometheus_MetadataLiteralDenied — a literal metadata IP in
// endpoint is rejected by validateEndpoint before the request.
func TestBrokerPrometheus_MetadataLiteralDenied(t *testing.T) {
	doer := &recordingDoer{respBody: `{}`}
	kv := staticKV{data: map[string]any{}}
	_, err := BrokerPrometheus(context.Background(), kv, doer, "https://169.254.169.254/api/v1/query", "vault:secret/keeper/prom", "up")
	if err == nil {
		t.Fatalf("expected denial of metadata literal IP")
	}
	if doer.gotReq != nil {
		t.Errorf("запрос не должен был уйти при metadata-endpoint")
	}
}

// TestBrokerPrometheus_Non2xx_NoBodyLeak — non-2xx → error without the response body.
func TestBrokerPrometheus_Non2xx_NoBodyLeak(t *testing.T) {
	doer := &recordingDoer{respStatus: http.StatusForbidden, respBody: "secret-internal-detail"}
	kv := staticKV{data: map[string]any{}}
	_, err := BrokerPrometheus(context.Background(), kv, doer, pubPromEndpoint, "vault:secret/keeper/prom", "up")
	if err == nil {
		t.Fatalf("expected error on 403")
	}
	if strings.Contains(err.Error(), "secret-internal-detail") {
		t.Errorf("тело ответа не должно течь в ошибку: %v", err)
	}
}

// TestBrokerPrometheus_CredentialNotInError — a request failure doesn't leak
// the credential into the error text.
func TestBrokerPrometheus_CredentialNotInError(t *testing.T) {
	doer := &recordingDoer{err: errors.New("dial fail")}
	kv := staticKV{data: map[string]any{"token": "super-secret-token"}}
	_, err := BrokerPrometheus(context.Background(), kv, doer, pubPromEndpoint, "vault:secret/keeper/prom", "up")
	if err == nil {
		t.Fatalf("expected request error")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("credential не должен течь в ошибку: %v", err)
	}
}

func TestBuildPromURL(t *testing.T) {
	got, err := buildPromURL("https://prom.example.com:9090/", `sum(rate(http_requests_total[5m]))`)
	if err != nil {
		t.Fatalf("buildPromURL: %v", err)
	}
	if !strings.Contains(got, "/api/v1/query?query=") {
		t.Errorf("url = %q", got)
	}
	if strings.Contains(got, "//api/v1/query") {
		t.Errorf("trailing slash endpoint не нормализован: %q", got)
	}
}
