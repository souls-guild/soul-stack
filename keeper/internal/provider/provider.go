// Package provider is the Cloud Provider registry in Postgres (ADR-017,
// docs/keeper/cloud.md).
//
// Cloud.CRUD.a: types + CRUD (Insert / SelectByName / SelectAll). Provider is an
// API-managed record: CloudDriver plugin (`Type`), region, and vault-ref to
// credentials. The secret itself is not stored in the DB.
//
// Matching `Type` to keeper.yml::plugins.cloud_drivers[].name is checked at the
// service layer (Cloud.CRUD.b), not here. This layer only validates kebab format.
package provider

import (
	"regexp"
	"strings"
	"time"
)

// NamePattern is the canonical Provider name / valid `Type` form: kebab-case,
// length 1..63. Same as CHECK providers_name_format / providers_type_format in
// migration 019.
const NamePattern = `^[a-z0-9-]{1,63}$`

// CredentialsRefPrefix is the only vault-ref scheme supported in the MVP
// (recon-crud.md branch #2). env:/secret-store: is post-MVP ADR.
const CredentialsRefPrefix = "vault:"

var nameRe = regexp.MustCompile(NamePattern)

// ValidName checks that name matches the canonical form (kebab 1..63). Used for
// both Provider name and the `Type` field (CloudDriver plugin name).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// ValidCredentialsRef checks that ref starts with [CredentialsRefPrefix] and
// carries a non-empty path after it.
func ValidCredentialsRef(ref string) bool {
	return strings.HasPrefix(ref, CredentialsRefPrefix) &&
		len(ref) > len(CredentialsRefPrefix)
}

// FQDNSuffixPattern is the fqdn_suffix form (self-onboard option T, ADR-017(h)):
// DNS labels separated by dots, without leading/trailing dot or underscore
// (RFC-1035-compatible; Keeper joins `<name>-<index>.<suffix>` into a valid
// FQDN=SID). Mirrors CHECK providers_fqdn_suffix_format (migration 094).
const FQDNSuffixPattern = `^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`

var fqdnSuffixRe = regexp.MustCompile(FQDNSuffixPattern)

// ValidFQDNSuffix checks that a non-empty suffix matches [FQDNSuffixPattern]. The
// validator rejects empty suffixes: "no suffix" is encoded as NULL/nil, not an
// empty string, which would produce an FQDN with a trailing dot.
func ValidFQDNSuffix(suffix string) bool { return fqdnSuffixRe.MatchString(suffix) }

// Provider is the runtime representation of a `providers` registry row.
type Provider struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Region         string `json:"region"`
	CredentialsRef string `json:"credentials_ref"`
	// FQDNSuffix is the provider VM FQDN suffix (self-onboard option T,
	// ADR-017(h)): Keeper predicts SID=FQDN as
	// `<name>-<index>.<FQDNSuffix>`. nil means the provider has no predictable
	// FQDN and self-onboard is unavailable. No leading dot.
	FQDNSuffix   *string   `json:"fqdn_suffix,omitempty"`
	CreatedByAID *string   `json:"created_by_aid,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// SecretWritten is a request-scoped marker: Keeper wrote plaintext
	// credentials to Vault in this operation (ADR-064 audit event). json:"-";
	// read by audit payload (key plaintext_ingested), not stored in PG/View.
	SecretWritten bool `json:"-"`
}
