package util_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func TestStringParam_Required(t *testing.T) {
	p := mustStruct(t, map[string]any{"name": "redis"})
	got, err := util.StringParam(p, "name")
	if err != nil || got != "redis" {
		t.Fatalf("StringParam name=%q err=%v", got, err)
	}
	if _, err := util.StringParam(p, "missing"); err == nil {
		t.Fatal("StringParam missing: want error")
	}
	if _, err := util.StringParam(nil, "name"); err == nil {
		t.Fatal("StringParam on nil: want error")
	}
}

func TestStringParam_RejectsNonString(t *testing.T) {
	p := mustStruct(t, map[string]any{"name": 42})
	if _, err := util.StringParam(p, "name"); err == nil {
		t.Fatal("StringParam on non-string: want error")
	}
}

func TestOptStringParam_NullAndAbsent(t *testing.T) {
	p := mustStruct(t, map[string]any{"version": nil})
	got, err := util.OptStringParam(p, "version")
	if err != nil || got != "" {
		t.Fatalf("OptStringParam null: got=%q err=%v", got, err)
	}
	got, err = util.OptStringParam(p, "missing")
	if err != nil || got != "" {
		t.Fatalf("OptStringParam missing: got=%q err=%v", got, err)
	}
}

func TestOptIntParam(t *testing.T) {
	p := mustStruct(t, map[string]any{"uid": float64(1000)})
	v, ok, err := util.OptIntParam(p, "uid")
	if err != nil || !ok || v != 1000 {
		t.Fatalf("OptIntParam: v=%d ok=%v err=%v", v, ok, err)
	}
	if _, ok, _ := util.OptIntParam(p, "missing"); ok {
		t.Fatal("OptIntParam missing: want ok=false")
	}
	pNonInt := mustStruct(t, map[string]any{"uid": float64(3.14)})
	if _, _, err := util.OptIntParam(pNonInt, "uid"); err == nil {
		t.Fatal("OptIntParam non-integer: want error")
	}
}

func TestOptBoolParam(t *testing.T) {
	p := mustStruct(t, map[string]any{
		"create":  true,
		"recurse": false,
		"null":    nil,
		"nonbool": "yes",
	})
	cases := []struct {
		key     string
		want    bool
		wantErr bool
	}{
		{"create", true, false},
		{"recurse", false, false},
		{"null", false, false},
		{"absent", false, false},
		{"nonbool", false, true},
	}
	for _, c := range cases {
		got, err := util.OptBoolParam(p, c.key)
		if c.wantErr {
			if err == nil {
				t.Errorf("OptBoolParam %q: want error, got nil", c.key)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("OptBoolParam %q: got=%v err=%v, want=%v", c.key, got, err, c.want)
		}
	}
}

func TestTriBoolParam(t *testing.T) {
	p := mustStruct(t, map[string]any{
		"yes":     true,
		"no":      false,
		"null":    nil,
		"nonbool": "yes",
	})
	cases := []struct {
		key         string
		wantVal     bool
		wantPresent bool
		wantErr     bool
	}{
		{"yes", true, true, false},
		{"no", false, true, false},
		{"null", false, false, false},
		{"absent", false, false, false},
		{"nonbool", false, false, true},
	}
	for _, c := range cases {
		val, present, err := util.TriBoolParam(p, c.key)
		if c.wantErr {
			if err == nil {
				t.Errorf("TriBoolParam %q: want error, got nil", c.key)
			}
			continue
		}
		if err != nil || val != c.wantVal || present != c.wantPresent {
			t.Errorf("TriBoolParam %q: val=%v present=%v err=%v, want val=%v present=%v",
				c.key, val, present, err, c.wantVal, c.wantPresent)
		}
	}

	if _, present, err := util.TriBoolParam(nil, "x"); present || err != nil {
		t.Fatalf("TriBoolParam(nil): present=%v err=%v", present, err)
	}
}

func TestOptStringSliceParam(t *testing.T) {
	p := mustStruct(t, map[string]any{"groups": []any{"wheel", "docker"}})
	got, err := util.OptStringSliceParam(p, "groups")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got) != 2 || got[0] != "wheel" || got[1] != "docker" {
		t.Fatalf("got=%v", got)
	}
	pMixed := mustStruct(t, map[string]any{"groups": []any{"wheel", 42}})
	if _, err := util.OptStringSliceParam(pMixed, "groups"); err == nil {
		t.Fatal("OptStringSliceParam mixed: want error")
	}
}

func TestOptIntSliceParam(t *testing.T) {
	p := mustStruct(t, map[string]any{"status_codes": []any{200, 204}})
	got, err := util.OptIntSliceParam(p, "status_codes")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got) != 2 || got[0] != 200 || got[1] != 204 {
		t.Fatalf("got=%v", got)
	}

	// Отсутствие ключа → nil без ошибки.
	if got, err := util.OptIntSliceParam(p, "missing"); err != nil || got != nil {
		t.Fatalf("absent: got=%v err=%v", got, err)
	}

	// Дробное число → ошибка (целостность как в OptIntParam).
	pFrac := mustStruct(t, map[string]any{"status_codes": []any{200.5}})
	if _, err := util.OptIntSliceParam(pFrac, "status_codes"); err == nil {
		t.Fatal("OptIntSliceParam float: want error")
	}

	// Не-список → ошибка.
	pScalar := mustStruct(t, map[string]any{"status_codes": 200})
	if _, err := util.OptIntSliceParam(pScalar, "status_codes"); err == nil {
		t.Fatal("OptIntSliceParam scalar: want error")
	}

	// Строка в списке → ошибка.
	pStr := mustStruct(t, map[string]any{"status_codes": []any{"200"}})
	if _, err := util.OptIntSliceParam(pStr, "status_codes"); err == nil {
		t.Fatal("OptIntSliceParam string element: want error")
	}
}

func TestOptStringParam_NilParams(t *testing.T) {
	got, err := util.OptStringParam(nil, "x")
	if err != nil || got != "" {
		t.Fatalf("OptStringParam(nil): got=%q err=%v", got, err)
	}
}

func TestOptStringParam_RejectsNonString(t *testing.T) {
	p := mustStruct(t, map[string]any{"version": float64(42)})
	if _, err := util.OptStringParam(p, "version"); err == nil {
		t.Fatal("OptStringParam non-string: want error")
	}
}

func TestOptIntParam_NilAndNull(t *testing.T) {
	if _, ok, err := util.OptIntParam(nil, "x"); ok || err != nil {
		t.Fatalf("OptIntParam(nil): ok=%v err=%v", ok, err)
	}
	p := mustStruct(t, map[string]any{"uid": nil})
	if _, ok, err := util.OptIntParam(p, "uid"); ok || err != nil {
		t.Fatalf("OptIntParam null: ok=%v err=%v", ok, err)
	}
}

func TestOptIntParam_RejectsNonNumber(t *testing.T) {
	p := mustStruct(t, map[string]any{"uid": "1000"})
	if _, _, err := util.OptIntParam(p, "uid"); err == nil {
		t.Fatal("OptIntParam string: want error")
	}
}

func TestOptBoolParam_NilParams(t *testing.T) {
	if got, err := util.OptBoolParam(nil, "x"); got || err != nil {
		t.Fatalf("OptBoolParam(nil): got=%v err=%v", got, err)
	}
}

func TestOptStringMapParam(t *testing.T) {
	p := mustStruct(t, map[string]any{
		"env": map[string]any{"FOO": "bar", "BAZ": "qux"},
	})
	got, err := util.OptStringMapParam(p, "env")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got) != 2 || got["FOO"] != "bar" || got["BAZ"] != "qux" {
		t.Fatalf("got=%v", got)
	}

	// Отсутствие ключа / null / nil params → nil без ошибки.
	if got, err := util.OptStringMapParam(p, "missing"); err != nil || got != nil {
		t.Fatalf("absent: got=%v err=%v", got, err)
	}
	if got, err := util.OptStringMapParam(nil, "env"); err != nil || got != nil {
		t.Fatalf("nil params: got=%v err=%v", got, err)
	}
	pNull := mustStruct(t, map[string]any{"env": nil})
	if got, err := util.OptStringMapParam(pNull, "env"); err != nil || got != nil {
		t.Fatalf("null: got=%v err=%v", got, err)
	}

	// Не-объект → ошибка.
	pScalar := mustStruct(t, map[string]any{"env": "FOO=bar"})
	if _, err := util.OptStringMapParam(pScalar, "env"); err == nil {
		t.Fatal("OptStringMapParam scalar: want error")
	}

	// Non-string значение внутри map → ошибка с указанием ключа.
	pMixed := mustStruct(t, map[string]any{"env": map[string]any{"FOO": float64(1)}})
	err = nil
	_, err = util.OptStringMapParam(pMixed, "env")
	if err == nil {
		t.Fatal("OptStringMapParam non-string value: want error")
	}
}

func TestOptStructMapParam(t *testing.T) {
	p := mustStruct(t, map[string]any{
		"vars": map[string]any{
			"name":    "redis",
			"port":    float64(6379),
			"enabled": true,
			"nested":  map[string]any{"k": "v"},
			"list":    []any{"a", "b"},
		},
	})
	got, err := util.OptStructMapParam(p, "vars")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got["name"] != "redis" {
		t.Errorf("name=%v want redis", got["name"])
	}
	if got["port"] != float64(6379) {
		t.Errorf("port=%v want 6379 (число сохраняется как float64)", got["port"])
	}
	if got["enabled"] != true {
		t.Errorf("enabled=%v want true", got["enabled"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["k"] != "v" {
		t.Errorf("nested=%v want вложенный map", got["nested"])
	}
	list, ok := got["list"].([]any)
	if !ok || len(list) != 2 {
		t.Errorf("list=%v want []any{a,b}", got["list"])
	}

	// Отсутствие / null / nil → nil без ошибки.
	if got, err := util.OptStructMapParam(p, "missing"); err != nil || got != nil {
		t.Fatalf("absent: got=%v err=%v", got, err)
	}
	if got, err := util.OptStructMapParam(nil, "vars"); err != nil || got != nil {
		t.Fatalf("nil params: got=%v err=%v", got, err)
	}
	pNull := mustStruct(t, map[string]any{"vars": nil})
	if got, err := util.OptStructMapParam(pNull, "vars"); err != nil || got != nil {
		t.Fatalf("null: got=%v err=%v", got, err)
	}

	// Не-объект (список/скаляр) → ошибка.
	pScalar := mustStruct(t, map[string]any{"vars": "x"})
	if _, err := util.OptStructMapParam(pScalar, "vars"); err == nil {
		t.Fatal("OptStructMapParam scalar: want error")
	}
	pList := mustStruct(t, map[string]any{"vars": []any{"x"}})
	if _, err := util.OptStructMapParam(pList, "vars"); err == nil {
		t.Fatal("OptStructMapParam list: want error")
	}
}

func TestOptStringSliceParam_EdgeCases(t *testing.T) {
	// nil params / отсутствие / null → nil без ошибки.
	if got, err := util.OptStringSliceParam(nil, "x"); err != nil || got != nil {
		t.Fatalf("nil params: got=%v err=%v", got, err)
	}
	p := mustStruct(t, map[string]any{"groups": nil})
	if got, err := util.OptStringSliceParam(p, "groups"); err != nil || got != nil {
		t.Fatalf("null: got=%v err=%v", got, err)
	}
	// Не-список → ошибка.
	pScalar := mustStruct(t, map[string]any{"groups": "wheel"})
	if _, err := util.OptStringSliceParam(pScalar, "groups"); err == nil {
		t.Fatal("OptStringSliceParam scalar: want error")
	}
}

func TestParamPresent(t *testing.T) {
	// nil params / отсутствие ключа / явный null → отсутствует.
	if util.ParamPresent(nil, "x") {
		t.Fatal("ParamPresent(nil) = true")
	}
	p := mustStruct(t, map[string]any{"content": "x", "empty": "", "null": nil})
	if util.ParamPresent(p, "missing") {
		t.Fatal("ParamPresent отсутствующего ключа = true")
	}
	if util.ParamPresent(p, "null") {
		t.Fatal("ParamPresent явного null = true (должен трактоваться как отсутствие)")
	}
	// Ключ присутствует — даже с пустой строкой (ключевое отличие от пустоты).
	if !util.ParamPresent(p, "content") {
		t.Fatal("ParamPresent заданного ключа = false")
	}
	if !util.ParamPresent(p, "empty") {
		t.Fatal("ParamPresent пустой строки = false (присутствие ключа != пустота значения)")
	}
}
