package audit

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// maskedValue is the placeholder that replaces sensitive values in a payload
// before INSERT. Matches the masking of the OTel-exporter Operator API
// (docs/keeper/operator-api.md → Secret masking).
const maskedValue = "***MASKED***"

// MaskedValue is [maskedValue] for consumers outside this package that must
// recognize or assert on the placeholder.
const MaskedValue = maskedValue

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
// Source: redis TLS consolidation (redis.instance: PEM in connect params;
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

// IsSensitiveKey reports whether a key names a value this package would mask.
// Exported for a caller that must REFUSE rather than mask: `core.ssh.run`
// (NIM-849) will not put a host field with such a name into a command line at
// all, and the one catalog of sensitive names has to be the same one the
// maskers use — a second spelling would let the refusal and the masking
// disagree about which field is the token.
func IsSensitiveKey(key string) bool { return isSensitiveKey(key) }

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

// vaultRefTokenRe matches a whole vault reference — [vaultRefRe] plus the
// relative path that follows it, up to the first character that cannot be part
// of one (whitespace, a quote, a shell metacharacter). Used by
// [MaskRefsInText]; the payload masker replaces the whole value and needs only
// to DETECT a ref, while a text masker must know where the ref ends.
var vaultRefTokenRe = regexp.MustCompile(`vault:[A-Za-z0-9._-]+/[A-Za-z0-9._/-]*`)

// MaskRefsInText replaces every vault reference inside s with [MaskedValue],
// leaving the surrounding text intact.
//
// This is the free-text counterpart of [MaskSecrets], which masks a matching
// value WHOLE. Whole-value masking is right for a payload field — the field is
// the secret — and wrong for a byte stream that happens to contain one:
// blanking a 32 KiB console recording chunk because a vault path appeared in it
// destroys the record the masking exists to make safe to keep
// ([ADR-0074(g)](../../docs/adr/0074-interactive-console-pty.md), NIM-145).
//
// Only the content layer fires here. A free-form stream has no key for the
// name layer ([isSensitiveKey]) to read, and a credential a command prints in
// plaintext is indistinguishable from any other text — the same bound the MCP
// `run-command` surface states (ADR-0074 amendment 2026-07-27).
func MaskRefsInText(s string) string {
	if s == "" {
		return s
	}
	return vaultRefTokenRe.ReplaceAllString(s, maskedValue)
}

// MaskRefsInBytes is [MaskRefsInText] over a byte slice. The input is never
// mutated; when there is nothing to mask the input is returned as-is.
func MaskRefsInBytes(b []byte) []byte {
	if len(b) == 0 || !vaultRefTokenRe.Match(b) {
		return b
	}
	return vaultRefTokenRe.ReplaceAll(b, []byte(maskedValue))
}

// vaultRefMarker is the literal every vault reference starts with. Callers of
// [SafeMaskSplit] do not need it; it is the anchor that makes a partial match
// recognizable without running the regexp against every suffix.
const vaultRefMarker = "vault:"

// vaultRefMarkerBytes is [vaultRefMarker] for the byte-slice searches in
// [SafeMaskSplit], which runs under the recording lock on every chunk of a live
// pty stream: `string(buf)` there copies the whole buffer to answer a question
// about six bytes of it (NIM-815).
var vaultRefMarkerBytes = []byte(vaultRefMarker)

// MaxMaskCarryBytes bounds how much of a stream [SafeMaskSplit] will hold back.
// A reference longer than this is not masked across a chunk boundary — a mount
// plus path of 4 KiB is not a reference anybody writes, while an unbounded
// carry would be a memory hole on a stream that happens to contain the literal
// `vault:` followed by megabytes of base64.
const MaxMaskCarryBytes = 4 << 10

// SafeMaskSplit reports how many leading bytes of buf may be masked and written
// out now, so that a vault reference SPLIT ACROSS chunk boundaries is still
// masked. The remainder is the caller's carry: prepend it to the next chunk.
//
// Chunking is not hypothetical on a console. A pty echoes keystrokes one byte
// at a time, so an operator typing `vault:secret/db` produces fifteen chunks
// and not one of them matches the reference pattern. Masking each chunk in
// isolation would therefore mask nothing at all, exactly where the operator is
// most likely to type a credential path.
//
// Returns len(buf) when nothing could be in progress, and 0 when the whole
// buffer might be. The bound is [MaxMaskCarryBytes] — past it the buffer is
// released whole, so a complete reference inside it is still masked and only a
// reference longer than the bound can straddle.
func SafeMaskSplit(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	if i := bytes.LastIndex(buf, vaultRefMarkerBytes); i >= 0 && refTailGrowable(buf[i+len(vaultRefMarker):]) {
		if len(buf)-i > MaxMaskCarryBytes {
			return len(buf)
		}
		return i
	}
	// No marker in progress, but the buffer may end PART-WAY through one.
	n := len(vaultRefMarker) - 1
	if n > len(buf) {
		n = len(buf)
	}
	for ; n > 0; n-- {
		if bytes.HasSuffix(buf, vaultRefMarkerBytes[:n]) {
			return len(buf) - n
		}
	}
	return len(buf)
}

// refTailGrowable reports whether every byte could still be part of the mount
// or path of a vault reference — i.e. whether the marker before it may yet grow
// into a full match. The class is the union of the mount and path classes of
// [vaultRefTokenRe]; a superset is correct here, since a false "growable" only
// delays bytes by one chunk.
func refTailGrowable(tail []byte) bool {
	for _, c := range tail {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '/':
		default:
			return false
		}
	}
	return true
}

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
