package config

// The single renderer for "this field must hold a vault-ref and does not".
//
// There are two vault-ref validators in this package — [checkVaultRef]
// (semantic.go, phase semantic-validate, code `vault_ref_invalid_format`) and
// [isVaultRef] (schema.go, phase schema-validate, code `vault_ref_invalid`) —
// with different grammars, phases and codes, so they are not one function. Their
// MESSAGE is one function, here, and it is the part NIM-505 is about: every one
// of the 13 fields between them used to interpolate the rejected value with `%q`,
// and a keeper-config diagnostic does not stay in the operator's terminal. It
// travels: `Store.Reload` → [audit.FormatDiagnostics] → the `config.reload_failed`
// payload → `audit_log`, which is append-only and kept for 365 days. A password
// pasted where a vault-ref belongs outlived the incident that produced it, in a
// table nothing prunes for a year and everything reads.
//
// The fix is not "reword the message" — a message can be reworded back. It is
// that the renderer below cannot see the value: it takes the field and the
// expected form and nothing else, so no call site is able to leak one whether it
// remembers to or not. [audit.MaskDeclared] states the rule the placeholder comes
// from; a `*_ref` field is declared-secret by its contract (it names a Vault
// location for a credential), so the answer is always [audit.MaskedValue].

import (
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// The two accepted vault-ref forms, as the operator should read them back. They
// differ because the two validators differ: semantic-validate accepts an opaque
// path with an optional `#field` selector, schema-validate additionally demands a
// `<mount>/<path>` split. Unifying the GRAMMARS is a contract change (the codes
// are in docs/naming-rules.md) and is out of scope here — NIM-571.
const (
	vaultRefFormPath  = "vault:<path>[#<field>]"
	vaultRefFormMount = "vault:<mount>/<path>"
)

// vaultRefMessage renders the message for a `*_ref` field that does not hold a
// vault-ref. field is the operator-facing dotted field name (`postgres.dsn_ref`),
// form one of the vaultRefForm* constants.
//
// The rejected VALUE IS NOT A PARAMETER, by design — see the file comment. The
// placeholder is [audit.MaskedValue], the token [audit.MaskDeclared] answers with
// and the payload maskers write, so an operator who greps an audit entry for the
// mask finds every surface at once instead of two spellings.
func vaultRefMessage(field, form string) string {
	return fmt.Sprintf("%s must be a vault-ref (%s), got %s",
		field, form, audit.MaskedValue)
}

// fieldFromYAMLPath turns the `$.a.b` yaml-path used for AST position lookup into
// the `a.b` field name a message reads with. Paths in this package always carry
// the `$.` root prefix; anything else is passed through untouched.
func fieldFromYAMLPath(yamlPath string) string {
	return strings.TrimPrefix(yamlPath, "$.")
}
