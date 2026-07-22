package herald

// Dual-class channel model for Herald delivery (ADR-052 amendment, architect decision).
// HTTP-class (webhook/telegram/slack/mattermost/discord/custom):
// each type is a [channelDriver] with three responsibilities: (1) config validation per
// its field descriptor, (2) declare whether it uses top-level secret_ref
// (webhook only), (3) resolve delivery into ready [httpDelivery] (URL + method +
// body + headers + opt. signature + SSRF-opt-out-flags). SMTP-class (email) is
// NOT a channelDriver (no httpDelivery/HTTP-transport), lives on separate axis
// ([email.go], its own branch in [DeliveryWorker.deliver]).
//
// UNIFIED SSRF circuit: driver ONLY builds httpDelivery, guard
// (validateDeliveryEndpoint + guardedDeliveryClient, egress.go) and client.Do
// called by [DeliveryWorker.deliver]. New HTTP-type CANNOT bypass SSRF-guard
// by construction.
//
// UNIFIED source of types: [channelDrivers] (+ HeraldEmail) — from it derived
// (1) generic-validator of config per descriptor, (2) list of types for huma-enum,
// (3) PG-CHECK-validation, (4) catalog GET /v1/herald-types. Adding HTTP-type is
// one entry in [channelDrivers] (+ CHECK-migration + huma-enum, verified
// by guard-test with [AllHeraldTypes]).
//
// Names channelDriver / httpDelivery / HeraldFieldSpec / FieldKind are fixed
// in naming-rules (ADR-052 amendment).

import (
	"context"
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// channelDriver is a driver for one HTTP-class-type channel. validateConfig
// checks config at CRUD phase (field form + domain invariants), without
// reading Vault (secret held as vault-ref, resolved only at delivery time).
// secretRequired returns whether type uses top-level secret_ref (HMAC signing-token);
// true only for webhook, for others credential is a vault-ref-field INSIDE config
// (split ADR-052 amendment). resolveDelivery resolves channel into ready
// httpDelivery at delivery time (config may have changed after create).
type channelDriver interface {
	// validateConfig is CRUD-validation of config per type (form + domain invariants).
	validateConfig(config map[string]any) error
	// secretRequired returns true if type uses top-level secret_ref (webhook).
	secretRequired() bool
	// resolveDelivery builds httpDelivery at delivery time. Errors from secret
	// resolution from Vault are passed through (caller preserves terminal/transient
	// classification: Vault failure is transient, bad config is terminal).
	resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error)
	// fields returns descriptor of config-fields for type (single source for generic-validator
	// and catalog GET /v1/herald-types; catalog and validation do NOT diverge).
	fields() []HeraldFieldSpec
}

// httpDelivery is the result of resolved HTTP-class delivery: request template
// (url + method + body + headers) + SSRF-opt-out-flags + opt. signing-key for
// HMAC-signature. [DeliveryWorker.deliver] builds *http.Request from these fields,
// runs UNIFIED SSRF-guard on url and optionally signs the body.
//
// httpAllowed/allowPrivate are per-Herald opt-out (webhook from config; fixed
// public endpoints telegram/slack/… have both false). signingKey nil means no
// signature (webhook only with secret_ref). method empty defaults to POST; contentType empty defaults to
// application/json (normalized by deliver()).
type httpDelivery struct {
	url          string
	method       string
	contentType  string
	body         []byte
	headers      map[string]string
	httpAllowed  bool
	allowPrivate bool
	signingKey   []byte
}

// channelDrivers is the canonical registry of HTTP-class drivers, SINGLE source
// of HTTP types. email is NOT here (SMTP-class, separate axis). New HTTP type
// is added with ONE entry here (+ CHECK-migration + huma-enum, both verified
// by guard-test with AllHeraldTypes).
var channelDrivers = map[HeraldType]channelDriver{
	HeraldWebhook:    webhookChannel{},
	HeraldTelegram:   telegramChannel{},
	HeraldSlack:      slackChannel{},
	HeraldMattermost: mattermostChannel{},
	HeraldDiscord:    discordChannel{},
	HeraldCustom:     customChannel{},
}

// driverFor returns the HTTP-class driver for a type. ok=false means type is not
// HTTP-class (email) or unknown. Caller ([DeliveryWorker.deliver]) for email
// takes its own SMTP branch before driverFor; for unknown — terminal-fail.
func driverFor(t HeraldType) (channelDriver, bool) {
	d, ok := channelDrivers[t]
	return d, ok
}

// resolveDelivery dispatches HTTP-class delivery resolution by channel type.
// Single entry point for [DeliveryWorker.deliver] (HTTP branch). Unknown /
// non-HTTP type → terminal-no-retry (nothing to deliver). Email does not reach
// here (deliver routes it to SMTP branch first).
func resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error) {
	d, ok := driverFor(h.Type)
	if !ok {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: channel %q type %q has no HTTP driver", h.Name, h.Type)}
	}
	return d.resolveDelivery(ctx, h, job, kv)
}

// --- config field descriptor (unified source of validation + catalog) ----------

// FieldKind is the kind of config field value for generic validator and catalog
// (name under confirmation). string/int/bool/enum are scalars; map/list are
// containers; url is URL string (SSRF guard lives in delivery, here — "string");
// list_string is list of strings (email to); vault_ref is vault-ref string
// (value must parse via vault.ParseRef, secret is not stored cleartext in PG).
type FieldKind string

const (
	KindString     FieldKind = "string"
	KindInt        FieldKind = "int"
	KindBool       FieldKind = "bool"
	KindEnum       FieldKind = "enum"
	KindMap        FieldKind = "map"
	KindList       FieldKind = "list"
	KindListString FieldKind = "list_string"
	KindURL        FieldKind = "url"
	KindVaultRef   FieldKind = "vault_ref"
)

// HeraldFieldSpec describes one config field of a type (name under confirmation).
// Secret=true ⟹ field holds vault-ref (Kind must be KindVaultRef; secret
// in config, not in top-level secret_ref — ADR-052 amendment routing). EnumValues
// filled only for Kind==KindEnum (allowed set of strings; empty element
// "" in set = "field omitted/plain" is allowed).
type HeraldFieldSpec struct {
	Name       string
	Label      string
	Required   bool
	Secret     bool
	Kind       FieldKind
	EnumValues []string
}

// validateBySpec is a generic traversal of field descriptors: required-presence +
// kind-match + secret-field → valid vault-ref. Unknown config keys are NOT
// rejected (forward-compat: JSONB may come from newer version; huma-level
// strips unknown fields on wire, domain remains tolerant).
func validateBySpec(t HeraldType, fields []HeraldFieldSpec, config map[string]any) error {
	for _, f := range fields {
		raw, present := config[f.Name]
		if !present {
			if f.Required {
				return fmt.Errorf("herald: %s config requires %q", t, f.Name)
			}
			continue
		}
		if err := checkFieldKind(t, f, raw); err != nil {
			return err
		}
	}
	return nil
}

// checkFieldKind checks one present field by its Kind. Empty string in
// required field is treated as missing (required violated). For
// vault_ref, additionally checks vault.ParseRef (secret-field must be
// vault-ref, not plaintext).
func checkFieldKind(t HeraldType, f HeraldFieldSpec, raw any) error {
	switch f.Kind {
	case KindString, KindURL:
		s, ok := raw.(string)
		if !ok || (f.Required && s == "") {
			return fmt.Errorf("herald: %s config %q must be a non-empty string", t, f.Name)
		}
	case KindVaultRef:
		s, ok := raw.(string)
		if !ok || s == "" {
			return fmt.Errorf("herald: %s config %q must be a non-empty vault-ref", t, f.Name)
		}
		if _, err := vault.ParseRef(s); err != nil {
			return fmt.Errorf("herald: %s config %q must be a vault-ref (vault:<mount>/<path>): %w", t, f.Name, err)
		}
	case KindEnum:
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("herald: %s config %q must be a string", t, f.Name)
		}
		if !containsString(f.EnumValues, s) {
			return fmt.Errorf("herald: %s config %q must be one of %v, got %q", t, f.Name, f.EnumValues, s)
		}
	case KindBool:
		if _, ok := raw.(bool); !ok {
			return fmt.Errorf("herald: %s config %q must be a boolean", t, f.Name)
		}
	case KindInt:
		if !isJSONInt(raw) {
			return fmt.Errorf("herald: %s config %q must be a number", t, f.Name)
		}
	case KindMap:
		if _, ok := raw.(map[string]any); !ok {
			return fmt.Errorf("herald: %s config %q must be an object", t, f.Name)
		}
	case KindList:
		if _, ok := raw.([]any); !ok {
			return fmt.Errorf("herald: %s config %q must be an array", t, f.Name)
		}
	case KindListString:
		xs, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("herald: %s config %q must be an array of strings", t, f.Name)
		}
		if f.Required && len(xs) == 0 {
			return fmt.Errorf("herald: %s config %q must be a non-empty array of strings", t, f.Name)
		}
		for _, el := range xs {
			s, ok := el.(string)
			if !ok || s == "" {
				return fmt.Errorf("herald: %s config %q must contain only non-empty strings", t, f.Name)
			}
		}
	}
	return nil
}

// isJSONInt is JSON number (float64 from decode, or native int/int64). We don't
// strictly check integrity (ports are validated by domain hook for email separately).
func isJSONInt(raw any) bool {
	switch raw.(type) {
	case float64, int, int64:
		return true
	default:
		return false
	}
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// AllHeraldTypes returns sorted list of ALL known channel types (HTTP-class
// from channelDrivers + email). SINGLE source for huma-enum verification, PG-CHECK
// verification and catalog; guard-test catches divergence. Sorting determines
// enum string.
func AllHeraldTypes() []HeraldType {
	out := make([]HeraldType, 0, len(channelDrivers)+1)
	for t := range channelDrivers {
		out = append(out, t)
	}
	out = append(out, HeraldEmail)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// fieldsFor returns config field descriptor for a type for catalog GET /v1/herald-
// types. HTTP-class takes from driver; email from [emailFields] (SMTP axis outside
// channelDrivers). ok=false means unknown type.
func fieldsFor(t HeraldType) ([]HeraldFieldSpec, bool) {
	if d, ok := channelDrivers[t]; ok {
		return d.fields(), true
	}
	if t == HeraldEmail {
		return emailFields(), true
	}
	return nil, false
}

// HeraldTypeDescriptor is public descriptor of one channel type for catalog
// endpoint GET /v1/herald-types: type + its config fields + top-level
// secret_ref indicator. SINGLE source with validation (same [HeraldFieldSpec]
// that CRUD validates; SecretRequired is same [channelDriver.secretRequired]
// that [ValidateSecretRef] checks) — catalog and validation don't diverge.
// SecretRequired=true ⟹ type has top-level secret_ref (HMAC signing-token,
// webhook only); UI shows secret_ref field by this indicator, not by type
// hardcode.
type HeraldTypeDescriptor struct {
	Type           HeraldType
	Fields         []HeraldFieldSpec
	SecretRequired bool
}

// TypeCatalog collects descriptors of ALL known channel types (sorted as
// [AllHeraldTypes]) for catalog endpoint. Field source is drivers (HTTP-class)
// and [emailFields] (SMTP); same set that CRUD validates. SecretRequired taken
// from driver (email is not channelDriver, doesn't use top-level secret_ref → false).
func TypeCatalog() []HeraldTypeDescriptor {
	types := AllHeraldTypes()
	out := make([]HeraldTypeDescriptor, 0, len(types))
	for _, t := range types {
		fields, _ := fieldsFor(t) // t in AllHeraldTypes means descriptor always exists
		d, ok := driverFor(t)     // email is not HTTP class, so ok=false and secret_ref is not for it
		out = append(out, HeraldTypeDescriptor{Type: t, Fields: fields, SecretRequired: ok && d.secretRequired()})
	}
	return out
}
