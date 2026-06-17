package vault

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

// newTestClient поднимает httptest.Server, имитирующий Vault HTTP API
// (handler-функция отвечает на любой path), и оборачивает его в наш
// *Client с фиксированным kvMount.
//
// handler — функция, принимающая w/r — она интерпретирует path как
// `/v1/<vault-path>` и решает, что вернуть.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = srv.URL
	api, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		t.Fatalf("vaultapi.NewClient: %v", err)
	}
	api.SetToken("dev-root")
	return &Client{c: api, kvMount: "secret"}, srv
}

func TestSignCSR_HappyPath(t *testing.T) {
	const (
		mount   = "pki"
		role    = "soul-seed"
		fakeCSR = "-----BEGIN CERTIFICATE REQUEST-----\nAAAA\n-----END CERTIFICATE REQUEST-----"
	)
	var captured struct {
		path string
		body map[string]any
	}
	expectedExp := time.Now().Add(720 * time.Hour).Unix()

	cl, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured.body)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id": "req-1",
			"data": map[string]any{
				"certificate":   "CERT-PEM",
				"issuing_ca":    "CA-ROOT-PEM\n",
				"ca_chain":      []any{"CA-INT-PEM"},
				"serial_number": "01:02:03",
				"expiration":    expectedExp,
			},
		})
	})
	defer srv.Close()

	res, err := cl.SignCSR(context.Background(), mount, role, fakeCSR)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}

	if captured.path != "/v1/pki/sign/soul-seed" {
		t.Errorf("path = %q, want /v1/pki/sign/soul-seed", captured.path)
	}
	if got, want := captured.body["csr"], fakeCSR; got != want {
		t.Errorf("body.csr = %q, want %q", got, want)
	}
	if got := captured.body["format"]; got != "pem" {
		t.Errorf("body.format = %v, want pem", got)
	}

	if string(res.CertificatePEM) != "CERT-PEM" {
		t.Errorf("CertificatePEM = %q, want CERT-PEM", res.CertificatePEM)
	}
	if !strings.Contains(string(res.CAChainPEM), "CA-ROOT-PEM") {
		t.Errorf("CAChainPEM missing CA-ROOT-PEM: %q", res.CAChainPEM)
	}
	if !strings.Contains(string(res.CAChainPEM), "CA-INT-PEM") {
		t.Errorf("CAChainPEM missing CA-INT-PEM: %q", res.CAChainPEM)
	}
	if res.SerialNumber != "01:02:03" {
		t.Errorf("SerialNumber = %q", res.SerialNumber)
	}
	if !res.NotAfter.Equal(time.Unix(expectedExp, 0).UTC()) {
		t.Errorf("NotAfter = %v, want unix %d", res.NotAfter, expectedExp)
	}
}

func TestSignCSR_TrimsMountTrailingSlash(t *testing.T) {
	var capturedPath string
	cl, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"certificate":   "C",
				"serial_number": "S",
			},
		})
	})
	defer srv.Close()

	_, err := cl.SignCSR(context.Background(), "pki/", "soul-seed", "csr-pem")
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}
	if capturedPath != "/v1/pki/sign/soul-seed" {
		t.Errorf("path = %q (trailing slash not trimmed)", capturedPath)
	}
}

func TestSignCSR_EmptyValidation(t *testing.T) {
	cl, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})
	defer srv.Close()

	cases := []struct {
		name             string
		mount, role, csr string
		want             error
	}{
		{"empty mount", "", "soul-seed", "csr", ErrPKIMountEmpty},
		{"empty role", "pki", "", "csr", ErrPKIRoleEmpty},
		{"empty csr", "pki", "soul-seed", "", ErrPKICSREmpty},
		{"whitespace csr", "pki", "soul-seed", "   \n  ", ErrPKICSREmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cl.SignCSR(context.Background(), tc.mount, tc.role, tc.csr)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is(%v)", err, tc.want)
			}
		})
	}
}

func TestSignCSR_MissingCertificate(t *testing.T) {
	cl, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"serial_number": "01",
			},
		})
	})
	defer srv.Close()

	_, err := cl.SignCSR(context.Background(), "pki", "soul-seed", "csr-pem")
	if !errors.Is(err, ErrPKIResponseInvalid) {
		t.Errorf("err = %v, want ErrPKIResponseInvalid", err)
	}
}

func TestSignCSR_MissingSerialNumber(t *testing.T) {
	cl, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"certificate": "C",
			},
		})
	})
	defer srv.Close()

	_, err := cl.SignCSR(context.Background(), "pki", "soul-seed", "csr-pem")
	if !errors.Is(err, ErrPKIResponseInvalid) {
		t.Errorf("err = %v, want ErrPKIResponseInvalid", err)
	}
}

func TestSignCSR_VaultHTTPError(t *testing.T) {
	cl, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":["role not found"]}`, http.StatusNotFound)
	})
	defer srv.Close()

	_, err := cl.SignCSR(context.Background(), "pki", "soul-seed", "csr-pem")
	if err == nil {
		t.Fatal("SignCSR: nil err, want HTTP error")
	}
	// Errors из vaultapi оборачиваются нашим fmt.Errorf — sentinel-теста
	// здесь нет, проверяем только что err != nil.
}

func TestCoerceExpiration_Variants(t *testing.T) {
	now := int64(1700000000)
	want := time.Unix(now, 0).UTC()

	type jsonNumberStub struct{ n int64 }
	// json.Number имитируем через Stringer + Int64() — но coerceExpiration
	// проверяет interface{ Int64() (int64, error) }, поэтому подсовываем
	// прямой json.Number от encoding/json.
	cases := []struct {
		name string
		in   any
	}{
		{"int", int(now)},
		{"int64", int64(now)},
		{"float64", float64(now)},
		{"json.Number", json.Number("1700000000")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Helper()
			got, err := coerceExpiration(tc.in)
			if err != nil {
				t.Fatalf("coerceExpiration(%v): %v", tc.in, err)
			}
			if !got.Equal(want) {
				t.Errorf("got=%v, want=%v", got, want)
			}
		})
	}
	_ = jsonNumberStub{}
}

func TestCoerceExpiration_Error(t *testing.T) {
	if _, err := coerceExpiration(nil); err == nil {
		t.Error("coerceExpiration(nil): nil err, want error")
	}
	if _, err := coerceExpiration([]int{1, 2}); err == nil {
		t.Error("coerceExpiration(slice): nil err, want error")
	}
	if _, err := coerceExpiration("not-a-time"); err == nil {
		t.Error("coerceExpiration(bad string): nil err, want error")
	}
}
