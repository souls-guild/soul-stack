package augur

// Shared HTTP layer for the prom/elk brokers (delegate=false, augur.md §2.1):
// issuing a request to an Omen's UNtrusted endpoint through the SSRF-guarded
// client (egress.go), injecting the credential read from Vault via
// Omen.AuthRef, and reading a size-limited JSON body. The external credential
// never reaches Soul — data flows through Keeper into AugurReply.inline_data.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// HTTPDoer — the minimal http-client surface the prom/elk brokers need.
// Broker-level parameter for testability: unit tests swap in a guarded client
// hitting an httptest server, no real network. Prod uses [NewEgressClient]
// (SSRF-guarded). The grpc handler holds one instance and passes it to the
// brokers.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// credential — authorization material for an external system, parsed out of
// a Vault secret. Exactly one variant carries a value (or all empty → no
// auth). Never logged and never reaches Soul.
type credential struct {
	bearer   string // Bearer token (Prometheus / Grafana-style)
	apiKey   string // Elasticsearch API key (header `Authorization: ApiKey <...>`)
	username string // Basic-auth
	password string
}

// apply attaches the credential to a request by convention (brokered
// read-only access). Priority: bearer → apiKey → basic. Multiple fields set
// at once is an invalid secret, but a deterministic pick is safer than
// silently mixing them.
func (c credential) apply(req *http.Request) {
	switch {
	case c.bearer != "":
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	case c.apiKey != "":
		req.Header.Set("Authorization", "ApiKey "+c.apiKey)
	case c.username != "":
		enc := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
		req.Header.Set("Authorization", "Basic "+enc)
	}
}

// resolveCredential reads the root credential (the external system's
// account) from Vault via Omen.AuthRef and maps the KV fields into
// [credential] by convention:
//
//	token / bearer_token → Bearer
//	api_key              → ApiKey (ELK)
//	username + password  → Basic
//
// auth_ref is always a vault-ref (invariant augur.md §4.1; CRUD requires
// ValidAuthRef). Empty isn't expected, but is treated as "no auth"
// (best-effort for external systems without authorization). Credential
// values never land in errors/logs — only the vault-path (not a secret).
func resolveCredential(ctx context.Context, kv KVReader, authRef string) (credential, error) {
	if authRef == "" {
		return credential{}, nil
	}
	path, err := vault.ParseRef(authRef)
	if err != nil {
		return credential{}, fmt.Errorf("augur: omen auth_ref invalid: %w", err)
	}
	data, err := kv.ReadKV(ctx, path)
	if err != nil {
		return credential{}, fmt.Errorf("augur: read omen credential %q: %w", path, err)
	}
	return credentialFromKV(data), nil
}

// credentialFromKV extracts the auth fields from a KV secret. Unknown fields
// are ignored. Values are taken only if they're strings.
func credentialFromKV(data map[string]any) credential {
	var c credential
	c.bearer = kvString(data, "token")
	if c.bearer == "" {
		c.bearer = kvString(data, "bearer_token")
	}
	c.apiKey = kvString(data, "api_key")
	c.username = kvString(data, "username")
	c.password = kvString(data, "password")
	return c
}

func kvString(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

// doJSONStruct issues req through the guarded doer, reads a size-limited
// body, decodes it as JSON, and wraps it in structpb.Struct for inline_data.
//
// The body is read through io.LimitReader at maxResponseBytes (DoS
// protection: an UNtrusted endpoint doesn't get to allocate arbitrarily).
// Non-2xx → error WITHOUT the external system's body in the text (the body
// may carry a leak / reflected input) — only the status code. An SSRF/dial
// network failure arrives as the doer's own error (the guard fired in
// DialContext).
func doJSONStruct(doer HTTPDoer, req *http.Request) (*structpb.Struct, error) {
	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("augur: http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("augur: upstream returned status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("augur: read upstream body: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("augur: upstream body exceeds %d bytes limit", int64(maxResponseBytes))
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("augur: upstream body is not valid JSON: %w", err)
	}
	return wrapInlineData(decoded)
}

// wrapInlineData wraps a decoded JSON result in Struct per the shape
// convention (augur.md §5.3): an object (map) carries through as-is; an
// array / scalar goes into a single `value` key (Struct can't carry a
// non-object at the top level).
func wrapInlineData(v any) (*structpb.Struct, error) {
	if m, ok := v.(map[string]any); ok {
		s, err := structpb.NewStruct(m)
		if err != nil {
			return nil, fmt.Errorf("augur: encode upstream object: %w", err)
		}
		return s, nil
	}
	s, err := structpb.NewStruct(map[string]any{"value": v})
	if err != nil {
		return nil, fmt.Errorf("augur: encode upstream value: %w", err)
	}
	return s, nil
}
