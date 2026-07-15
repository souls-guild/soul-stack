package applyrun

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// TestRecipe_MarshalUnmarshalRoundtrip — a recipe roundtrip through the jsonb form.
// The key check is invariant A: the vault-ref in Input is preserved as a STRING,
// unresolved (marshal/unmarshal do not interpret the `vault:` reference).
func TestRecipe_MarshalUnmarshalRoundtrip(t *testing.T) {
	aid := "archon-alice"
	const vaultRef = "vault:secret/data/db#password"
	in := &Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "web", Git: "https://git/web", Ref: "v1.2.0"},
		ScenarioName: "add_user",
		Input: map[string]any{
			"db_password": vaultRef,
			"username":    "svc",
			"port":        float64(5432),
		},
		StartedByAID: &aid,
	}

	b, err := MarshalRecipe(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The vault-ref must sit in jsonb as-is — unresolved, unmasked.
	if !strings.Contains(string(b), vaultRef) {
		t.Fatalf("marshalled recipe lost vault-ref verbatim; got: %s", b)
	}

	out, err := UnmarshalRecipe(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out == nil {
		t.Fatal("unmarshal returned nil for non-empty recipe")
	}
	if out.ServiceRef != in.ServiceRef {
		t.Errorf("ServiceRef = %+v, want %+v", out.ServiceRef, in.ServiceRef)
	}
	if out.ScenarioName != in.ScenarioName {
		t.Errorf("ScenarioName = %q, want %q", out.ScenarioName, in.ScenarioName)
	}
	if got := out.Input["db_password"]; got != vaultRef {
		t.Errorf("Input[db_password] = %v, want %q (vault-ref as-is)", got, vaultRef)
	}
	if got := out.Input["username"]; got != "svc" {
		t.Errorf("Input[username] = %v, want %q", got, "svc")
	}
	if out.StartedByAID == nil || *out.StartedByAID != aid {
		t.Errorf("StartedByAID = %v, want %q", out.StartedByAID, aid)
	}
}

// TestMarshalRecipe_Nil — the old Insert(running) path carries no recipe:
// a nil recipe → (nil, nil) → SQL NULL in the column.
func TestMarshalRecipe_Nil(t *testing.T) {
	b, err := MarshalRecipe(nil)
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if b != nil {
		t.Errorf("MarshalRecipe(nil) = %v, want nil (→ SQL NULL)", b)
	}
}

// TestUnmarshalRecipe_Empty — SQL NULL (nil / empty bytes) from old rows'
// column → (nil, nil), not a parser error.
func TestUnmarshalRecipe_Empty(t *testing.T) {
	for _, in := range [][]byte{nil, {}} {
		r, err := UnmarshalRecipe(in)
		if err != nil {
			t.Fatalf("unmarshal %v: %v", in, err)
		}
		if r != nil {
			t.Errorf("UnmarshalRecipe(%v) = %+v, want nil", in, r)
		}
	}
}

// TestRecipe_FromUpgradeRoundtrip — jsonb persistence of FromUpgrade (ADR-0068):
// at claim the Acolyte must read the flag and render upgrade/<slug>/, not
// scenario/. true → present in jsonb; the false default is omitted (omitempty,
// forward-compat with old recipes).
func TestRecipe_FromUpgradeRoundtrip(t *testing.T) {
	in := &Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "redis", Ref: "v2.0.0"},
		ScenarioName: "to_v2",
		FromUpgrade:  true,
	}
	b, err := MarshalRecipe(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"from_upgrade":true`) {
		t.Fatalf("marshalled recipe lost from_upgrade; got: %s", b)
	}
	out, err := UnmarshalRecipe(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.FromUpgrade {
		t.Errorf("FromUpgrade = false, want true (round-trip)")
	}

	// The false default — omitempty drops the key (forward-compat: old recipes
	// without the field read as false → the normal scenario/ path).
	legacy := &Recipe{ServiceRef: artifact.ServiceRef{Name: "redis", Ref: "v1"}, ScenarioName: "create"}
	lb, err := MarshalRecipe(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if strings.Contains(string(lb), "from_upgrade") {
		t.Errorf("legacy recipe carries from_upgrade key (must be omitempty): %s", lb)
	}
}

// TestUnmarshalRecipe_NilInput — a scenario recipe without input (Input nil)
// round-trips correctly.
func TestUnmarshalRecipe_NilInput(t *testing.T) {
	in := &Recipe{
		ServiceRef:   artifact.ServiceRef{Name: "noop", Ref: "main"},
		ScenarioName: "create",
	}
	b, err := MarshalRecipe(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := UnmarshalRecipe(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out == nil || out.ScenarioName != "create" {
		t.Fatalf("roundtrip lost recipe: %+v", out)
	}
	if out.Input != nil {
		t.Errorf("Input = %v, want nil", out.Input)
	}
	if out.StartedByAID != nil {
		t.Errorf("StartedByAID = %v, want nil", out.StartedByAID)
	}
}
