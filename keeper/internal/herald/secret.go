package herald

// Dual-mode ingestion of Herald secret (ADR-064, NIM-11). Operator passes secret
// by value (plaintext) instead of vault-ref: top-level webhook signing-secret
// (Secret XOR SecretRef) and channel config fields (<base> XOR <base>_ref for each
// Secret field of type descriptor — telegram bot_token, slack/discord webhook_url,
// custom header_secret). Keeper writes plaintext to Vault at deterministic
// path secret/herald/<name>/<field> and replaces with internal ref; plaintext does NOT
// go to PG/logs/audit/View.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/secretwrite"
)

// SecretWriter is narrow surface for materializing plaintext secret to Vault
// (implemented by *secretwrite.Writer). nil → dual-mode plaintext unavailable.
type SecretWriter interface {
	WriteString(ctx context.Context, domain, entity, field, value string) (string, error)
}

// ErrPlaintextDisabled is returned when operator passes plaintext secret but ingestion
// is disabled (ADR-064 mitigation a: requires TLS-front Operator API/MCP + secret_ingest.
// accept_plaintext). Wrapped in [ErrValidation] → 422.
var ErrPlaintextDisabled = errors.New("plaintext secret ingestion disabled (enable secret_ingest.accept_plaintext on a TLS-fronted Operator API, or provide a *_ref)")

// materializeHeraldSecrets converts Herald record plaintext secrets to Vault at
// deterministic path secret/herald/<name>/<field>, replacing with internal
// vault-ref (ADR-064). Processes top-level Secret and config fields <base> for
// each Secret field <base>_ref of channel type. plaintext erased from h after
// write (doesn't go to PG/audit/View). XOR invariant: exactly one of value/ref for
// each secret field.
//
// Called by Service before Insert/Update. plaintext + accept=false (or w=nil)
// → [ErrPlaintextDisabled]. Errors don't carry secret value.
func materializeHeraldSecrets(ctx context.Context, w SecretWriter, accept bool, h *Herald) error {
	if h == nil {
		return fmt.Errorf("herald: nil herald")
	}
	// entity=<name> must be safe path segment before Vault write
	// (materializeField writes secret/herald/<name>/…). Name format checked
	// here before write; validateHerald at Insert will check again, but write-path needs it first.
	if !ValidName(h.Name) {
		return wrapValidation(fmt.Errorf("invalid name %q (must match %s)", h.Name, NamePattern))
	}

	wrote := false

	// --- top-level webhook signing secret (Secret XOR SecretRef) ---
	did, err := materializeField(ctx, w, accept, h.Name, "secret",
		ptrStr(h.Secret), ptrStr(h.SecretRef),
		func(ref string) { h.SecretRef = &ref })
	if err != nil {
		return err
	}
	h.Secret = nil // plaintext erased
	wrote = wrote || did

	// --- config fields of channel (<base> XOR <base>_ref for each Secret-field) ---
	fields, ok := fieldsFor(h.Type)
	if ok {
		for _, f := range fields {
			if !f.Secret {
				continue
			}
			base := strings.TrimSuffix(f.Name, "_ref") // bot_token_ref → bot_token
			if base == f.Name {
				continue // not *_ref field (guard; all Secret fields of descriptor are *_ref)
			}
			plainVal, _ := h.Config[base].(string)
			refVal, _ := h.Config[f.Name].(string)
			refField := f.Name
			did, err := materializeField(ctx, w, accept, h.Name, base,
				plainVal, refVal,
				func(ref string) { h.Config[refField] = ref })
			if err != nil {
				return err
			}
			delete(h.Config, base) // plaintext erased from config (even junk value)
			wrote = wrote || did
		}
	}

	h.SecretWritten = wrote
	return nil
}

// materializeField processes one secret field: value(plaintext) XOR ref.
// value set → writes to Vault (secret/herald/<entity>/<field>) and calls
// setRef(ref); returns did=true. Empty/ref only → no-op (existing behavior). Both set
// → [ErrValidation]. plaintext + !accept (or w=nil) → [ErrPlaintextDisabled].
// Secret value doesn't leak to error.
func materializeField(ctx context.Context, w SecretWriter, accept bool, entity, field, value, ref string, setRef func(string)) (bool, error) {
	hasValue := value != ""
	hasRef := ref != ""
	if hasValue && hasRef {
		return false, wrapValidation(fmt.Errorf("%s and %s_ref are mutually exclusive (provide exactly one)", field, field))
	}
	if !hasValue {
		return false, nil
	}
	if !accept || w == nil {
		return false, wrapValidation(ErrPlaintextDisabled)
	}
	newRef, err := w.WriteString(ctx, secretwrite.DomainHerald, entity, field, value)
	if err != nil {
		// err from secretwrite doesn't carry secret value.
		return false, fmt.Errorf("herald: materialize %s secret: %w", field, err)
	}
	setRef(newRef)
	return true, nil
}

// ptrStr dereferences *string to "" if nil.
func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
