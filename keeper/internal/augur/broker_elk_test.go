package augur

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const pubELKEndpoint = "https://elk.example.com:9200"

func TestBrokerELK_OK_InlineData(t *testing.T) {
	doer := &recordingDoer{respBody: `{"took":3,"hits":{"total":{"value":0},"hits":[]}}`}
	kv := staticKV{data: map[string]any{"api_key": "ak-xyz"}}

	s, err := BrokerELK(context.Background(), kv, doer, pubELKEndpoint, "vault:secret/keeper/elk", "logs-app")
	if err != nil {
		t.Fatalf("BrokerELK: %v", err)
	}
	if int(s.GetFields()["took"].GetNumberValue()) != 3 {
		t.Errorf("inline_data took not carried: %v", s.AsMap())
	}
	if !strings.HasSuffix(doer.gotReq.URL.Path, "/logs-app/_search") {
		t.Errorf("path = %q, want suffix /logs-app/_search", doer.gotReq.URL.Path)
	}
	if doer.gotAuth != "ApiKey ak-xyz" {
		t.Errorf("Authorization = %q, want ApiKey ak-xyz", doer.gotAuth)
	}
}

func TestBrokerELK_HTTPEndpointDenied(t *testing.T) {
	doer := &recordingDoer{respBody: `{}`}
	kv := staticKV{data: map[string]any{}}
	_, err := BrokerELK(context.Background(), kv, doer, "http://elk.example.com:9200", "vault:secret/keeper/elk", "logs-app")
	if err == nil {
		t.Fatalf("expected denial of http endpoint")
	}
	if doer.gotReq != nil {
		t.Errorf("request should not have been sent for http-endpoint")
	}
}

func TestBrokerELK_LoopbackLiteralDenied(t *testing.T) {
	doer := &recordingDoer{respBody: `{}`}
	kv := staticKV{data: map[string]any{}}
	_, err := BrokerELK(context.Background(), kv, doer, "https://127.0.0.1:9200", "vault:secret/keeper/elk", "logs-app")
	if err == nil {
		t.Fatalf("expected denial of loopback literal IP")
	}
	if doer.gotReq != nil {
		t.Errorf("request should not have been sent for loopback-endpoint")
	}
}

func TestBrokerELK_Non2xx_NoBodyLeak(t *testing.T) {
	doer := &recordingDoer{respStatus: http.StatusUnauthorized, respBody: "index-internal-detail"}
	kv := staticKV{data: map[string]any{}}
	_, err := BrokerELK(context.Background(), kv, doer, pubELKEndpoint, "vault:secret/keeper/elk", "logs-app")
	if err == nil {
		t.Fatalf("expected error on 401")
	}
	if strings.Contains(err.Error(), "index-internal-detail") {
		t.Errorf("response body should not leak into error: %v", err)
	}
}

func TestBuildELKURL_PathEscape(t *testing.T) {
	got, err := buildELKURL("https://elk.example.com:9200/", "logs-2026.05")
	if err != nil {
		t.Fatalf("buildELKURL: %v", err)
	}
	if !strings.HasSuffix(got, "/logs-2026.05/_search") {
		t.Errorf("url = %q", got)
	}
	if strings.Contains(got, "//logs") {
		t.Errorf("trailing slash endpoint not normalized: %q", got)
	}
}

// TestBuildELKURL_NoPathInjection — slashes in index are escaped (can't reach
// the admin API via `../`).
func TestBuildELKURL_NoPathInjection(t *testing.T) {
	got, err := buildELKURL("https://elk.example.com:9200", "../_cluster/health")
	if err != nil {
		t.Fatalf("buildELKURL: %v", err)
	}
	if strings.Contains(got, "/_cluster/health/_search") && !strings.Contains(got, "%2F") {
		t.Errorf("index not escaped, path injection possible: %q", got)
	}
}
