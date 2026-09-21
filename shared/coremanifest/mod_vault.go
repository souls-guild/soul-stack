package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modVault declares core.vault.
var modVault = schema.Module{
	Name: "vault",
	States: map[string]schema.State{
		"kv-present": {
			Description: "Keeper-side (on:keeper). Generate-if-absent - guarantees the secret exists (generates the missing crypto/rand per policy; if present - no-op, never returns values).",
			Input: schema.Input{
				"policy":  {Type: schema.Map, Description: "Step-level default policy for all targets: length (8..1024, default 32), charset (alphanumeric|hex|base64url|ascii-printable-safe) OR allowed_chars (mutually exclusive)."},
				"targets": {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.Map}, Description: "Non-empty list of targets {path: <Vault KV path without #field>, field?: <field name, default password>, policy?: <per-target override>}."},
			},
		},
		"kv-read": {
			Description: "Keeper-side (on:keeper). Explicit read of Vault KV with an audit-event (ADR-017).",
			Input: schema.Input{
				"fields": {Type: schema.List, Items: &schema.Param{Type: schema.String}, Description: "Which keys to extract; empty -> the whole secret."},
				"path":   {Type: schema.String, Required: true, Description: "Vault KV path (mount-relative)."},
			},
			// data is the secret itself; path and fields are the audit facts about
			// it, which are exactly what an operator needs to see when this task
			// fails ([ADR-0083] §8 — the per-task `no_log` this replaces hid all
			// three).
			Output: schema.Output{
				"data":   {Type: schema.Map, Secret: true, Description: "The extracted keys and their values."},
				"fields": {Type: schema.List, Items: &schema.Param{Type: schema.String}, Description: "Names of the keys in data, sorted."},
				"path":   {Type: schema.String, Description: "Echo of the requested Vault KV path."},
			},
		},
	},
}
