package config

// Runtime resolution of input values: applying the `input:` schema to the values
// actually passed by the operator ([`docs/input.md`] → "Value resolution").
// Symmetric with the schema validator (input_schema.go validates the schema
// ITSELF): here the schema is already valid, and the passed values are checked.
//
// Used both by the prod path (keeper scenario-runner before render) and by L0
// (soul-trial) — a single source of truth for the effective input, so L0 does not
// mask a missing merge phase.

import (
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ResolveInputValues builds the effective input from the schema and passed
// values:
//
//  1. default merge: for each param with a `default:` missing from provided (or
//     an empty string for type=string without allow_empty — treated as absent,
//     docs/input.md §"Empty strings"), the default is substituted;
//  2. required: a param with `required: true`, no value and no default → error;
//  3. value validation of passed values against the schema: type-match, enum,
//     pattern (type=string) — recursively into array (items) and object
//     (properties + required fields). Default-expressions (`${ … }`/`{{ … }}`)
//     and value-expressions are not validated against enum/pattern at any level
//     (the final value appears only after the CEL/template phase).
//
// provided — the operator-passed input (incarnation.spec.input or L0
// fixtures.input); nil is safe. Returns a NEW map (provided is not mutated);
// unknown provided keys (with no schema) are passed through as is — the MVP
// grammar does not forbid an "unknown input key".
//
// The first error aborts resolution: a clear message to the operator beats a list.
//
// vault resolution of an input-ref (a `vault:` value of a secret field with
// `vault_scope`) is NOT done here — that's the keeper-side scoped phase (see
// [ResolveInputValuesVault]). This path is used by L0-trial (vault-refs are not
// resolved) and by contexts without a vault client; a `vault:` value goes through
// ordinary value validation as a string.
func ResolveInputValues(schema InputSchemaMap, provided map[string]any) (map[string]any, error) {
	merged := mergeInputDefaults(schema, provided)
	if err := requireInputValues(schema, merged); err != nil {
		return nil, err
	}
	if err := validateInputValues(schema, merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// InputVaultResolver resolves ONE input vault-ref into the secret value.
//
// name — the input field name (for audit/diagnostics), s — its schema (carries
// VaultScope + Secret), raw — the operator-passed value (a string with a `vault:`
// prefix). The (keeper-side) implementation checks scope+deny, reads Vault KV and
// writes audit. Returns the resolved value that replaces the `vault:` string
// BEFORE value validation (pattern/enum are checked on it).
type InputVaultResolver func(name string, s *InputSchema, raw string) (any, error)

// ResolveInputValuesVault is the keeper-side resolution of the effective input
// with the scoped vault phase spliced in (docs/input.md → "vault_scope"). Strict
// order:
//
//	default merge + required → vault-resolve input-ref → value validation.
//
// vault-resolve deliberately runs BEFORE value validation: pattern/enum/type are
// checked on the ALREADY-resolved value, not on the `vault:...` string. resolve
// is called exactly once per input-ref (the value is read from Vault once, then
// enters render N times already resolved).
//
// resolve may be nil — behavior then matches [ResolveInputValues] (vault-refs are
// untouched). With a nil resolve a field's `vault:` value passes as a string
// (back-compat for paths without a vault client).
func ResolveInputValuesVault(schema InputSchemaMap, provided map[string]any, resolve InputVaultResolver) (map[string]any, error) {
	merged := mergeInputDefaults(schema, provided)
	if err := requireInputValues(schema, merged); err != nil {
		return nil, err
	}
	if resolve != nil {
		if err := resolveInputVaultRefs(schema, merged, resolve); err != nil {
			return nil, err
		}
	}
	if err := validateInputValues(schema, merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// reSID — the FQDN form of a SID (ADR-044 S-T1): starts with a letter/digit, then
// [a-z0-9.-], up to 254 chars. Matches the Soul identity form (SID = FQDN). Checks
// only value syntax; catalog membership is the backend's job.
var reSID = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,253}$`)

// Regexps for format values that have no exact parser in the stdlib
// (docs/input.md → "Allowed format values"). IP/CIDR/email/uri/duration are
// validated by the net/net-mail/net-url/time parsers — more precise and without a
// hand-written regex.
var (
	// reHostnameLabel — one hostname label (RFC 1123): letter/digit at the edges,
	// a dash allowed inside, 1..63 chars.
	reHostnameLabel = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

	// reUUID — a UUID of any version (8-4-4-4-12 hex), case-insensitive.
	reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

	// reSemver — Semantic Versioning 2.0.0 (semver.org BNF): MAJOR.MINOR.PATCH
	// + opt. pre-release (`-rc1`, `-alpha.1`) + opt. build-metadata (`+build`).
	reSemver = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-(0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*)?(\+[0-9a-zA-Z-]+(\.[0-9a-zA-Z-]+)*)?$`)
)

// validateFormat checks a string value against a predefined format
// (docs/input.md). Returns true if the value is valid. An unknown format
// (schema-validate won't let it through) is treated as "nothing to check" → true.
//
// hostname vs fqdn semantics (docs/input.md examples): hostname = a single
// RFC1123 label with no dots (`redis-01`); fqdn = ≥2 dot-separated labels
// (`redis-01.prod.example.local`). sid is a separate FQDN form (reSID), whose
// enforcement predates this change.
func validateFormat(format, v string) bool {
	switch format {
	case "sid":
		return reSID.MatchString(v)
	case "hostname":
		return reHostnameLabel.MatchString(v)
	case "fqdn":
		return isFQDNValue(v)
	case "ipv4":
		ip := net.ParseIP(v)
		return ip != nil && ip.To4() != nil
	case "ipv6":
		ip := net.ParseIP(v)
		return ip != nil && ip.To4() == nil
	case "cidr":
		_, _, err := net.ParseCIDR(v)
		return err == nil
	case "email":
		addr, err := mail.ParseAddress(v)
		// mail.ParseAddress accepts the "Name <a@b>" form; format:email needs the
		// bare address — verify the parsed address equals the input.
		return err == nil && addr.Address == v
	case "uri":
		u, err := url.Parse(v)
		// A URI must carry a scheme (docs example https://…); a relative path with
		// no scheme is not a URI in the format sense.
		return err == nil && u.Scheme != "" && (u.Host != "" || u.Opaque != "")
	case "uuid":
		return reUUID.MatchString(v)
	case "semver":
		return reSemver.MatchString(v)
	case "duration":
		_, err := time.ParseDuration(v)
		return err == nil
	}
	return true
}

// isFQDNValue — format:fqdn (docs/input.md): ≥2 dot-separated RFC1123 labels, each
// label valid, total length ≤253. Differs from hostname by requiring ≥1 dot.
// Separate from semantic.isFQDN (which validates SID coven-config: lowercase-only,
// accepts a single label) — format:fqdn has its own "fully-qualified name"
// semantics.
func isFQDNValue(v string) bool {
	if len(v) == 0 || len(v) > 253 {
		return false
	}
	labels := strings.Split(v, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !reHostnameLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// vaultRefMarker is the prefix of an input value referencing Vault KV. Matches
// the authored ref form (render.vaultRefPrefix / vault.ParseRef); used here only
// to detect "field value is a vault-ref".
const vaultRefMarker = "vault:"

// resolveInputVaultRefs walks the schema and, for each present string value
// starting with `vault:`, calls resolve and replaces the value with the result. A
// field without `vault_scope` carrying a `vault:` value is an error (default-deny):
// resolve itself must raise it, but ref detection is needed here to call resolve
// at all. Only top-level secret fields (vault_scope applies only to them) — we
// don't descend into array/object.
func resolveInputVaultRefs(schema InputSchemaMap, merged map[string]any, resolve InputVaultResolver) error {
	for name, s := range schema {
		if s == nil || s.Type != "string" {
			continue
		}
		raw, ok := merged[name]
		if !ok {
			continue
		}
		str, ok := raw.(string)
		if !ok || !strings.HasPrefix(str, vaultRefMarker) {
			continue
		}
		val, err := resolve(name, s, str)
		if err != nil {
			return err
		}
		merged[name] = val
	}
	return nil
}

// mergeInputDefaults builds a new map: provided + default substitution for
// missing/empty params (step 1 of docs/input.md "Value resolution"). required is
// not checked here — that's the separate requireInputValues phase, so
// vault-resolve (if any) can sit between merge and validation.
func mergeInputDefaults(schema InputSchemaMap, provided map[string]any) map[string]any {
	out := make(map[string]any, len(provided)+len(schema))
	for k, v := range provided {
		out[k] = v
	}
	for name, s := range schema {
		if s == nil {
			continue
		}
		raw, passed := out[name]
		if passed && isAbsentValue(raw, s) {
			passed = false
			delete(out, name)
		}
		if passed {
			continue
		}
		if hasDefault(s) {
			out[name] = s.Default
		}
	}
	return out
}

// requireInputValues — step 2: a param with required:true (unconditional) or with
// a required_when whose predicate over the merged input is true (conditional),
// with no value and no default → error (after merge — meaning neither provided nor
// default supplied it).
//
// required_when is evaluated AFTER mergeInputDefaults (defaults materialized) —
// the predicate sees the effective input. The predicate context is only input.*
// (a narrow CEL env, input_required_when.go); this is input validation, not
// render. The message carries the same recognizable "required, but not passed and
// has no default" form as the unconditional required — downstream detection
// (checkdrift.isInputRequiredErr) catches both with one match.
func requireInputValues(schema InputSchemaMap, merged map[string]any) error {
	for name, s := range schema {
		if s == nil {
			continue
		}
		if _, present := merged[name]; present {
			continue
		}
		// Required (field-level, bool) is read directly — not via requiredKind:
		// a post-resolve $type node carries object-level RequiredProps
		// (requiredKind==requiredList from the type) AND the overlay-carried
		// field-mandatory Required=true at the same time (ADR-062,
		// applyRefOverlay). Symmetric with validateObjectFields, which reads
		// RequiredProps directly. For non-resolved schemas the invariant is
		// unchanged: Required=true ⟺ requiredKind==requiredBool.
		if s.Required {
			return fmt.Errorf("input %q is required but was not provided and has no default", name)
		}
		if s.RequiredWhen != "" {
			required, err := evalRequiredWhen(s.RequiredWhen, merged)
			if err != nil {
				return fmt.Errorf("input %q: evaluating required_when: %w", name, err)
			}
			if required {
				return fmt.Errorf("input %q is required but was not provided and has no default (required_when: %s)", name, s.RequiredWhen)
			}
		}
	}
	return nil
}

// validateInputValues — step 3: value validation of present values against the
// schema (type/enum/pattern, recursively). Default values are validated too: the
// old ResolveInputValues skipped a default unchecked, but the default already
// passed schema-time validateDefaultContent — re-checking is equivalent and
// simplifies the phase model. Value-expressions (`${…}`/`{{…}}`) are exempt from
// enum/pattern at their level (see validateValueAt).
func validateInputValues(schema InputSchemaMap, merged map[string]any) error {
	for name, s := range schema {
		if s == nil {
			continue
		}
		raw, present := merged[name]
		if !present {
			continue
		}
		if err := validateInputValue(name, s, raw); err != nil {
			return err
		}
	}
	return nil
}

// isAbsentValue reports whether a passed value is treated as "not passed": an
// empty string for type=string without allow_empty (docs/input.md §"Empty
// strings"). Other values (including 0, false, an empty list) are passed.
func isAbsentValue(v any, s *InputSchema) bool {
	if s.Type != "string" || s.AllowEmpty {
		return false
	}
	str, ok := v.(string)
	return ok && str == ""
}

// hasDefault reports whether the param declares a default (a non-nil value). A nil
// default is indistinguishable from no default — it's the absence of a default,
// not a default value.
func hasDefault(s *InputSchema) bool {
	return s.Default != nil
}

// validateInputValue checks one passed value against the param schema (the
// operator→render trust boundary). A thin wrapper over the recursive
// validateValueAt: sets the root path `$.<name>` for clear messages.
func validateInputValue(name string, s *InputSchema, v any) error {
	return validateValueAt("$."+name, s, v)
}

// maskedSecretLiteral is the placeholder for a secret field's raw value in a
// validation error message. The architecture (ADR-010, secret masking) requires
// never showing secrets in any output channel; a validation error lands in
// incarnation.StatusDetails / audit, so masking is needed here, at the source.
const maskedSecretLiteral = "<masked>"

// literalFor returns the value string for an error diagnostic: the raw literal for
// a normal field (the diagnostic matters), the placeholder for a secret field. The
// type isn't disclosed separately — the field format is known from the schema/path
// itself.
func literalFor(s *InputSchema, v any) string {
	if s != nil && s.Secret {
		return maskedSecretLiteral
	}
	return formatLiteral(v)
}

// validateValueAt recursively validates a passed value against the schema at any
// nesting depth (qa.1: top-level-only let garbage through inside array/object all
// the way to CEL/shell — a trust-boundary hole). Symmetric with schema-time
// validateDefaultValue (input_schema.go), but in runtime form: returns an error
// with a path (`$.users[1].acl`) instead of a diag, and applies the full set of
// checks to a passed value (type → enum → pattern → required-props), not just a
// type-match (we trust an author's default literal more than operator input).
//
// What's expressed in the schema is checked (type/enum/pattern); a full format
// validator (ipv4/fqdn/…) is post-MVP, the forms aren't used yet in prod services
// (redis: pattern only).
//
// At every level a string value-expression (`${ … }` / `{{ … }}`) is exempt from
// enum AND pattern: the final form appears only after the render phase
// (docs/input.md §"Value resolution").
func validateValueAt(path string, s *InputSchema, v any) error {
	if s == nil || s.Type == "" {
		return nil
	}

	// A string-expression is exempt from this level's value checks (enum +
	// pattern) — its final form is unknown here. The "string" type is still
	// formally satisfied, so it passes the type check below normally.
	exprString := s.Type == "string" && isStringExpr(v)

	if !valueMatchesType(v, s.Type) {
		return fmt.Errorf("input %s = %s does not match type %q", path, literalFor(s, v), s.Type)
	}

	if !exprString && len(s.Enum) > 0 &&
		(s.Type == "string" || s.Type == "integer" || s.Type == "number" || s.Type == "boolean") {
		if !enumContains(s.Enum, v) {
			// The field's enum literals are masked too: for a secret field the
			// list of allowed values is itself a secret (e.g. a fixed password
			// set).
			if s.Secret {
				return fmt.Errorf("input %s = %s is not in enum", path, maskedSecretLiteral)
			}
			return fmt.Errorf("input %s = %s is not in enum %s", path, formatLiteral(v), formatEnum(s.Enum))
		}
	}

	// format — each value is checked against a predefined format (docs/input.md →
	// "Allowed format values": hostname/fqdn/ipv4/ipv6/cidr/email/uri/uuid/
	// semver/duration + sid). Only the STRUCTURE of the value is checked; catalog
	// membership / resource availability is NOT checked here (for sid the backend
	// resolves the hosts catalog / Choir party). For type=array the check applies
	// per element via items (validateValueAt recursion). Exempt for a
	// value-expression for the same reason as enum/pattern.
	if !exprString && s.Type == "string" && s.Format != "" {
		if !validateFormat(s.Format, v.(string)) {
			return fmt.Errorf("input %s = %s does not match format %q", path, literalFor(s, v), s.Format)
		}
	}

	if !exprString && s.Type == "string" && s.Pattern != "" {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			// The schema is already validated (input_pattern_invalid) — an
			// invalid pattern shouldn't reach here; defensive.
			return fmt.Errorf("input %s: pattern %q does not compile: %w", path, s.Pattern, err)
		}
		if !re.MatchString(v.(string)) {
			return fmt.Errorf("input %s = %s does not match pattern %q", path, literalFor(s, v), s.Pattern)
		}
	}

	// min_length / max_length — length in Unicode code points (docs/input.md), not
	// bytes: utf8.RuneCountInString. Exempt for a value-expression for the same
	// reason as enum/pattern — the final length appears only after render.
	// Schema-time already guaranteed min_length >= 0, max_length >= min_length;
	// here the operator's ACTUAL value is checked (previously not enforced —
	// lint↔runtime drift).
	if !exprString && s.Type == "string" && (s.MinLength != nil || s.MaxLength != nil) {
		n := utf8.RuneCountInString(v.(string))
		if s.MinLength != nil && n < *s.MinLength {
			return fmt.Errorf("input %s = %s is shorter than min_length %d (length %d)", path, literalFor(s, v), *s.MinLength, n)
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			return fmt.Errorf("input %s = %s is longer than max_length %d (length %d)", path, literalFor(s, v), *s.MaxLength, n)
		}
	}

	switch s.Type {
	case "array":
		return validateArrayItems(path, s, v)
	case "object":
		return validateObjectFields(path, s, v)
	}
	return nil
}

// validateArrayItems validates each array element against the items schema and
// checks length limits (min_items/max_items). The array type-match was already
// checked by validateValueAt above. Schema-time already guaranteed min_items >= 0
// and max_items >= min_items; here the operator's ACTUAL length is checked
// (previously not enforced — lint↔runtime drift; for a sid-list, ADR-044 S-T1, the
// limits mean "no fewer/no more than N selected hosts").
func validateArrayItems(path string, s *InputSchema, v any) error {
	arr := v.([]any)
	n := len(arr)
	if s.MinItems != nil && n < *s.MinItems {
		return fmt.Errorf("input %s contains %d elements, fewer than min_items %d", path, n, *s.MinItems)
	}
	if s.MaxItems != nil && n > *s.MaxItems {
		return fmt.Errorf("input %s contains %d elements, more than max_items %d", path, n, *s.MaxItems)
	}
	if s.Items == nil {
		// items is required for an array (schema-validate catches its absence);
		// without an element schema there's nothing to descend into.
		return nil
	}
	for i, el := range arr {
		if err := validateValueAt(fmt.Sprintf("%s[%d]", path, i), s.Items, el); err != nil {
			return err
		}
	}
	return nil
}

// validateObjectFields validates object fields against the properties schemas and
// checks presence of required fields. The object type-match was already checked
// above.
//
// Fields with no schema in properties are not validated (additional_properties —
// the MVP doesn't check runtime values in depth, symmetric with
// validateDefaultValue).
func validateObjectFields(path string, s *InputSchema, v any) error {
	obj := v.(map[string]any)

	for _, req := range s.RequiredProps {
		fv, present := obj[req]
		if !present || isMissingField(s.Properties[req], fv) {
			return fmt.Errorf("input %s.%s is required but was not provided", path, req)
		}
	}

	for k, fv := range obj {
		prop := s.Properties[k]
		if prop == nil || prop.Type == "" {
			continue
		}
		if err := validateValueAt(path+"."+k, prop, fv); err != nil {
			return err
		}
	}
	return nil
}

// isMissingField — an object field is treated as not-passed by the same
// empty-string semantics as top-level isAbsentValue: an empty string for
// type=string without allow_empty = "not passed" (docs/input.md §"Empty strings").
// prop may be nil (required references a field outside properties — schema-validate
// catches that, defensive here: treat as passed).
func isMissingField(prop *InputSchema, v any) bool {
	if prop == nil {
		return false
	}
	return isAbsentValue(v, prop)
}

// isStringExpr reports whether the value is a string-expression (`${ … }` /
// `{{ … }}`). A convenience wrapper over isExprLiteral for any values: non-string
// → false.
func isStringExpr(v any) bool {
	str, ok := v.(string)
	return ok && isExprLiteral(str)
}

// isExprLiteral reports whether the string is a CEL/template expression
// (`${ … }` / `{{ … }}`) whose final value appears only after the render phase.
// Such values can't be checked against pattern at resolve time. Same heuristic as
// defaultMatchesType for default-expressions.
func isExprLiteral(s string) bool {
	t := strings.TrimSpace(s)
	return (strings.HasPrefix(t, "${") && strings.HasSuffix(t, "}")) ||
		(strings.HasPrefix(t, "{{") && strings.HasSuffix(t, "}}"))
}
