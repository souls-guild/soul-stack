package api

// Optional[T] — generic presence-обёртка для JSON-полей с PATCH-presence-семантикой
// (ADR-054 §Pattern третий tier). Различает ТРИ состояния входного поля, которые
// `*T` схлопывает в два (nil-указатель не отличает «ключа не было» от «ключ = null»):
//
//   - omitted   — ключа физически нет в теле → UnmarshalJSON НЕ вызывается → Set=false;
//   - explicit null — `"field": null` → Set=true, Null=true (Value — zero-value);
//   - value     — `"field": <v>` → Set=true, Null=false, Value=<v>.
//
// Это ЭТАЛОН tier-а для редких PATCH-полей, где явный сброс (null) отличается от
// «не трогать» (omitted). Заменяет ОТВЕРГНУТЫЙ RawBody []byte-мост: тот тащил в
// OpenAPI-фрагмент artifact `application/octet-stream` (huma на поле RawBody []byte
// генерит octet-stream requestBody), ломавший web-codegen при мерже. Optional[T]
// несёт presence В ТИПЕ → requestBody остаётся чистым application/json.
//
// Прочие PATCH (cadence/service/synod/incarnation-hosts) presence НЕ детектят —
// им хватает read-modify-write `*T omitempty` (nil = не трогать, без явного сброса).
// Не плодить Optional[T] там, где сброса в семантике поля нет.

import (
	"encoding/json"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// Optional обёртывает значение типа T, неся флаг присутствия (Set) и флаг явного
// null (Null) — для трёхзначной PATCH-presence-семантики. Zero-value Optional[T]
// (Set=false) означает «поле в теле отсутствовало».
type Optional[T any] struct {
	// Set — ключ физически присутствовал в JSON-теле (вкл. значение null).
	Set bool
	// Null — ключ присутствовал со значением null (Set обязан быть true).
	Null bool
	// Value — декодированное значение при Set && !Null; иначе zero-value T.
	Value T
}

// UnmarshalJSON реализует json.Unmarshaler. Вызывается стандартным декодером ТОЛЬКО
// когда ключ физически есть в теле (omitted → метод не вызван → Set остаётся false,
// zero-value). `null` → Null=true, Value=zero. Иначе — unmarshal в Value.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		o.Set = true
		o.Null = true
		var zero T
		o.Value = zero
		return nil
	}
	if err := json.Unmarshal(data, &o.Value); err != nil {
		// presence-флаги ставим только в success-ветках: при ошибке парса
		// Optional остаётся неустановленным (нет грязного Set=true на провале).
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
