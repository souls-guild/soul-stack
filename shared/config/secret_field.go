package config

// A secret declared in `state_schema` ([ADR-0083] §1). The service author marks the
// property and writes nothing else; the platform derives the Vault path from
// (service, incarnation, state field, key). No path is authored anywhere.
//
// Exactly two shapes are accepted, because the derivation formula has exactly one field
// segment and one optional key segment:
//
//	# collection — one secret per element, addressed by a sibling property
//	redis_users:
//	  type: array
//	  items:
//	    type: object
//	    properties:
//	      name:     { type: string }
//	      password: { type: secret, key: name }
//
//	# scalar — one secret for the whole incarnation
//	admin_password:
//	  type: secret
//
// A `type: secret` anywhere else — deeper nesting, an array of arrays, under
// `additionalProperties` — is a load-time error rather than a best-effort guess. The
// alternative is a path the author cannot predict from the declaration they wrote.
//
// Do not confuse this with the older, orthogonal marker `secret: true` on a state
// property ([ADR-010] §7.4): that one says "this value LIVES in state, mask it on the
// way out". `type: secret` says the opposite — the value never lives in state at all.
// Both stay, and a `type: secret` path is masked too ([isSecretNode]) so that a value
// arriving there through some other route is not printed.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SecretTypeName — the `type:` value marking a state_schema property as a declared
// secret.
const SecretTypeName = "secret"

// ScalarSecretVaultField — the field name inside the Vault secret of a SCALAR declared
// field. A collection secret is keyed by its property name (`password`); a scalar one
// has no property name to use, and an explicit constant beats an empty `#` suffix that
// would make resolution depend on the number of fields in the KV entry (parity with the
// reasoning in keeper/internal/secretwrite.WriteString).
const ScalarSecretVaultField = "value"

// SecretFieldReservedStateCode — a declared secret on a state field the platform
// already derives under (NIM-706).
const SecretFieldReservedStateCode = "secret_field_reserved_state_name"

// defaultVaultMount — the KV mount assumed when a caller passes "" (matches
// keeper/internal/secretwrite.defaultMount and vault.defaultKVMount).
const defaultVaultMount = "secret"

// vaultSegmentRe matches a safe Vault path segment: letters/digits/`_`/`-`. Rejects
// `.`/`..`/slashes/`#`/empty. This is the [ADR-064] write-path grammar, lifted here so
// the derived path of a declared secret and keeper's operator-secret writer check the
// same rule instead of two copies that drift.
var vaultSegmentRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidVaultPathSegment reports whether s is safe as one segment of a Vault path.
func ValidVaultPathSegment(s string) bool { return vaultSegmentRe.MatchString(s) }

// EffectiveVaultMount resolves the KV mount a derived path lands on: the caller's
// configured mount, or the default when it is empty. Exported so a caller can assemble
// the namespace prefix of a service INDEPENDENTLY of [SecretField.VaultPath] — a guard
// built out of the thing it guards stops guarding it.
//
// Slashes are trimmed because keeper/internal/vault.Client does the same to
// cfg.KVMount before it builds a request. A `kv_mount: "secret/"` that survived here
// would derive `secret//<service>/…` — a path the client would never read back under
// that name, so the write and the ref would disagree with the read. Loading a
// keeper.yml with a mount that is not one plain segment is an error
// (`vault_kv_mount_invalid`); this trim is the second half of that pair, for the
// callers that get a mount from somewhere other than a validated config.
func EffectiveVaultMount(mount string) string {
	mount = strings.Trim(mount, "/")
	if mount == "" {
		return defaultVaultMount
	}
	return mount
}

// secretNodeKeys — the whole grammar of a `type: secret` node. A key outside it is an
// author error rather than an extension point: a `minLength: 40` written next to
// `type: secret` reads as enforced, and nothing enforces it — the value never passes
// through state validation, because it is never in state. (Same reasoning as the policy
// grammar tightening, [ADR-0083] §9.)
var secretNodeKeys = map[string]bool{"type": true, "key": true, "label": true}

// SecretField is one `type: secret` property declared in a service's `state_schema`.
//
// Zero Property and Key mean a scalar field; both set mean one secret per element of a
// collection. There is no third combination — [CollectSecretFields] is the only
// constructor and it produces no other.
type SecretField struct {
	// State is the top-level `state_schema` property: the collection holding the
	// secrets, or the scalar field that is one.
	State string
	// Property is the secret property inside a collection element. Empty for a scalar
	// field.
	Property string
	// Key is the sibling property whose value addresses one element's secret. Empty for
	// a scalar field.
	Key string
	// Label is the optional UI caption (the surviving quarter of `revealable_secrets`,
	// [ADR-0083] §2).
	Label string
	// Path is the YAML path of the declaration RELATIVE to the state_schema root
	// (`.properties.redis_users.items.properties.password`). The caller prefixes its own
	// root to turn a rejected declaration into a positional diagnostic.
	Path string
}

// Collection reports whether the field declares one secret per element rather than one
// per incarnation.
func (f SecretField) Collection() bool { return f.Key != "" }

// ID is the stable identifier of the declaration — what `revealable_secrets.id` used to
// be spelled out by hand ([ADR-0083] §2). A collection element may carry more than one
// secret, so the property has to be part of it.
func (f SecretField) ID() string {
	if f.Property == "" {
		return f.State
	}
	return f.State + "." + f.Property
}

// VaultField is the field name inside the Vault KV entry.
func (f SecretField) VaultField() string {
	if f.Property == "" {
		return ScalarSecretVaultField
	}
	return f.Property
}

// VaultPath derives the logical Vault path of one secret value ([ADR-0083] §1):
//
//	collection:  <mount>/<service>/<incarnation>/<state-field>/<key>
//	scalar:      <mount>/<service>/<incarnation>/<state-field>
//
// key addresses the element and is required for a collection field, ignored for a
// scalar one. mount == "" means the default KV mount.
//
// Every segment is checked against [ValidVaultPathSegment] and the call fails closed.
// That is not a formality: `key` comes from state DATA — an operator-supplied user name
// — and is the one segment an attacker can influence. The service's own `types.yml` may
// constrain it as well; the platform does not rely on a service having done so.
func (f SecretField) VaultPath(mount, service, incarnation, key string) (string, error) {
	mount = EffectiveVaultMount(mount)
	// Namespace floor (NIM-706): a service whose name is one the platform writes under
	// as a fixed first segment derives on top of that family, and secretwrite REPLACES
	// a KV entry rather than merging into it. Registration refuses the name
	// ([IsReservedVaultNamespace] at serviceregistry.validateFields and at
	// `service_name_reserved`), so reaching this is either a service admitted before the
	// rule existed or a route that skipped it — both are cases for failing closed rather
	// than emitting the path. The mount is deliberately not part of the comparison, for
	// the same reason [PathAddressesOwnNamespace] leaves it out: what makes the path
	// collide is the service segment, not which mount it lives on.
	if IsReservedVaultNamespace(service) {
		return "", fmt.Errorf("secret field %s: service %q is a reserved Vault namespace (%s) — its derived path would collide with the platform's own",
			f.ID(), service, strings.Join(ReservedVaultNamespaceNames(), ", "))
	}
	segs := []struct{ name, value string }{
		{"mount", mount},
		{"service", service},
		{"incarnation", incarnation},
		{"state field", f.State},
	}
	if f.Collection() {
		if key == "" {
			return "", fmt.Errorf("secret field %s: key is required for a collection secret", f.ID())
		}
		segs = append(segs, struct{ name, value string }{"key", key})
	}
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if !ValidVaultPathSegment(s.value) {
			return "", fmt.Errorf("secret field %s: unsafe %s segment %q", f.ID(), s.name, s.value)
		}
		out = append(out, s.value)
	}
	return strings.Join(out, "/"), nil
}

// VaultRef derives the internal `vault:<path>#<field>` reference — the same form
// keeper/internal/secretwrite returns for an operator-supplied secret, so a consumer
// resolves both through one code path.
func (f SecretField) VaultRef(mount, service, incarnation, key string) (string, error) {
	path, err := f.VaultPath(mount, service, incarnation, key)
	if err != nil {
		return "", err
	}
	return "vault:" + path + "#" + f.VaultField(), nil
}

// SecretFieldIssue is one rejected declaration. Path is relative to the state_schema
// root, so the caller prefixes its own root and resolves a position; Code is the
// diagnostic code the caller emits.
type SecretFieldIssue struct {
	Path    string
	Code    string
	Message string
	Hint    string
}

// CollectSecretFields returns every secret declared in a `state_schema`, sorted by
// declaration path, plus the declarations it refused.
//
// A refusal is never silent and never partial: the tree is first scanned for EVERY
// `type: secret` node wherever it sits, then the two supported shapes claim theirs, and
// whatever is left over is reported as an unsupported location. A shape this function
// does not understand can therefore not slip through as "no secrets here" — which would
// put a plaintext password in `incarnation.state` and mask nothing.
func CollectSecretFields(schema map[string]any) ([]SecretField, []SecretFieldIssue) {
	if schema == nil {
		return nil, nil
	}
	declared := map[string]bool{}
	scanSecretNodes(schema, "", declared, false)

	var fields []SecretField
	var issues []SecretFieldIssue
	claimed := map[string]bool{}

	props, _ := schema["properties"].(map[string]any)
	for _, name := range sortedMapKeys(props) {
		node, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		base := ".properties." + name
		switch schemaNodeType(node) {
		case SecretTypeName:
			claimed[base] = true
			f, iss := scalarSecretField(name, node, base)
			issues = append(issues, iss...)
			if len(iss) == 0 {
				fields = append(fields, f)
			}
		case "array":
			items, _ := node["items"].(map[string]any)
			itemProps, _ := items["properties"].(map[string]any)
			for _, prop := range sortedMapKeys(itemProps) {
				pnode, ok := itemProps[prop].(map[string]any)
				if !ok || schemaNodeType(pnode) != SecretTypeName {
					continue
				}
				path := base + ".items.properties." + prop
				claimed[path] = true
				f, iss := collectionSecretField(name, prop, pnode, itemProps, items, path)
				issues = append(issues, iss...)
				if len(iss) == 0 {
					fields = append(fields, f)
				}
			}
		}
	}

	for _, path := range sortedMapKeys(declared) {
		if claimed[path] {
			continue
		}
		issues = append(issues, SecretFieldIssue{
			Path: path, Code: "secret_field_unsupported_location",
			Message: "type: secret is only supported on a top-level property or on a property of a top-level array's items",
			Hint:    "the Vault path is derived from (service, incarnation, state field, key) — a deeper position has no place in it",
		})
	}
	return fields, issues
}

// reservedStateFieldIssue rejects a declared secret whose state field is a name the
// platform already derives under inside the service's own namespace (NIM-706). Shared by
// both shapes: the collision is decided by the field name alone, and a collection's
// element key — the segment that would actually complete it — is state DATA, unknown at
// authoring time, so the field name is the last static point where this can be refused.
func reservedStateFieldIssue(state, path string) []SecretFieldIssue {
	if !IsReservedStateField(state) {
		return nil
	}
	return []SecretFieldIssue{{
		Path: path, Code: SecretFieldReservedStateCode,
		Message: fmt.Sprintf("state field %q is reserved: the platform already derives secrets under `<mount>/<service>/<incarnation>/%s/`", state, state),
		Hint:    "rename the state field — reserved names are " + strings.Join(ReservedStateFieldNames(), ", "),
	}}
}

// scalarSecretField builds the field for a top-level `type: secret` property.
func scalarSecretField(name string, node map[string]any, path string) (SecretField, []SecretFieldIssue) {
	issues := checkSecretNodeGrammar(node, path)
	if _, has := node["key"]; has {
		issues = append(issues, SecretFieldIssue{
			Path: path + ".key", Code: "secret_field_key_on_scalar",
			Message: "key: is meaningless on a scalar secret — there is one value, not one per element",
			Hint:    "drop key:, or move the secret into the items of a collection",
		})
	}
	if !ValidVaultPathSegment(name) {
		issues = append(issues, SecretFieldIssue{
			Path: path, Code: "secret_field_name_unsafe",
			Message: fmt.Sprintf("state field %q is not a safe Vault path segment", name),
			Hint:    "letters, digits, `_` and `-` only — the field name becomes a path segment",
		})
	}
	issues = append(issues, reservedStateFieldIssue(name, path)...)
	label, labelIssue := secretNodeLabel(node, path)
	issues = append(issues, labelIssue...)
	return SecretField{State: name, Label: label, Path: path}, issues
}

// collectionSecretField builds the field for a `type: secret` property inside the items
// of a top-level array, and validates the `key:` that addresses one element.
func collectionSecretField(state, prop string, node, itemProps, items map[string]any, path string) (SecretField, []SecretFieldIssue) {
	issues := checkSecretNodeGrammar(node, path)

	for _, seg := range []struct{ kind, value string }{{"state field", state}, {"property", prop}} {
		if !ValidVaultPathSegment(seg.value) {
			issues = append(issues, SecretFieldIssue{
				Path: path, Code: "secret_field_name_unsafe",
				Message: fmt.Sprintf("%s %q is not a safe Vault path segment", seg.kind, seg.value),
				Hint:    "letters, digits, `_` and `-` only — the name becomes a path segment",
			})
		}
	}
	issues = append(issues, reservedStateFieldIssue(state, path)...)

	key, _ := node["key"].(string)
	switch {
	case node["key"] == nil:
		issues = append(issues, SecretFieldIssue{
			Path: path, Code: "secret_field_key_required",
			Message: "a secret inside a collection needs key: <sibling property> to address one element",
			Hint:    "key: name — the sibling whose value becomes the path segment",
		})
	case key == "":
		issues = append(issues, SecretFieldIssue{
			Path: path + ".key", Code: "secret_field_key_invalid",
			Message: fmt.Sprintf("key: must be the name of a sibling property, got %T", node["key"]),
		})
	default:
		sibling, ok := itemProps[key].(map[string]any)
		switch {
		case !ok:
			issues = append(issues, SecretFieldIssue{
				Path: path + ".key", Code: "secret_field_key_unknown",
				Message: fmt.Sprintf("key: %q names no sibling property of the element", key),
			})
		case schemaNodeType(sibling) != "string":
			issues = append(issues, SecretFieldIssue{
				Path: path + ".key", Code: "secret_field_key_not_string",
				Message: fmt.Sprintf("key: %q must name a string property, it is %q", key, schemaNodeType(sibling)),
				Hint:    "the value becomes one segment of the Vault path",
			})
		}
	}

	// `required` naming the secret would make every element invalid: the value is in
	// Vault, so it is never present in the state the schema describes.
	for _, req := range stringSeq(items["required"]) {
		if req == prop {
			issues = append(issues, SecretFieldIssue{
				Path: path, Code: "secret_field_required",
				Message: fmt.Sprintf("%q is listed in required: — a secret never lives in state, so it can never satisfy it", prop),
				Hint:    "remove it from required:",
			})
		}
	}

	label, labelIssue := secretNodeLabel(node, path)
	issues = append(issues, labelIssue...)
	return SecretField{State: state, Property: prop, Key: key, Label: label, Path: path}, issues
}

// checkSecretNodeGrammar rejects any key outside [secretNodeKeys]. Offenders are
// reported in sorted order — map iteration is unordered, and an error text that varies
// run to run on the same input is not diagnosable.
func checkSecretNodeGrammar(node map[string]any, path string) []SecretFieldIssue {
	var unknown []string
	for k := range node {
		if !secretNodeKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return []SecretFieldIssue{{
		Path: path, Code: "secret_field_unknown_key",
		Message: fmt.Sprintf("type: secret does not take %s (want type/key/label)", strings.Join(unknown, ", ")),
		Hint:    "a secret value never passes through state validation, so a constraint written here would not be enforced",
	}}
}

// secretNodeLabel reads the optional UI caption.
func secretNodeLabel(node map[string]any, path string) (string, []SecretFieldIssue) {
	v, has := node["label"]
	if !has || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", []SecretFieldIssue{{
			Path: path + ".label", Code: "secret_field_label_not_string",
			Message: fmt.Sprintf("label: must be a string, got %T", v),
		}}
	}
	return s, nil
}

// scanSecretNodes records the relative path of EVERY `type: secret` node in the tree.
// The walk is structural rather than schema-aware — every map and every sequence
// element — so a position nobody anticipated is still found and reported instead of
// passing as "no secrets here".
//
// Subtrees holding DATA rather than schema nodes are skipped: a `default:` of
// `{type: secret}` is a value that happens to look like a declaration, and scanning it
// would reject a legal schema.
//
// inProps says the map being walked is a `properties`/`patternProperties` bag, whose
// keys are FIELD NAMES chosen by the author. The skip set must not apply there: a
// field legitimately named `default` or `enum` would take its whole subtree out of the
// walk, and the declaration inside it would come back as "no secrets here" — the one
// outcome this function's contract rules out. Skipping is a statement about schema
// keywords, and a schema keyword is defined by its position, not by its spelling.
func scanSecretNodes(node any, path string, out map[string]bool, inProps bool) {
	switch n := node.(type) {
	case map[string]any:
		if schemaNodeType(n) == SecretTypeName && path != "" {
			out[path] = true
		}
		for _, k := range sortedMapKeys(n) {
			if !inProps && secretScanSkip[k] {
				continue
			}
			scanSecretNodes(n[k], path+"."+k, out, !inProps && secretPropsBag[k])
		}
	case []any:
		for i, v := range n {
			scanSecretNodes(v, fmt.Sprintf("%s[%d]", path, i), out, false)
		}
	}
}

// secretScanSkip — keys whose subtree is data, not schema nodes. Applied only outside
// a properties bag (see scanSecretNodes).
var secretScanSkip = map[string]bool{"default": true, "const": true, "enum": true, "examples": true}

// secretPropsBag — keys whose VALUE is a map of author-chosen field names.
var secretPropsBag = map[string]bool{"properties": true, "patternProperties": true}

// schemaNodeType reads a JSON Schema node's `type`. "" when absent or non-string.
func schemaNodeType(node map[string]any) string {
	t, _ := node["type"].(string)
	return t
}

// stringSeq reads a `required`-style sequence of strings; anything else yields nothing
// (its own diagnostic is emitted by validateJSONSchemaNode).
func stringSeq(v any) []string {
	seq, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(seq))
	for _, e := range seq {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// sortedMapKeys returns a map's keys in sorted order — every walk over a schema must
// produce byte-identical output for identical input.
func sortedMapKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// StripDeclaredSecrets removes every `type: secret` value from a state record, in
// place ([ADR-0083] §4). The value of a declared secret lives in Vault; state
// carries the addressing (the element's `key` property) and nothing else.
//
// Called at the end of the state merge on BOTH paths — the scenario runner and the
// Trial twin — so a declared secret cannot reach `incarnation.state`,
// `state_history` or a Trial diff even if a task hands one back. That is belt and
// braces rather than the mechanism: `core.state.*` returns a `vault:`
// reference, so the plaintext should never be in the merged record to begin with.
//
// A schema issue is ignored here, deliberately. This is a stripper on the way out,
// not a validator: the declaration was already rejected at load time, and a merge
// that fails on a schema the loader accepted would take a run down for a reason the
// operator cannot act on. What a torn schema costs here is a field not stripped —
// and that field is masked by [audit.SecretPathSet] anyway.
func StripDeclaredSecrets(state, schema map[string]any) {
	if len(state) == 0 || len(schema) == 0 {
		return
	}
	fields, _ := CollectSecretFields(schema)
	for _, f := range fields {
		if !f.Collection() {
			delete(state, f.State)
			continue
		}
		for _, el := range collectionElements(state[f.State]) {
			delete(el, f.Property)
		}
	}
}

// collectionElements returns the object elements of a state collection. A list is
// the declared shape; a map is accepted too, because the state engine materializes
// a `map`-typed collection the same way and a stripper that silently skipped one of
// them would leave a secret in the record.
//
// A container that is neither is DESCENDED INTO rather than skipped, and the typed
// slice shape Go-built state produces is matched alongside the `[]any` a JSONB
// round-trip produces. The asymmetry of the two mistakes is the whole argument:
// descending one level too far can only delete a property the declaration named, in
// a record the declaration says holds a secret; skipping a shape leaves the
// plaintext in `incarnation.state` and `state_history` permanently. No cycle is
// reachable — state is decoded JSON.
func collectionElements(v any) []map[string]any {
	var out []map[string]any
	switch t := v.(type) {
	case []map[string]any:
		out = append(out, t...)
	case []any:
		for _, el := range t {
			out = append(out, elementOrDescend(el)...)
		}
	case map[string]any:
		for _, el := range t {
			out = append(out, elementOrDescend(el)...)
		}
	}
	return out
}

func elementOrDescend(el any) []map[string]any {
	if m, ok := el.(map[string]any); ok {
		return []map[string]any{m}
	}
	return collectionElements(el)
}
