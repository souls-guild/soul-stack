package provider

// THE INVARIANT, guarded where a Provider derives a Vault path ([ADR-0085],
// NIM-728): a Provider's `label` participates in nothing derived, and the
// `<entity>` segment of `secret/provider/<entity>/credentials` comes from its
// `name` alone.
//
// One hop, like Herald, and with the same consequence: `Writer.WriteKV`
// dispatches to `Put`, not `Patch`, so a write at a moved path does not merge
// into the old one — a caption in that segment would relocate the cloud
// credentials on every label edit and leave what was already stored unreachable,
// with no error raised anywhere.
//
// HOW TO BREAK IT ON PURPOSE (the mutation this file exists to catch):
// in Service.resolveCredentials, pass `derefLabel(in.Label)` instead of `in.Name`
// to secretWriter.WriteMap. Both tests below go red.
//
// The fixture caption is deliberately a VALID identifier — kebab, in range,
// segment-safe — so a naive substitution produces a well-formed path and no
// format check can be what fails. Only the assertions catch it.

import (
	"context"
	"strings"
	"testing"
)

const (
	// guardProviderID is the identifier: what the derived path must be built from.
	guardProviderID = "aws-prod"
	// guardProviderLabel is the caption. Different, and itself well-formed.
	guardProviderLabel = "aws-prod-frankfurt"
)

// entityCapturingVault records the ENTITY segment of every write (fakeCredVault
// in secret_leak_test.go records the payload instead).
type entityCapturingVault struct{ entities []string }

func (v *entityCapturingVault) WriteMap(_ context.Context, domain, entity, field string, _ map[string]any) (string, error) {
	v.entities = append(v.entities, entity)
	return "vault:secret/" + domain + "/" + entity + "/" + field, nil
}

func guardLabelInput(label *string) CreateInput {
	return CreateInput{
		Name:        guardProviderID,
		Label:       label,
		Type:        "aws",
		Region:      "eu-west-1",
		Credentials: map[string]any{"access_key": "AKIA0000", "secret_key": "not-the-subject"},
	}
}

// TestProviderLabel_NotInDerivedVaultPath drives the real create path with a
// Provider whose caption differs from its identifier, and pins that the entity
// segment IS the identifier and the caption reaches no segment and no ref.
func TestProviderLabel_NotInDerivedVaultPath(t *testing.T) {
	label := guardProviderLabel
	v := &entityCapturingVault{}
	db := &fakeDB{}
	svc := newCredService(t, v, db, true)

	p, err := svc.Create(context.Background(), guardLabelInput(&label))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(v.entities) == 0 {
		t.Fatal("no Vault write happened — the guard would pass vacuously; the fixture must materialize credentials")
	}
	for i, entity := range v.entities {
		if entity != guardProviderID {
			t.Errorf("write %d: derived Vault entity segment is %q, want the IDENTIFIER %q\n"+
				"ADR-0085: a Provider's label participates in nothing derived. A caption in this "+
				"segment relocates secret/provider/<entity>/credentials on every label edit, and "+
				"secretwrite REPLACES rather than merges — the credentials already stored become "+
				"unreachable, silently.", i, entity, guardProviderID)
		}
		if strings.Contains(entity, guardProviderLabel) {
			t.Errorf("write %d: the CAPTION %q reached the derived Vault path segment %q",
				i, guardProviderLabel, entity)
		}
	}
	if !strings.Contains(p.CredentialsRef, "/"+guardProviderID+"/") {
		t.Errorf("stored credentials_ref %q does not address the identifier %q", p.CredentialsRef, guardProviderID)
	}
	if strings.Contains(p.CredentialsRef, guardProviderLabel) {
		t.Errorf("stored credentials_ref %q carries the CAPTION %q", p.CredentialsRef, guardProviderLabel)
	}
	if p.Label == nil || *p.Label != guardProviderLabel {
		t.Errorf("Label after create = %v, want it stored as given: %q", p.Label, guardProviderLabel)
	}
}

// TestProviderLabel_DerivedPathIgnoresCaptionChanges is the invariant as the
// operator experiences it: change the caption, and the derived address is
// byte-identical.
func TestProviderLabel_DerivedPathIgnoresCaptionChanges(t *testing.T) {
	derive := func(t *testing.T, label *string) string {
		t.Helper()
		v := &entityCapturingVault{}
		svc := newCredService(t, v, &fakeDB{}, true)
		p, err := svc.Create(context.Background(), guardLabelInput(label))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return p.CredentialsRef
	}

	first := guardProviderLabel
	second := "AWS — production (Frankfurt)"
	before := derive(t, &first)
	after := derive(t, &second)
	none := derive(t, nil)

	if before != after || before != none {
		t.Errorf("the derived credentials path moved when the caption changed: %q → %q (and %q with no caption).\n"+
			"ADR-0085: \"I changed the label and nothing moved\" must be a guarantee, not a hope.",
			before, after, none)
	}
}
