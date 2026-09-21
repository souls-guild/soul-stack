package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modURL declares core.url.
var modURL = schema.Module{
	Name:         "url",
	Capabilities: []schema.Capability{schema.NetworkOutbound, schema.FSWriteRoot},
	States: map[string]schema.State{
		"fetched": {
			Description: "Download a file over https to path with checksum verification (ADR-015/ADR-016, https-only).",
			Input: schema.Input{
				// allow_http / allow_private / insecure_skip_verify are opt-out flags
				// (secure-by-default): false by default — the strictest mode possible. Each one
				// relaxes an independent security boundary and is only engaged by an explicit
				// operator choice (dropping the guard is logged as a warn per host).
				"allow_http":    {Type: schema.Bool, Default: false, Description: "Allow http:// (downgrade risk). Does NOT open up SSRF — the dial-guard is held separately (allow_private)."},
				"allow_private": {Type: schema.Bool, Default: false, Description: "Drop the SSRF guard: allow dialing metadata/loopback/RFC1918 (legitimate internal endpoint)."},
				"checksum":      {Type: schema.String, Description: "Expected hash in the form \"sha256:<hex>\"/\"sha1:<hex>\" (supply-chain)."},
				"group":         {Type: schema.String, Description: "Owning group (group name)."},
				// headers — sensitive-by-construction (ADR-010 §7.4): values are never logged and
				// never appear in output. The DSL secret flag is not set: it forces a `^vault:.*`
				// ref, and headers is a free-form map (masked on output, not a vault-ref). Masking
				// is a runtime aspect, not expressible by the trimmed Param.
				"headers":              {Type: schema.Map, Items: &schema.Param{Type: schema.String}, Description: "HTTP request headers (values are secret, never appear in output). If-None-Match/If-Modified-Since here give conditional-GET (304 → no-op)."},
				"insecure_skip_verify": {Type: schema.Bool, Default: false, Description: "Skip TLS chain verification (self-signed / internal CA). MITM risk."},
				"mode":                 {Type: schema.String, Description: "Permissions in octal form, e.g. \"0644\"."},
				"owner":                {Type: schema.String, Description: "Owner (username)."},
				"path":                 {Type: schema.String, Required: true, Description: "Target file path."},
				"timeout":              {Type: schema.String, Description: "Request timeout (Soul Stack duration, e.g. \"300s\")."},
				"url":                  {Type: schema.String, Required: true, Description: "Source (https:// only; SSRF-guard on the resolved IP)."},
			},
		},
	},
}
