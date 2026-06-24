package cel

import (
	"strconv"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// CEL-функция merge() ([templating.md §2.3], [ADR-010 Amendment 2026-06-22]).
// Слияние map-ов слева направо по ключу ВЕРХНЕГО уровня. Две формы:
//
//	${ merge(essence.redis.defaults, input.redis_settings) }   → map  (varargs)
//	${ merge(a, b, c) }                                         → map  (varargs)
//	${ merge(input.users.map(name, {...})) }                   → map  (один list(map))
//
// Вторая форма принимает ОДИН аргумент-список map-ов и flatten-ит его слева
// направо: то же слияние, но над элементами списка, а не над позиционными
// аргументами. Закрывает случай, когда коллекция приходит из CEL-comprehension
// `.map(...)` (даёт список), а в шаблон её нужно передать map-ом «имя→объект»
// ради ДЕТЕРМИНИЗМА порядка строк (text/template range по map сортирует ключи,
// по list — сохраняет, возможно недетерминированный, порядок итерации Go-map).
//
// SHALLOW (вложенный map НЕ сливается глубоко — правый аргумент целиком
// замещает значение совпавшего верхнего ключа), last-wins (правый перекрывает
// левый). Pure: без I/O, сети, секретов, крипты, eval-time состояния —
// симметрично stdlib-функциям size()/glob(). Slot трансляции «простой
// типизированный input оператора → детальный конфиг»: база CEL не имеет
// оператора map+map, а граница движков ([ADR-010]) не пускает sprig-merge в
// `.yml` — merge() закрывает слияние авторского пресета с passthrough-input-map
// в render-фазе.
//
// Тип результата — map(dyn,dyn) (как остальные узлы до закрытия type-model
// [templating.md §2.4]). Не-map аргумент → eval-ошибка (types.NewErr, не паника):
// CEL даёт overload только для map-аргументов, но при dyn-склейке невычислимого
// типа binding получает не-mapper ref.Val — возвращаем внятную ошибку.
//
// Безопасность ([ADR-010 Amendment 2026-06-22], security-blocker): merge()
// сохраняет ключи верхнего уровня без переименования, поэтому merged-map с
// vault-значением (${ vault(...) } под SENSITIVE-ключом назначения — password/
// secret/token/…) маскируется выходным слоем (shared/audit.MaskSecrets по
// sensitive-имени ключа) идентично прямому ${ vault(...) } под тем же ключом.
//
// vault-ref-маркерная ветвь MaskSecrets ([vaultRefRe]) здесь НЕ срабатывает:
// vault() резолвится в plaintext keeper-side, в merged-map попадает уже значение
// секрета, а не строка-ссылка `vault:<mount>/…`. Поэтому секрет под НЕ-sensitive
// ключом НЕ маскируется — это инвариант авторов сценариев (класть секрет под
// secret-именованный ключ), а НЕ ответственность merge(). Доказано
// [TestMerge_SecretMaskedSameAsDirectVault] (sensitive-ключ маскируется) +
// [TestMerge_SecretUnderNonSensitiveKeyNotMasked] (несекретный ключ — нет).
//
// Регистрируется в основном scenario/destiny-режиме (Keeper full env) и в
// flow-control-режиме ([NewFlowControl], [ADR-012(d)]): pure-функция без внешнего
// контекста, симметрия с scenario-выражениями. В migration-CEL ([ADR-019]) НЕ
// регистрируется (см. [buildEngine]): hermetic-песочница с минимумом surface area
// (только `state` + stdlib), расширение требует отдельного ADR (как glob()).

// mergeFuncName — имя функции в CEL-env. Пользователь пишет `merge(a, b, ...)`.
const mergeFuncName = "merge"

// mergeEnvOptions возвращает EnvOption-ы регистрации merge(): глобальная
// вариадик-функция через overload-ы для 1..N map-аргументов. cel-go не имеет
// нативного variadic-overload-а, поэтому объявляем фиксированные арности до
// mergeMaxArity (покрывает реальные сценарии: слияние пресет-слоёв; больше —
// внятная compile-ошибка no-such-overload, расширяемо без breaking change).
func mergeEnvOptions() []cel.EnvOption {
	mapType := cel.MapType(cel.DynType, cel.DynType)
	listOfMapType := cel.ListType(mapType)
	overloads := make([]cel.FunctionOpt, 0, mergeMaxArity+1)
	for arity := 1; arity <= mergeMaxArity; arity++ {
		args := make([]*cel.Type, arity)
		for i := range args {
			args[i] = mapType
		}
		overloads = append(overloads, cel.Overload(
			overloadName(arity), args, mapType,
			cel.FunctionBinding(callMerge),
		))
	}
	// merge(list(map)) -> map: ОДИН аргумент-список map-ов, flatten слева
	// направо. Отдельный overload по типу аргумента (list(map) ≠ map), поэтому
	// не конфликтует с arity-1 varargs-формой merge(map): cel-go выбирает по типу.
	overloads = append(overloads, cel.Overload(
		mergeFuncName+"_listmap_map", []*cel.Type{listOfMapType}, mapType,
		cel.UnaryBinding(callMergeList),
	))
	return []cel.EnvOption{cel.Function(mergeFuncName, overloads...)}
}

// mergeMaxArity — максимальное число map-аргументов merge(), для которого
// объявлен overload. Слияние пресет-слоёв на практике укладывается в единицы
// аргументов; больше — расширяемо без breaking change (добавить overload-ы).
const mergeMaxArity = 8

// overloadName — детерминированное имя overload-а для arity аргументов
// (cel-go требует уникальное имя на overload).
func overloadName(arity int) string {
	return mergeFuncName + "_" + strconv.Itoa(arity) + "map_map"
}

// callMerge — binding функции merge(m, m, ...). Слияние SHALLOW last-wins:
// последовательный обход аргументов слева направо, ключи верхнего уровня
// перезаписываются (правый бьёт левый). Не-map аргумент (dyn-склейка дала
// невычислимый тип) → types.NewErr (штатная eval-ошибка). Результат собирается
// в map[ref.Val]ref.Val и адаптируется к CEL-map через types.NewRefValMap.
func callMerge(args ...ref.Val) ref.Val {
	out := make(map[ref.Val]ref.Val)
	for _, a := range args {
		if err := mergeInto(out, a); err != nil {
			return err
		}
	}
	return types.NewRefValMap(types.DefaultTypeAdapter, out)
}

// callMergeList — binding формы merge(list(map)). Один аргумент-список map-ов
// flatten-ится слева направо тем же SHALLOW last-wins-правилом. Пустой список →
// пустой map. Элемент списка не-map (dyn-список) → внятная eval-ошибка.
func callMergeList(arg ref.Val) ref.Val {
	l, ok := arg.(traits.Lister)
	if !ok {
		return types.NewErr("merge(): аргумент должен быть list(map), получено %s", arg.Type().TypeName())
	}
	out := make(map[ref.Val]ref.Val)
	it := l.Iterator()
	for it.HasNext() == types.True {
		if err := mergeInto(out, it.Next()); err != nil {
			return err
		}
	}
	return types.NewRefValMap(types.DefaultTypeAdapter, out)
}

// mergeInto вливает один map-аргумент в накопитель out (ключи верхнего уровня,
// last-wins). Не-map аргумент → types.NewErr (возвращается вызывающим как есть).
func mergeInto(out map[ref.Val]ref.Val, a ref.Val) ref.Val {
	m, ok := a.(traits.Mapper)
	if !ok {
		return types.NewErr("merge(): элемент должен быть map, получено %s", a.Type().TypeName())
	}
	it := m.Iterator()
	for it.HasNext() == types.True {
		k := it.Next()
		out[k] = m.Get(k)
	}
	return nil
}
