package audit

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// maskedValue is the placeholder that replaces sensitive values in a payload
// before INSERT. Matches the masking of the OTel-exporter Operator API
// (docs/keeper/operator-api.md → Secret masking).
const maskedValue = "***MASKED***"

// sensitiveKeyRe is a case-insensitive substring match on the key name. It masks
// any key CONTAINING one of the secret fragments, not only exact matches:
// `bootstrap_token`, `aws_secret_access_key`, `db_password`, `tls_private_key`,
// `credentials_ref`, `jwt_signing_key` all match (security-review H1: an exact
// match on `"token"` let `"bootstrap_token"` through → plaintext leak).
//
// TLS PEM material (`tls_key` / `tls_cert` / `tls_ca`, plus the hyphen and
// `*-data` forms — `tls-key`, `tls_ca_data`) is masked by the fragment
// `tls[_-]?(key|cert|ca)`: the masking model is the key NAME, and a bare
// `key`/`cert`/`ca` fragment is not in the catalog (to avoid over-masking
// harmless `cache`/`scary`). The private key (`tls_key`) is a secret in the
// strict sense; cert/ca are masked too (marked `secret: true` in the schema and
// may carry private material in a combined PEM). The boundary is exact:
// `certificate`/`cacheable` does NOT match (needs the `tls` prefix + separator).
// Source: redis TLS consolidation (community.redis: PEM in connect params;
// BLOCKER masking-guard).
//
// Extend the catalog with an ordinary PR to this regex when a new sensitive area
// appears; the dictionary invariant does not require propose-and-wait (it
// formalizes an observed pattern, see docs/architecture.md → Error codes /
// catalog extension).
var sensitiveKeyRe = regexp.MustCompile(
	`(?i)(token|secret|password|passwd|private[_-]?key|privatekey|credential|signing[_-]?key|api[_-]?key|access[_-]?key|tls[_-]?(key|cert|ca))`,
)

// extraExactKeys are short keys with no secret fragment in the name that still
// carry a secret. The substring regex would miss them (`jwt` contains neither
// token nor secret), so they are kept as a separate exact set (case-insensitive
// comparison on the lower-cased key).
var extraExactKeys = map[string]struct{}{
	"jwt": {},
}

// isSensitiveKey reports whether the key must be masked whole (by the substring
// regex or the extra-exact set). Case-insensitive.
func isSensitiveKey(key string) bool {
	if sensitiveKeyRe.MatchString(key) {
		return true
	}
	_, ok := extraExactKeys[strings.ToLower(key)]
	return ok
}

// CredentialsRefPrefix is the canonical form of a vault reference to a KV secret
// ([ADR-017]: `vault:<mount>/<path>`, default mount `secret`). Any string value
// CONTAINING this marker is masked whole (a vault path can leak into
// logs/observability via the payload). Applied to string values regardless of
// key (a second filter on top of the key match).
//
// Substring, not prefix, match (security-review: a vault ref leaks not only as
// the bare value `vault:secret/x` but also glued into a string — error messages
// like `render: ... vault:secret/db ...` that reach status_details
// (GET incarnation) and error_summary. A prefix filter let them through →
// plaintext leak of the vault path into an observable channel).
//
// Masking is done by [vaultRefRe] — a regexp on the form `vault:<mount>/` (any
// mount, not just the default `secret`): security audit K5 showed the operator
// may configure a custom KV mount in `keeper.yml` (config.Vault.KVMount), after
// which refs like `vault:kv/…` / `vault:db-creds/…` leaked into
// audit/OTel/SSE/error in plaintext — the `vault:secret/` marker missed them.
// The regexp requires a mount token + `/`, so legitimate strings without a vault
// ref are not over-masked: `https://vault:8200` (no `/` after the port token),
// `hashicorp/vault:1.18` (`1.18` has no `/`), `vault: KV error` (space is not in
// the token class) — passthrough.
//
// CredentialsRefPrefix stays as a default-mount constant for other consumers
// (provider-ref validation); masking itself goes through [vaultRefRe].
const CredentialsRefPrefix = "vault:secret/"

// vaultRefRe matches the canonical vault-reference form `vault:<mount>/<path>`
// with an arbitrary mount token (`secret`, `kv`, `db-creds`, …). The mount token
// is `[A-Za-z0-9._-]+` (characters valid in a Vault mount path) followed by a
// mandatory `/` (the mount↔rel separator from vault.ParseRef). This closes the
// K5 gap (custom mount) without over-masking strings that have no ref form.
var vaultRefRe = regexp.MustCompile(`vault:[A-Za-z0-9._-]+/`)

// MaskSecrets returns a copy of payload with sensitive values masked. The walk
// is recursive — it descends into nested maps and slices, including typed
// containers (`map[string]string`, `[]string`, structs, pointers) via reflect.
//
// Masking rules:
//
//   - Key (case-insensitive) matches [isSensitiveKey] → the value becomes
//     `"***MASKED***"` (type is lost, a compliance requirement).
//   - String value contains `vault:secret/` (anywhere) → also `"***MASKED***"`
//     (guards against leaking vault refs into logs/observability via any key,
//     including those glued into error strings; the marker is narrowed — see
//     [CredentialsRefPrefix]).
//   - Map (any key/value type) → recursive walk; the key is stringified for the
//     key match.
//   - Slice / array → recursive walk of elements.
//   - Struct → walk of fields (field name = key); unexported fields are skipped.
//   - Pointer / interface → dereference and walk.
//   - Other scalar values are copied as-is.
//
// payload is not mutated; a new map of the same shape is returned (top-level is
// always `map[string]any`, nesting normalized to `map[string]any`/`[]any`/scalar
// when walking typed containers).
// nil input → nil output (the caller treats it as an empty payload).
func MaskSecrets(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		if isSensitiveKey(k) {
			out[k] = maskedValue
			continue
		}
		out[k] = maskValue(v)
	}
	return out
}

// maskValue is the walk helper. Unexported: its result format and shape are only
// meaningful within MaskSecrets.
//
// Fast path for common types (`string`/`map[string]any`/`[]any`/
// `map[string]string`/`[]string`) — no reflect. Other containers (struct, other
// map/slice/ptr) go through the reflect walk — a cold path (audit/SSE payload)
// where readability beats allocations.
func maskValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return maskString(x)
	case map[string]any:
		return MaskSecrets(x)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = maskValue(el)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, el := range x {
			if isSensitiveKey(k) {
				out[k] = maskedValue
				continue
			}
			out[k] = maskString(el)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = maskString(el)
		}
		return out
	default:
		return maskReflect(reflect.ValueOf(v))
	}
}

// maskString masks a string value if it contains a vault-ref marker (anywhere,
// not only as a prefix — see [CredentialsRefPrefix]).
func maskString(s string) any {
	if vaultRefRe.MatchString(s) {
		return maskedValue
	}
	return s
}

// maskReflect is the reflect fallback for typed containers not covered by the
// maskValue fast path (struct, arbitrary map/slice types, pointers). Returns a
// structure normalized to `map[string]any`/`[]any`/scalar.
func maskReflect(rv reflect.Value) any {
	if !rv.IsValid() {
		return nil
	}
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return maskReflect(rv.Elem())
	case reflect.String:
		return maskString(rv.String())
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			k := stringifyKey(iter.Key())
			if isSensitiveKey(k) {
				out[k] = maskedValue
				continue
			}
			out[k] = maskReflect(iter.Value())
		}
		return out
	case reflect.Slice, reflect.Array:
		n := rv.Len()
		out := make([]any, n)
		for i := 0; i < n; i++ {
			out[i] = maskReflect(rv.Index(i))
		}
		return out
	case reflect.Struct:
		t := rv.Type()
		out := make(map[string]any, rv.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				// Unexported field — reflect cannot read the value safely;
				// skip it (secrets in the payload live in exported fields).
				continue
			}
			name := structFieldName(f)
			if isSensitiveKey(name) {
				out[name] = maskedValue
				continue
			}
			out[name] = maskReflect(rv.Field(i))
		}
		return out
	default:
		return rv.Interface()
	}
}

// stringifyKey converts a reflect map key to a string for the key match.
// Non-string keys (int, etc.) are stringified via %v — a secret in such a key is
// unlikely, but the key match still works by name.
func stringifyKey(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	return fmt.Sprintf("%v", k.Interface())
}

// structFieldName is the field name for the key match: the json tag (without
// options) if set, otherwise the field name. The json tag matters because
// payload structs serialize by tags — a secret tagged `json:"bootstrap_token"`
// must match the same as the map key `bootstrap_token`.
func structFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return f.Name
	}
	if comma := strings.IndexByte(tag, ','); comma >= 0 {
		tag = tag[:comma]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}
