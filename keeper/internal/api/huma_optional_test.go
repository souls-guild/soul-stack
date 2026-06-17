package api

// Guard-тесты generic Optional[T] (ADR-054 §Pattern третий tier). Три уровня:
//
//   - UnmarshalJSON — три ветки presence (omitted / explicit null / value);
//   - Schema(r) — huma-SchemaProvider даёт nullable-схему вложенного T БЕЗ
//     octet-stream artifact (ключевой инвариант tier-а против RawBody []byte-моста);
//   - round-trip через РЕАЛЬНУЮ huma-input-привязку (chi+huma) — presence долетает
//     из тела запроса в Optional[T] так же, как в проде (huma зовёт UnmarshalJSON
//     только когда ключ физически в теле).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
)

// TestOptional_UnmarshalJSON_ThreeBranches — json-декод поля Optional[string]
// внутри объекта: omitted (метод не вызван → Set=false), explicit null
// (Set=true, Null=true, zero-value), value (Set=true, Null=false, Value=x).
func TestOptional_UnmarshalJSON_ThreeBranches(t *testing.T) {
	type wrap struct {
		Field Optional[string] `json:"field"`
	}

	t.Run("omitted", func(t *testing.T) {
		var w wrap
		if err := json.Unmarshal([]byte(`{}`), &w); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if w.Field.Set {
			t.Errorf("omitted: Set=%v, want false (UnmarshalJSON не должен вызываться)", w.Field.Set)
		}
		if w.Field.Null {
			t.Errorf("omitted: Null=%v, want false", w.Field.Null)
		}
		if w.Field.Value != "" {
			t.Errorf("omitted: Value=%q, want zero-value", w.Field.Value)
		}
	})

	t.Run("explicit null", func(t *testing.T) {
		var w wrap
		if err := json.Unmarshal([]byte(`{"field":null}`), &w); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !w.Field.Set {
			t.Errorf("null: Set=%v, want true (ключ присутствует)", w.Field.Set)
		}
		if !w.Field.Null {
			t.Errorf("null: Null=%v, want true", w.Field.Null)
		}
		if w.Field.Value != "" {
			t.Errorf("null: Value=%q, want zero-value", w.Field.Value)
		}
	})

	t.Run("value", func(t *testing.T) {
		var w wrap
		if err := json.Unmarshal([]byte(`{"field":"coven=prod"}`), &w); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !w.Field.Set {
			t.Errorf("value: Set=%v, want true", w.Field.Set)
		}
		if w.Field.Null {
			t.Errorf("value: Null=%v, want false", w.Field.Null)
		}
		if w.Field.Value != "coven=prod" {
			t.Errorf("value: Value=%q, want coven=prod", w.Field.Value)
		}
	})
}

// TestOptional_optionalToPtr — конверт в *T для доменного presence-моста: задано
// не-null → указатель на значение; omitted/null → nil (presence несёт отдельно Set).
func TestOptional_optionalToPtr(t *testing.T) {
	if p := optionalToPtr(Optional[string]{}); p != nil {
		t.Errorf("omitted: optionalToPtr=%v, want nil", *p)
	}
	if p := optionalToPtr(Optional[string]{Set: true, Null: true}); p != nil {
		t.Errorf("null: optionalToPtr=%v, want nil", *p)
	}
	if p := optionalToPtr(Optional[string]{Set: true, Value: "x"}); p == nil || *p != "x" {
		t.Errorf("value: optionalToPtr=%v, want ptr to x", p)
	}
}

// TestOptional_Schema_NullableNoOctetStream — huma.SchemaProvider Optional[string]
// даёт схему вложенного string с Nullable:true (3.1 `type: [string, null]`), БЕЗ
// октет-потока. Прямой ассерт на возвращённую *huma.Schema.
func TestOptional_Schema_NullableNoOctetStream(t *testing.T) {
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	s := Optional[string]{}.Schema(reg)

	if s == nil {
		t.Fatal("Schema(r) вернул nil")
	}
	if s.Type != huma.TypeString {
		t.Errorf("Schema.Type = %q, want %q (схема вложенного T, не octet-stream/object)", s.Type, huma.TypeString)
	}
	if !s.Nullable {
		t.Errorf("Schema.Nullable = %v, want true (presence-сброс через null)", s.Nullable)
	}
	// octet-stream живёт на уровне requestBody-MIME (не на schema) — его отсутствие
	// сторожит golden OpenAPI-фрагмента (TestHumaRole_PatchPermissions_RequestBody_JSONOnly).
	// Здесь сторожим, что схема — чистый скаляр без binary-кодировки (рецидив RawBody []byte
	// дал бы ContentEncoding:"base64"/Format:"binary", а не nullable-string).
	if s.ContentEncoding != "" {
		t.Errorf("Schema.ContentEncoding = %q, want пусто (рецидив binary/RawBody-моста)", s.ContentEncoding)
	}
	if s.Format == "binary" {
		t.Errorf("Schema.Format = %q, want не-binary (Optional[string] — чистый string)", s.Format)
	}
}

// TestOptional_Schema_Int_Nullable — non-string скаляр: Optional[int] даёт схему
// integer с Nullable:true (3.1 `type: [integer, null]`), без octet-stream/binary.
func TestOptional_Schema_Int_Nullable(t *testing.T) {
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	s := Optional[int]{}.Schema(reg)

	if s == nil {
		t.Fatal("Schema(r) вернул nil")
	}
	if s.Type != huma.TypeInteger {
		t.Errorf("Schema.Type = %q, want %q", s.Type, huma.TypeInteger)
	}
	if !s.Nullable {
		t.Errorf("Schema.Nullable = %v, want true", s.Nullable)
	}
}

// optStructField — мелкий non-scalar тип для guard-теста ниже. huma на нём вернул бы
// `$ref` при allowRef=true, поэтому он — именно тот путь, что ронял регистрацию.
type optStructField struct {
	A string `json:"a"`
	B int    `json:"b"`
}

// TestOptional_Schema_StructT_NoRefNoPanic — BLOCKER-guard (ADR-054 §третий tier):
// Optional[<struct>] как поле request-body. До фикса Schema() отдавал huma `$ref` на
// компоненту (allowRef=true) и ставил Nullable=true поверх ref-узла → huma ПАНИКОВАЛ
// при РЕГИСТРАЦИИ операции («nullable is not supported for ... $ref»). Тест реально
// регистрирует операцию с таким полем (паника была на регистрации, не на Schema()) и
// проверяет, что схема — чистый inline nullable-object (`type: [object, null]` с
// properties), а не ref. allowRef=false закрыл blocker — регресс уронит этот тест.
func TestOptional_Schema_StructT_NoRefNoPanic(t *testing.T) {
	type structBody struct {
		Field Optional[optStructField] `json:"field" required:"false"`
	}
	type structInput struct {
		Body structBody
	}

	// Прямой ассерт на схему поля: inline-object, nullable, БЕЗ $ref.
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	fs := Optional[optStructField]{}.Schema(reg)
	if fs == nil {
		t.Fatal("Schema(r) вернул nil")
	}
	if fs.Ref != "" {
		t.Fatalf("Schema.Ref = %q, want пусто (struct-T должен инлайниться, не $ref — иначе nullable-on-ref паника)", fs.Ref)
	}
	if fs.Type != huma.TypeObject {
		t.Errorf("Schema.Type = %q, want %q (inline-object)", fs.Type, huma.TypeObject)
	}
	if !fs.Nullable {
		t.Errorf("Schema.Nullable = %v, want true (nullable на реальной object-схеме)", fs.Nullable)
	}
	if _, ok := fs.Properties["a"]; !ok {
		t.Errorf("Schema.Properties не несёт inline-поля struct-T: %v", fs.Properties)
	}

	// Главный guard: РЕГИСТРАЦИЯ huma-операции с этим полем не паникует. Именно здесь
	// huma раскрывает схему body и (до фикса) падал на nullable-ref.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("huma.Register с полем Optional[struct] запаниковал: %v (регресс allowRef=false → ref+nullable)", rec)
		}
	}()
	r := chi.NewRouter()
	api := newHumaCadenceAPI(r)
	huma.Register(api, huma.Operation{
		OperationID: "optStructRegister",
		Method:      http.MethodPost,
		Path:        "/opt-struct",
	}, func(_ context.Context, _ *structInput) (*struct{ Status int }, error) {
		return &struct{ Status int }{Status: http.StatusNoContent}, nil
	})
}

// TestOptional_Schema_NotInRequired — поле Optional[T] с `required:"false"` НЕ
// попадает в required схемы тела (presence несёт сам тип, не обязательность поля).
func TestOptional_Schema_NotInRequired(t *testing.T) {
	type body struct {
		Must string           `json:"must" required:"true"`
		Opt  Optional[string] `json:"opt" required:"false"`
	}
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	s := huma.SchemaFromType(reg, reflect.TypeFor[body]())

	for _, r := range s.Required {
		if r == "opt" {
			t.Errorf("Optional-поле `opt` попало в required %v — должно быть опциональным", s.Required)
		}
	}
	var hasMust bool
	for _, r := range s.Required {
		if r == "must" {
			hasMust = true
		}
	}
	if !hasMust {
		t.Errorf("required = %v, ожидалось наличие `must` (sanity)", s.Required)
	}
}

// TestOptional_RoundTrip_HumaInput — присутствие долетает из ТЕЛА HTTP-запроса в
// Optional[string] через реальную huma-input-привязку (chi+huma): omitted →
// Set=false; explicit null → Set=true,Null=true; value → Set=true,Value=x. Это
// доказывает, что huma-декодер зовёт UnmarshalJSON ровно когда ключ в теле.
func TestOptional_RoundTrip_HumaInput(t *testing.T) {
	type optBody struct {
		Field Optional[string] `json:"field" required:"false"`
	}
	type optInput struct {
		Body optBody
	}

	cases := []struct {
		name      string
		body      string
		wantSet   bool
		wantNull  bool
		wantValue string
	}{
		{"omitted", `{}`, false, false, ""},
		{"null", `{"field":null}`, true, true, ""},
		{"value", `{"field":"coven=prod"}`, true, false, "coven=prod"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured Optional[string]
			r := chi.NewRouter()
			api := newHumaCadenceAPI(r)
			huma.Register(api, huma.Operation{
				OperationID: "optRoundTrip",
				Method:      http.MethodPost,
				Path:        "/opt",
			}, func(_ context.Context, in *optInput) (*struct{ Status int }, error) {
				captured = in.Body.Field
				return &struct{ Status int }{Status: http.StatusNoContent}, nil
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/opt", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
			}
			if captured.Set != tc.wantSet || captured.Null != tc.wantNull || captured.Value != tc.wantValue {
				t.Errorf("Optional после huma-input = {Set:%v Null:%v Value:%q}, want {Set:%v Null:%v Value:%q}",
					captured.Set, captured.Null, captured.Value, tc.wantSet, tc.wantNull, tc.wantValue)
			}
		})
	}
}
