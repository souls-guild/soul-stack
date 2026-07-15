package augur

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWrapInlineData_Object(t *testing.T) {
	s, err := wrapInlineData(map[string]any{"a": "b", "n": float64(2)})
	if err != nil {
		t.Fatalf("wrapInlineData: %v", err)
	}
	if s.GetFields()["a"].GetStringValue() != "b" {
		t.Errorf("object not carried as-is: %v", s.AsMap())
	}
}

func TestWrapInlineData_ArrayWrappedInValue(t *testing.T) {
	s, err := wrapInlineData([]any{"x", "y"})
	if err != nil {
		t.Fatalf("wrapInlineData: %v", err)
	}
	lv := s.GetFields()["value"].GetListValue()
	if lv == nil || len(lv.GetValues()) != 2 {
		t.Errorf("array должен лечь в ключ value: %v", s.AsMap())
	}
}

func TestWrapInlineData_ScalarWrappedInValue(t *testing.T) {
	s, err := wrapInlineData(float64(42))
	if err != nil {
		t.Fatalf("wrapInlineData: %v", err)
	}
	if s.GetFields()["value"].GetNumberValue() != 42 {
		t.Errorf("скаляр должен лечь в ключ value: %v", s.AsMap())
	}
}

func TestCredentialFromKV(t *testing.T) {
	c := credentialFromKV(map[string]any{"token": "t"})
	if c.bearer != "t" {
		t.Errorf("token→bearer failed: %+v", c)
	}
	c = credentialFromKV(map[string]any{"bearer_token": "bt"})
	if c.bearer != "bt" {
		t.Errorf("bearer_token→bearer failed: %+v", c)
	}
	c = credentialFromKV(map[string]any{"api_key": "ak"})
	if c.apiKey != "ak" {
		t.Errorf("api_key failed: %+v", c)
	}
	c = credentialFromKV(map[string]any{"username": "u", "password": "p"})
	if c.username != "u" || c.password != "p" {
		t.Errorf("basic failed: %+v", c)
	}
	// non-string values are ignored
	c = credentialFromKV(map[string]any{"token": 123})
	if c.bearer != "" {
		t.Errorf("нестроковый token не должен попасть в bearer: %+v", c)
	}
}

func TestResolveCredential_InvalidAuthRef(t *testing.T) {
	_, err := resolveCredential(context.Background(), staticKV{}, "not-a-vault-ref")
	if err == nil {
		t.Fatalf("expected invalid auth_ref error")
	}
}

func TestResolveCredential_Empty(t *testing.T) {
	c, err := resolveCredential(context.Background(), staticKV{}, "")
	if err != nil {
		t.Fatalf("empty auth_ref should be no-auth, got %v", err)
	}
	if c.bearer != "" || c.apiKey != "" || c.username != "" {
		t.Errorf("empty auth_ref → empty credential, got %+v", c)
	}
}

// limitDoer returns a body larger than the limit to test the io.LimitReader cutoff.
type limitDoer struct{ body string }

func (d limitDoer) Do(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(d.body)),
		Header:     make(http.Header),
	}, nil
}

func TestDoJSONStruct_BodyLimit(t *testing.T) {
	big := "[" + strings.Repeat("0,", maxResponseBytes/2) + "0]"
	req, _ := http.NewRequest(http.MethodGet, "https://x.example.com", nil)
	_, err := doJSONStruct(limitDoer{body: big}, req)
	if err == nil {
		t.Fatalf("expected body-limit error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("ожидалась limit-ошибка, got %v", err)
	}
}

func TestDoJSONStruct_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://x.example.com", nil)
	_, err := doJSONStruct(limitDoer{body: "not json"}, req)
	if err == nil {
		t.Fatalf("expected invalid-json error")
	}
}
