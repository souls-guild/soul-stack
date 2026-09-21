//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// Operator-API HTTP client with JWT auth. Thin wrapper over net/http:
// injects `Authorization: Bearer <JWT>` and serializes the body to JSON.
//
// Does not use `keeper-side` api/openapi-client packages — the harness must
// test the public OpenAPI contract as a black box (otherwise E2E degrades
// into an integration test).

// opHTTPTimeout — deadline for one Operator-API HTTP call. CreateIncarnation
// / RunScenario are async-202 (starting a scenario does not block); there is
// no truly long synchronous contract in the MVP; 30s is a wide margin over
// the usual 50-500ms.
const opHTTPTimeout = 30 * time.Second

// opClient — JWT-authenticated HTTP client against the Operator-API.
type opClient struct {
	baseURL string
	jwt     string
	client  *http.Client
}

// opClient assembles a client with the first Archon's JWT from Stack.JWT.
func (s *Stack) opClient(t *testing.T) *opClient {
	t.Helper()
	if s.JWT == "" {
		t.Fatal("opClient: Stack.JWT is empty (did NewStack skip bootstrap?)")
	}
	return &opClient{
		baseURL: s.KeeperHTTPURL,
		jwt:     s.JWT,
		client:  &http.Client{Timeout: opHTTPTimeout},
	}
}

// post serializes body to JSON, sets `Authorization: Bearer <jwt>` and
// returns (body, statusCode, err).
func (c *opClient) post(ctx context.Context, path string, body any) ([]byte, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	return b, resp.StatusCode, nil
}

// get — GET with JWT, returns body+status. Used by AssertMetricGE
// (via a direct http.Get, bypassing JWT — the metrics listener has no auth
// in the MVP) and by future read endpoints.
func (c *opClient) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	return b, resp.StatusCode, nil
}
