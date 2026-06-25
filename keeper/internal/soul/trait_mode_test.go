package soul

import (
	"errors"
	"testing"
)

func TestValidTraitMode(t *testing.T) {
	for _, m := range []TraitMode{TraitMerge, TraitReplace, TraitRemove} {
		if !ValidTraitMode(m) {
			t.Errorf("ValidTraitMode(%q) = false, want true", m)
		}
	}
	for _, m := range []TraitMode{"append", "remove-all", "", "MERGE"} {
		if ValidTraitMode(m) {
			t.Errorf("ValidTraitMode(%q) = true, want false", m)
		}
	}
}

func TestValidTraitKey(t *testing.T) {
	good := []string{"namespace", "dc-eu", "tier-1", "a", "x9-y9"}
	for _, k := range good {
		if !ValidTraitKey(k) {
			t.Errorf("ValidTraitKey(%q) = false, want true", k)
		}
	}
	bad := []string{"", "Namespace", "1tier", "dc_eu", "dc-", "-dc", "a.b", "dc--eu"}
	for _, k := range bad {
		if ValidTraitKey(k) {
			t.Errorf("ValidTraitKey(%q) = true, want false", k)
		}
	}
}

func TestValidTraitValue_Scalars(t *testing.T) {
	for _, v := range []any{"s", true, false, float64(3), int(7), int64(9)} {
		if err := ValidTraitValue(v); err != nil {
			t.Errorf("ValidTraitValue(%#v) = %v, want nil", v, err)
		}
	}
}

func TestValidTraitValue_ListOfScalars(t *testing.T) {
	if err := ValidTraitValue([]any{"a", float64(1), true}); err != nil {
		t.Errorf("ValidTraitValue(list of scalars) = %v, want nil", err)
	}
	if err := ValidTraitValue([]any{}); err != nil {
		t.Errorf("ValidTraitValue(empty list) = %v, want nil", err)
	}
}

// TestValidTraitValue_RejectsNested — вложенный map / список-в-списке / null
// отвергаются (depth > 1 запрещён, ADR-060).
func TestValidTraitValue_RejectsNested(t *testing.T) {
	cases := []any{
		map[string]any{"k": "v"},        // вложенный объект
		[]any{[]any{"x"}},               // список в списке
		[]any{map[string]any{"k": "v"}}, // объект в списке
		nil,                             // null
	}
	for _, v := range cases {
		if err := ValidTraitValue(v); err == nil {
			t.Errorf("ValidTraitValue(%#v) = nil, want error (nested/null rejected)", v)
		}
	}
}

func TestValidateTraitDelta(t *testing.T) {
	ok := map[string]any{"namespace": "dba", "tier": float64(1), "tags": []any{"a", "b"}}
	if err := ValidateTraitDelta(ok); err != nil {
		t.Errorf("ValidateTraitDelta(valid) = %v, want nil", err)
	}
	if err := ValidateTraitDelta(map[string]any{"Bad-Key": "v"}); err == nil {
		t.Error("ValidateTraitDelta(bad key) = nil, want error")
	}
	if err := ValidateTraitDelta(map[string]any{"k": map[string]any{"nested": 1}}); err == nil {
		t.Error("ValidateTraitDelta(nested value) = nil, want error")
	}
	// nil/пустой map допустим (replace = «очистить»).
	if err := ValidateTraitDelta(nil); err != nil {
		t.Errorf("ValidateTraitDelta(nil) = %v, want nil", err)
	}
}

func TestValidateTraitKeys(t *testing.T) {
	if err := ValidateTraitKeys([]string{"namespace", "tier"}); err != nil {
		t.Errorf("ValidateTraitKeys(valid) = %v, want nil", err)
	}
	if err := ValidateTraitKeys([]string{"ok", "Bad_Key"}); err == nil {
		t.Error("ValidateTraitKeys(bad key) = nil, want error")
	}
}

// --- bulk dispatch / validation-before-DB ---

func TestBulkAssignTraits_RejectsReplaceMode(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkAssignTraits(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, TraitReplace, map[string]any{"k": "v"}, nil)
	if err == nil {
		t.Fatal("BulkAssignTraits with replace mode returned nil err (replace вынесен в BulkReplaceTraits)")
	}
	if f.queryCalls != 0 || f.execCalls != 0 {
		t.Errorf("DB touched on unsupported mode (queries=%d execs=%d)", f.queryCalls, f.execCalls)
	}
}

func TestBulkAssignTraits_RejectsBadKey_NoDB(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkAssignTraits(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, TraitMerge, map[string]any{"Bad-Key": "v"}, nil)
	if err == nil {
		t.Fatal("BulkAssignTraits merge with bad key returned nil err")
	}
	if f.queryCalls != 0 || f.execCalls != 0 {
		t.Errorf("DB touched on invalid key (queries=%d execs=%d)", f.queryCalls, f.execCalls)
	}
}

func TestBulkAssignTraits_RejectsNestedValue_NoDB(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkAssignTraits(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, TraitMerge, map[string]any{"k": map[string]any{"x": 1}}, nil)
	if err == nil {
		t.Fatal("BulkAssignTraits merge with nested value returned nil err")
	}
	if f.queryCalls != 0 || f.execCalls != 0 {
		t.Errorf("DB touched on nested value (queries=%d execs=%d)", f.queryCalls, f.execCalls)
	}
}

func TestBulkAssignTraits_Remove_RejectsBadKey_NoDB(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkAssignTraits(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, TraitRemove, nil, []string{"Bad_Key"})
	if err == nil {
		t.Fatal("BulkAssignTraits remove with bad key returned nil err")
	}
	if f.queryCalls != 0 || f.execCalls != 0 {
		t.Errorf("DB touched on invalid remove key (queries=%d execs=%d)", f.queryCalls, f.execCalls)
	}
}

func TestBulkReplaceTraits_RejectsBadKey_NoDB(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkReplaceTraits(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, map[string]any{"Bad-Key": "v"})
	if err == nil {
		t.Fatal("BulkReplaceTraits with bad key returned nil err")
	}
	if f.queryCalls != 0 || f.execCalls != 0 {
		t.Errorf("DB touched on invalid key (queries=%d execs=%d)", f.queryCalls, f.execCalls)
	}
}

// TestBulkAssignTraits_EmptySelector — пустой selector → ErrBulkEmptySelector
// (тот же гейт, что у coven; проверяется CountBulkMatched через buildBulkWhere).
func TestBulkAssignTraits_EmptySelector(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkAssignTraits(nil, bulkFakePool{f}, BulkSelector{},
		BulkScope{Unrestricted: true}, TraitMerge, map[string]any{"k": "v"}, nil)
	if !errors.Is(err, ErrBulkEmptySelector) {
		t.Fatalf("err = %v, want ErrBulkEmptySelector", err)
	}
}

func TestMarshalTraitPayload(t *testing.T) {
	b, err := marshalTraitPayload(nil)
	if err != nil {
		t.Fatalf("marshalTraitPayload(nil): %v", err)
	}
	if string(b) != "{}" {
		t.Errorf("marshalTraitPayload(nil) = %q, want {}", b)
	}
	b, err = marshalTraitPayload(map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("marshalTraitPayload: %v", err)
	}
	if string(b) != `{"k":"v"}` {
		t.Errorf("marshalTraitPayload = %q, want {\"k\":\"v\"}", b)
	}
}
