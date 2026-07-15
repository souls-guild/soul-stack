package api

// Optional[T] — a generic presence wrapper for JSON fields with PATCH-presence semantics
// (ADR-054 §Pattern, third tier). Distinguishes the THREE states of an input field that
// `*T` collapses into two (a nil pointer does not tell "the key was absent" from "key = null"):
//
//   - omitted   — the key is physically absent from the body → UnmarshalJSON is NOT called → Set=false;
//   - explicit null — `"field": null` → Set=true, Null=true (Value — zero-value);
//   - value     — `"field": <v>` → Set=true, Null=false, Value=<v>.
//
// This is the REFERENCE tier for rare PATCH fields where an explicit reset (null) differs from
// "leave alone" (omitted). It replaces the REJECTED RawBody []byte bridge: that one dragged an
// `application/octet-stream` artifact into the OpenAPI fragment (huma on a RawBody []byte field
// generates an octet-stream requestBody), which broke web-codegen on merge. Optional[T]
// carries presence IN THE TYPE → the requestBody stays clean application/json.
//
// Other PATCH bodies (cadence/service/synod/incarnation-hosts) do NOT detect presence —
// read-modify-write `*T omitempty` is enough for them (nil = leave alone, no explicit reset).
// Do not proliferate Optional[T] where the field's semantics have no reset.

import (
	"encoding/json"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// Optional wraps a value of type T, carrying a presence flag (Set) and an explicit-null
// flag (Null) — for three-valued PATCH-presence semantics. A zero-value Optional[T]
// (Set=false) means "the field was absent from the body".
type Optional[T any] struct {
	// Set — the key was physically present in the JSON body (incl. a null value).
	Set bool
	// Null — the key was present with a null value (Set must be true).
	Null bool
	// Value — the decoded value when Set && !Null; otherwise the zero-value T.
	Value T
}

// UnmarshalJSON implements json.Unmarshaler. The standard decoder calls it ONLY
// when the key is physically present in the body (omitted → the method is not called → Set stays false,
// zero-value). `null` → Null=true, Value=zero. Otherwise — unmarshal into Value.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		o.Set = true
		o.Null = true
		var zero T
		o.Value = zero
		return nil
	}
	if err := json.Unmarshal(data, &o.Value); err != nil {
		// set the presence flags only in the success branches: on a parse error the
		// Optional stays unset (no dirty Set=true on failure).
		return err
	}
	o.Set = true
	o.Null = false
	return nil
}

// Schema реализует huma.SchemaProvider: отдаёт схему вложенного типа T, помеченную
// nullable (3.1-форма `type: [<t>, null]`). huma на поле с этим типом НЕ добавляет
// его в required автоматически (required управляется тегами поля: `required:"false"`
// держит поле опциональным).
//
// allowRef=false ОБЯЗАТЕЛЕН: схема T ВСЕГДА инлайнится (а не отдаётся как `$ref` на
// зарегистрированную компоненту), поэтому Nullable применяется к РЕАЛЬНОЙ inline-схеме
// для ЛЮБОГО T — скаляр даёт `type: [<t>, null]`, struct даёт
// `type: [object, null]` с inline-properties. С allowRef=true huma вернул бы для
// struct-T голый `{"$ref": …}`, а `Nullable=true` поверх ref-узла роняет регистрацию
// операции паникой «nullable is not supported for ref». Presence-tier валиден для
// любого T (скаляр и struct), не только скалярного.
//
// requestBody операции остаётся application/json (никакого octet-stream artifact) —
// ключевой инвариант tier-а против RawBody []byte-моста.
func (o Optional[T]) Schema(r huma.Registry) *huma.Schema {
	s := r.Schema(reflect.TypeFor[T](), false, "")
	s.Nullable = true
	return s
}

// Get возвращает значение и флаг «значение задано и не null» (Set && !Null).
func (o Optional[T]) Get() (T, bool) {
	return o.Value, o.Set && !o.Null
}

// IsNull — поле присутствовало со значением null (явный сброс).
func (o Optional[T]) IsNull() bool {
	return o.Set && o.Null
}

// optionalToPtr переводит Optional[T] в *T для доменного presence-конверта:
// задано не-null значение (Set && !Null) → указатель на копию Value; иначе
// (omitted или explicit null) → nil. Presence-факт несёт отдельно поле Set —
// доменный SetDefaultScope строится из него, а не из nil-ности указателя.
func optionalToPtr[T any](o Optional[T]) *T {
	if v, ok := o.Get(); ok {
		return &v
	}
	return nil
}
