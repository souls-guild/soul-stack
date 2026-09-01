package herald

// THE INVARIANT, guarded where a Herald derives a Vault path ([ADR-0085],
// NIM-728): a Herald's `label` participates in nothing derived, and the
// `<entity>` segment of `secret/herald/<entity>/<field>` comes from its `name`
// alone.
//
// This registry is the sharpest instance in the platform, and that is why the
// guard lives here rather than only in a doc comment. `<mount>/herald/<entity>/
// <field>` takes its entity segment DIRECTLY from the registry row — one hop,
// with no state schema in between and nothing to notice — and
// `Writer.WriteKV` dispatches to `Put`, not `Patch`, so a write at a different
// path does not merge into the old one. A caption that reached that segment
// would therefore make every label edit relocate the channel's signing secret,
// leaving the value already issued unreachable, with no error raised anywhere:
// the new path is perfectly well-formed, it simply holds nothing.
//
// HOW TO BREAK IT ON PURPOSE (the mutation this file exists to catch):
// in materializeHeraldSecrets, pass `ptrStr(h.Label)` instead of `h.Name` as the
// entity argument to materializeField. Every test below goes red.
//
// The fixture captions are deliberately VALID identifiers — kebab, in range, and
// segment-safe — so that a naive substitution produces a perfectly well-formed
// path and no format check anywhere can be what fails. Only the assertions can
// catch it, which is the point: in production nothing else would.

import (
	"context"
	"strings"
	"testing"
)

const (
	// guardHeraldID is the identifier: what the derived path must be built from.
	guardHeraldID = "billing-webhook"
	// guardHeraldLabel is the caption. Different from the identifier, and itself a
	// well-formed one — see the file header for why that matters.
	guardHeraldLabel = "billing-webhook-prod"
)

// pathCapturingVault records the ENTITY segment of every write, which is the
// thing under test here (capturingVault in secret_leak_test.go records values).
type pathCapturingVault struct{ entities []string }

func (p *pathCapturingVault) WriteString(_ context.Context, domain, entity, field, _ string) (string, error) {
	p.entities = append(p.entities, entity)
	return "vault:secret/" + domain + "/" + entity + "/" + field + "#" + field, nil
}

// TestHeraldLabel_NotInDerivedVaultPath drives the real materialization with a
// Herald whose caption differs from its identifier, and pins both halves: the
// entity segment IS the identifier, and the caption appears in no segment and in
// no returned ref.
func TestHeraldLabel_NotInDerivedVaultPath(t *testing.T) {
	label := guardHeraldLabel
	secret := "s3cr3t-signing-value"
	h := &Herald{
		Name:   guardHeraldID,
		Label:  &label,
		Type:   HeraldWebhook,
		Config: map[string]any{"url": "https://example.test/hook"},
		Secret: &secret,
	}

	vault := &pathCapturingVault{}
	if err := materializeHeraldSecrets(context.Background(), vault, true, h); err != nil {
		t.Fatalf("materializeHeraldSecrets: %v", err)
	}

	if len(vault.entities) == 0 {
		t.Fatal("no Vault write happened — the guard would pass vacuously; the fixture must materialize a secret")
	}
	for i, entity := range vault.entities {
		if entity != guardHeraldID {
			t.Errorf("write %d: derived Vault entity segment is %q, want the IDENTIFIER %q\n"+
				"ADR-0085: a Herald's label participates in nothing derived. A caption in this "+
				"segment relocates secret/herald/<entity>/<field> on every label edit, and "+
				"secretwrite REPLACES rather than merges — the signing secret already issued "+
				"becomes unreachable, silently.", i, entity, guardHeraldID)
		}
		if strings.Contains(entity, guardHeraldLabel) {
			t.Errorf("write %d: the CAPTION %q reached the derived Vault path segment %q",
				i, guardHeraldLabel, entity)
		}
	}

	// The ref stored back on the row is the same address read back later; a
	// caption there is the same defect one indirection further out.
	if h.SecretRef == nil {
		t.Fatal("materialization left SecretRef nil — nothing to check")
	}
	if !strings.Contains(*h.SecretRef, "/"+guardHeraldID+"/") {
		t.Errorf("stored secret_ref %q does not address the identifier %q", *h.SecretRef, guardHeraldID)
	}
	if strings.Contains(*h.SecretRef, guardHeraldLabel) {
		t.Errorf("stored secret_ref %q carries the CAPTION %q", *h.SecretRef, guardHeraldLabel)
	}

	// The caption itself is untouched by the write path: it is display text, not
	// an input to anything.
	if h.Label == nil || *h.Label != guardHeraldLabel {
		t.Errorf("Label after materialization = %v, want it left alone as %q", h.Label, guardHeraldLabel)
	}
}

// TestHeraldLabel_DerivedPathIgnoresCaptionChanges is the invariant stated as the
// operator experiences it: change the caption, and every derived address stays
// byte-identical. Two runs of the real derivation, one with each caption.
func TestHeraldLabel_DerivedPathIgnoresCaptionChanges(t *testing.T) {
	derive := func(t *testing.T, label *string) []string {
		t.Helper()
		secret := "s3cr3t-signing-value"
		h := &Herald{
			Name:   guardHeraldID,
			Label:  label,
			Type:   HeraldWebhook,
			Config: map[string]any{"url": "https://example.test/hook"},
			Secret: &secret,
		}
		vault := &pathCapturingVault{}
		if err := materializeHeraldSecrets(context.Background(), vault, true, h); err != nil {
			t.Fatalf("materializeHeraldSecrets: %v", err)
		}
		return vault.entities
	}

	first := guardHeraldLabel
	second := "Billing Webhook — production"
	before := derive(t, &first)
	after := derive(t, &second)
	none := derive(t, nil)

	if len(before) != len(after) || len(before) != len(none) {
		t.Fatalf("different number of derived paths across captions: %d / %d / %d",
			len(before), len(after), len(none))
	}
	for i := range before {
		if before[i] != after[i] || before[i] != none[i] {
			t.Errorf("segment %d moved when the caption changed: %q → %q (and %q with no caption).\n"+
				"ADR-0085: \"I changed the label and nothing moved\" must be a guarantee, not a hope.",
				i, before[i], after[i], none[i])
		}
	}
}
