package obs

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// getMetrics — GET <base>/metrics с опц. basic-auth. Возвращает status + body.
func getMetrics(t *testing.T, base, user, pass string, withAuth bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if withAuth {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func shutdownMetrics(t *testing.T, ms *MetricsServer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ms.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestServeMetrics_NoAuth_Serves(t *testing.T) {
	reg := NewRegistry()
	ms, err := ServeMetrics("127.0.0.1:0", reg, nil)
	if err != nil {
		t.Fatalf("ServeMetrics: %v", err)
	}
	defer shutdownMetrics(t, ms)

	status, body := getMetrics(t, "http://"+ms.Addr(), "", "", false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("body missing go_goroutines core-collector; len=%d", len(body))
	}
}

func TestServeMetrics_BasicAuth_AcceptsRight(t *testing.T) {
	reg := NewRegistry()
	ms, err := ServeMetrics("127.0.0.1:0", reg, &BasicAuth{Username: "scrape", Password: "s3cret"})
	if err != nil {
		t.Fatalf("ServeMetrics: %v", err)
	}
	defer shutdownMetrics(t, ms)

	status, body := getMetrics(t, "http://"+ms.Addr(), "scrape", "s3cret", true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("body missing go_goroutines; len=%d", len(body))
	}
}

func TestServeMetrics_BasicAuth_RejectsWrong(t *testing.T) {
	reg := NewRegistry()
	ms, err := ServeMetrics("127.0.0.1:0", reg, &BasicAuth{Username: "scrape", Password: "s3cret"})
	if err != nil {
		t.Fatalf("ServeMetrics: %v", err)
	}
	defer shutdownMetrics(t, ms)
	base := "http://" + ms.Addr()

	cases := []struct {
		name       string
		user, pass string
		withAuth   bool
	}{
		{"no-credentials", "", "", false},
		{"wrong-password", "scrape", "nope", true},
		{"wrong-username", "intruder", "s3cret", true},
		{"both-wrong", "intruder", "nope", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := getMetrics(t, base, tc.user, tc.pass, tc.withAuth)
			if status != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", status)
			}
		})
	}
}

func TestServeMetrics_NilRegistry_Error(t *testing.T) {
	if _, err := ServeMetrics("127.0.0.1:0", nil, nil); err == nil {
		t.Fatal("ServeMetrics(nil registry) = nil error, want error")
	}
}

func TestServeMetrics_EmptyAddr_Error(t *testing.T) {
	if _, err := ServeMetrics("", NewRegistry(), nil); err == nil {
		t.Fatal("ServeMetrics(empty addr) = nil error, want error")
	}
}

func TestServeMetrics_IncompleteAuth_Error(t *testing.T) {
	for _, a := range []*BasicAuth{
		{Username: "u", Password: ""},
		{Username: "", Password: "p"},
	} {
		if _, err := ServeMetrics("127.0.0.1:0", NewRegistry(), a); err == nil {
			t.Errorf("ServeMetrics(incomplete auth %+v) = nil error, want error", a)
		}
	}
}

func TestMetricsServer_Shutdown_NilSafe(t *testing.T) {
	var ms *MetricsServer
	if err := ms.Shutdown(context.Background()); err != nil {
		t.Errorf("nil Shutdown = %v, want nil", err)
	}
	if addr := ms.Addr(); addr != "" {
		t.Errorf("nil Addr = %q, want empty", addr)
	}
}
