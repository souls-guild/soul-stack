package audit

// The one rule for TEXT. [MaskSecretsSealed] and friends (mask.go, mask_schema.go)
// answer "what does a structured payload look like once the secrets are out" —
// they walk maps and replace values. This file answers the narrower question that
// keeps coming up one call site at a time: a value is about to be interpolated
// into a human-readable string (a diagnostic message, a log line, an error), and
// its field is declared secret. What may the string say about it?
//
// Historically each surface answered on its own, and the answers drifted: the
// input-validation diagnostics rendered `<masked>` while every payload masker
// wrote [MaskedValue], and the config vault-ref diagnostics rendered the raw
// value with `%q` — which then reached durable `audit_log` through
// `config.reload_failed` (NIM-505). One rule, one token, one place to change.

// MaskDeclared returns what a human-readable string may say about raw: raw
// itself when the field is not declared secret (the value is the diagnostic),
// [MaskedValue] when it is.
//
// declaredSecret is the DECLARATION, not a guess — `input:`/`state_schema`
// `secret: true`, or a field whose contract is "this holds a credential" (a
// `*_ref` in the keeper config). The content-based and name-based layers
// ([MaskSecretsWithSchema] §7.4 layers 2 and 4) do not apply here: they answer
// "does this value LOOK like a secret", and a value that fails validation is
// exactly the value that does not look like one — a plaintext password typed
// where a vault-ref belongs matches no vault-ref regex, and the key name is not
// in the string being built. Declaration is the only layer that still holds when
// the value is malformed, which is precisely when diagnostics are written.
//
// Emitting [MaskedValue] rather than dropping the slot is deliberate: the reader
// learns a value was present and rejected, which is the half of the diagnostic
// that survives masking. The other half — WHERE — is carried by the path/line of
// the diagnostic, not by the message text.
func MaskDeclared(declaredSecret bool, raw string) string {
	if declaredSecret {
		return MaskedValue
	}
	return raw
}
