// Package client is a thin HTTP client for Keeper's Operator API.
//
// The API contract is docs/keeper/operator-api.md + docs/keeper/openapi.yaml.
// This package holds only the transport + typed `Incarnations`/`Souls`
// wrappers for soulctl commands. Business logic (formatting, validation)
// lives in the cmd package.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/soulctl/internal/config"
)

// HTTPDoer is a minimal interface over http.Client for swapping in tests
// (httptest.NewServer returns a real *http.Client; a mock Doer is more
// useful for soulctl command unit tests).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client wraps HTTPDoer with Keeper's base URL and a JWT bearer header.
type Client struct {
	baseURL string
	jwt     string
	http    HTTPDoer

	Incarnations  *IncarnationsAPI
	Souls         *SoulsAPI
	Archon        *ArchonAPI
	Errand        *ErrandAPI
	Voyages       *VoyagesAPI
	PushProviders *PushProvidersAPI
	Push          *PushAPI
}

// New builds a client from credentials. The base URL is normalized (trailing
// slash stripped). Returns a meaningful error if credentials are incomplete.
func New(c *config.Credentials) (*Client, error) {
	if c == nil || c.KeeperURL == "" || c.ArchonJWT == "" {
		return nil, errors.New("credentials пусты")
	}
	u, err := url.Parse(c.KeeperURL)
	if err != nil {
		return nil, fmt.Errorf("разобрать keeper_url %q: %w", c.KeeperURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("keeper_url %q должен быть полным URL (scheme + host)", c.KeeperURL)
	}
	cl := &Client{
		baseURL: strings.TrimRight(c.KeeperURL, "/"),
		jwt:     c.ArchonJWT,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	cl.bindAPIs()
	return cl, nil
}

// NewWithDoer is for tests: lets you swap the transport (httptest or mock).
// Repeats URL/JWT validation so behavior matches New.
func NewWithDoer(baseURL, jwt string, doer HTTPDoer) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("baseURL пуст")
	}
	cl := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		jwt:     jwt,
		http:    doer,
	}
	cl.bindAPIs()
	return cl, nil
}

func (c *Client) bindAPIs() {
	c.Incarnations = &IncarnationsAPI{c: c}
	c.Souls = &SoulsAPI{c: c}
	c.Archon = &ArchonAPI{c: c}
	c.Errand = &ErrandAPI{c: c}
	c.Voyages = &VoyagesAPI{c: c}
	c.PushProviders = &PushProvidersAPI{c: c}
	c.Push = &PushAPI{c: c}
}

// APIError is a structured error for Keeper's HTTP response. Carries the
// HTTP status and the type/title/detail from RFC 7807 ProblemDetails (if the
// response is application/problem+json).
type APIError struct {
	Status  int    `json:"status"`
	Type    string `json:"type,omitempty"`
	Title   string `json:"title,omitempty"`
	Detail  string `json:"detail,omitempty"`
	RawBody string `json:"-"`
	Method  string `json:"-"`
	Path    string `json:"-"`
}

// Error returns a human-readable representation; commands on top format
// 401/403/404 into standard hints (see internal/cmd/errors.go).
func (e *APIError) Error() string {
	switch {
	case e.Detail != "":
		return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, e.Title, e.Detail)
	case e.Title != "":
		return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, e.Title)
	case e.RawBody != "":
		return fmt.Sprintf("%s %s: %d: %s", e.Method, e.Path, e.Status, e.RawBody)
	default:
		return fmt.Sprintf("%s %s: %d", e.Method, e.Path, e.Status)
	}
}

// AsAPIError is a convenience switch for the cmd layer: errors.As + status check.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// Do performs an HTTP request with authorization and JSON-serializes body
// (if not nil). On HTTP status ≥400 it returns *APIError (RFC 7807 parsed
// when possible).
//
// `out` is a pointer to a struct for decoding the JSON response; nil means
// the response is ignored. If the body is empty (204 No Content) and
// out != nil, there's no error — out stays zero-value.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("сериализовать запрос: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("собрать запрос: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	req.Header.Set("Accept", "application/json, application/problem+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		apiErr := &APIError{
			Status:  resp.StatusCode,
			RawBody: strings.TrimSpace(string(payload)),
			Method:  method,
			Path:    path,
		}
		// RFC 7807 — application/problem+json. If parsing fails, keep RawBody.
		_ = json.Unmarshal(payload, apiErr)
		return apiErr
	}
	if out != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("разобрать ответ %s %s: %w", method, path, err)
		}
	}
	return nil
}

// JWT returns the client's current JWT (for archon whoami — decoding claims).
func (c *Client) JWT() string { return c.jwt }

// BaseURL returns the base URL (for diagnostics in whoami).
func (c *Client) BaseURL() string { return c.baseURL }
