package applyrun

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// TestRecipe_MarshalUnmarshalRoundtrip — roundtrip рецепта через jsonb-форму.
// Ключевая проверка инварианта A: vault-ref в Input сохраняется как СТРОКА
// без раскрытия (marshal/unmarshal не интерпретируют `vault:`-ссылку).
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
	// vault-ref должен лежать в jsonb как есть — не раскрыт, не замаскирован.
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

// TestMarshalRecipe_Nil — старый путь Insert(running) рецепт не несёт:
// nil-рецепт → (nil, nil) → SQL NULL в колонке.
func TestMarshalRecipe_Nil(t *testing.T) {
	b, err := MarshalRecipe(nil)
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if b != nil {
		t.Errorf("MarshalRecipe(nil) = %v, want nil (→ SQL NULL)", b)
	}
}

// TestUnmarshalRecipe_Empty — SQL NULL (nil / пустые байты) из колонки старых
// строк → (nil, nil), не ошибка парсера.
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

// TestRecipe_FromUpgradeRoundtrip — jsonb-персистенция FromUpgrade (ADR-0068):
// Acolyte при claim обязан прочитать флаг и рендерить upgrade/<slug>/, а не
// scenario/. true → присутствует в jsonb; дефолт false опущен (omitempty,
// forward-compat со старыми рецептами).
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

	// Дефолт false — omitempty опускает ключ (forward-compat: старые рецепты без
	// поля читаются как false → обычный scenario/-путь).
	legacy := &Recipe{ServiceRef: artifact.ServiceRef{Name: "redis", Ref: "v1"}, ScenarioName: "create"}
	lb, err := MarshalRecipe(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if strings.Contains(string(lb), "from_upgrade") {
		t.Errorf("legacy recipe carries from_upgrade key (must be omitempty): %s", lb)
	}
}

// TestUnmarshalRecipe_NilInput — рецепт сценария без input (Input nil)
// roundtrip-ит корректно.
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
