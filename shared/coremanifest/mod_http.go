package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modHTTP declares core.http.
var modHTTP = schema.Module{
	Name:         "http",
	Capabilities: []schema.Capability{schema.NetworkOutbound},
	States: map[string]schema.State{
		"probe": {
			Description: "Read-probe of an HTTP endpoint (GET/HEAD); read-only, changed is always false. ADR-015/ADR-016, https-only.",
			Input: schema.Input{
				"allow_http":    {Type: schema.Bool, Description: "Allow http:// (lift https-only); file:// remains forbidden. Does not open up SSRF (default false)."},
				"allow_private": {Type: schema.Bool, Description: "Lift the SSRF-guard for internal health-checks (default false)."},
				// headers is sensitive-by-construction (ADR-010 §7.4): values are not logged,
				// only the list of keys is returned in output. The secret DSL flag is not set (it
				// forces a `^vault:.*` ref, but headers is an arbitrary map). Masking is a runtime aspect.
				"headers":              {Type: schema.Map, Items: &schema.Param{Type: schema.String}, Description: "HTTP headers (values are secret, excluded from output)."},
				"insecure_skip_verify": {Type: schema.Bool, Description: "Disable TLS verification (self-signed / internal CA); MITM risk (default false)."},
				"method":               {Type: schema.String, Default: "GET", Enum: []any{"GET", "HEAD"}, Description: "HTTP method GET|HEAD (default GET); mutating methods are forbidden."},
				"status_codes":         {Type: schema.List, Items: &schema.Param{Type: schema.Int}, Description: "Expected response codes (list of int); defaults to 2xx."},
				"timeout":              {Type: schema.String, Format: "duration", Description: "Probe timeout (Soul Stack duration, e.g. \"30s\")."},
				"url":                  {Type: schema.String, Required: true, Description: "Target URL (https:// only; SSRF-guard on the resolved IP)."},
			},
		},
		"request": {
			Description: "One mutating HTTP API request (POST/PUT/PATCH/DELETE); success reports changed=true. ADR-015/ADR-016, https-only.",
			Input: schema.Input{
				"allow_http":    {Type: schema.Bool, Description: "Allow http:// (lift https-only); file:// remains forbidden. Does not open up SSRF (default false)."},
				"allow_private": {Type: schema.Bool, Description: "Lift the SSRF-guard for an explicitly trusted internal API (default false)."},
				"body":          {Type: schema.String, Description: "Optional request body. The response body, not this request body, is returned in output.body."},
				"content_type":  {Type: schema.String, Description: "Optional Content-Type header; when set, overrides headers[Content-Type]."},
				// Header values are sensitive-by-construction, including X-Consul-Token:
				// they are sent to the endpoint but never returned or logged. Secret cannot
				// be set here because schema.Secret requires each value to be a vault-ref.
				"headers":              {Type: schema.Map, Items: &schema.Param{Type: schema.String}, Description: "HTTP headers (all values are sensitive-by-construction; output contains keys only)."},
				"insecure_skip_verify": {Type: schema.Bool, Description: "Disable TLS verification (self-signed / internal CA); MITM risk (default false)."},
				"method":               {Type: schema.String, Required: true, Enum: []any{"POST", "PUT", "PATCH", "DELETE"}, Description: "Explicit mutating HTTP method POST|PUT|PATCH|DELETE; GET/HEAD are forbidden."},
				"status_codes":         {Type: schema.List, Items: &schema.Param{Type: schema.Int}, Description: "Expected response codes (list of int); defaults to [200]."},
				"timeout":              {Type: schema.String, Format: "duration", Description: "Request timeout (Soul Stack duration, e.g. \"30s\")."},
				"url":                  {Type: schema.String, Required: true, Description: "Target URL (https:// only; SSRF-guard on the resolved IP)."},
			},
		},
	},
}
