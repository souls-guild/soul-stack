// Инвариант P5 — согласованность встроенного manifest-а core-модуля и того, что
// реально проверяет валидатор `params:` задачи (shared/config).
//
// Зачем отдельный пакет `coremanifest_test`, а не дополнение к internal-тесту:
// production-код `shared/config` импортирует `shared/coremanifest`
// (config/module_params.go берёт схему через coremanifest.Default()). Internal
// тест `package coremanifest` не может импортировать config — это циклический
// импорт. Внешний тест-пакет в графе импортов production-кода не участвует,
// поэтому здесь допустимо тянуть и coremanifest, и config одновременно. Файл
// физически в shared/coremanifest/, production-код не меняется (зона ТЗ).
//
// АРХИТЕКТУРНАЯ ЗАМЕТКА (важна для понимания, что именно стережёт тест).
// У валидатора `params:` НЕТ собственного источника правды о наборе/required/типах
// параметров: validateModuleParams читает def.Input прямо из coremanifest.State.
// То есть «множество имён params в манифесте == множество имён, известных
// валидатору» сейчас верно ПО ПОСТРОЕНИЮ (один источник). TestP5_* фиксируют это
// как regression-guard: если кто-то заведёт валидатору второй источник params
// (хардкод-список, отдельная таблица допустимых ключей, кодген) — поведенческие
// проверки ниже начнут расходиться с манифестом и тест упадёт.
//
// Содержательная (не тавтологичная) часть инварианта — ТИПЫ: manifest-парсер
// (shared/plugin.validInputTypes) допускает более широкий набор type, чем валидатор
// params умеет интерпретировать на уровне литералов. Если в манифесте появится
// type, который валидатор молча проглатывает (ветка default в astMatchesType —
// «неизвестный тип, type-check пропускаем»), это тихая дыра: param_type_mismatch
// для такого параметра никогда не сработает. TestP5_DeclaredTypesAreEnforceable
// ловит это поведенчески, через публичный путь config.LoadDestinyTasksFromBytes.
package coremanifest_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// keeperSideModules — core-модули с диспетчером `on: keeper` (ADR-017). У них нет
// Soul-side destiny-формы «задача в tasks/main.yml», но `params:`-валидатор всё
// равно резолвит их через тот же coremanifest.State (config.module_params), так
// что синтетическая задача-проба ниже корректна и для них.
//
// Перечисление здесь — только чтобы тест-сообщения помечали такие модули, а не
// чтобы их пропускать: проверка одинакова для обеих сторон.
var keeperSideModules = map[string]bool{
	"core.soul":  true,
	"core.cloud": true,
	"core.vault": true,
	"core.choir": true,
}

// allRegisteredModules — детерминированный список всех зарегистрированных
// core-модулей. Берётся из самого реестра (Names() отсортирован), а не из
// хардкод-таблицы, чтобы «забыли новый модуль» не обходило P5.
func allRegisteredModules() []string {
	return coremanifest.Default().Names()
}

// dummyForType подбирает YAML-литерал, который для КАЖДОГО типа из манифеста
// заведомо НЕ совпадает по структуре с этим типом, — чтобы спровоцировать
// param_type_mismatch у валидатора, если он этот тип интерпретирует. Для string
// валидатор принимает почти всё, поэтому строке противопоставляем list-литерал, и
// наоборот. Возврат "" означает «для этого типа осмысленную проверку построить
// нельзя» (тогда type-проверку пропускаем, но факт залогируем).
func mismatchLiteralFor(declaredType string) (literal string, ok bool) {
	switch declaredType {
	case "string":
		return "[a, b]", true // list вместо string
	case "int", "integer", "number":
		return "[a, b]", true // list вместо числа
	case "bool", "boolean":
		return "[a, b]", true // list вместо bool
	case "list", "array":
		return "\"scalar\"", true // строка вместо list
	case "map", "object":
		return "\"scalar\"", true // строка вместо map
	default:
		// Тип, не известный построителю мисматч-литералов. Это и есть кандидат на
		// тихую дыру — сигналим вызывающему через ok=false.
		return "", false
	}
}

// firstState возвращает имя любого state модуля в детерминированном порядке.
// Нужно для построения адреса core.<mod>.<state> в синтетической задаче.
func firstStateWithRequiredOrAny(m *plugin.Manifest) (state string, def plugin.StateDef, ok bool) {
	names := make([]string, 0, len(m.Spec.States))
	for s := range m.Spec.States {
		names = append(names, s)
	}
	// Лексикографический порядок — воспроизводимость.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	if len(names) == 0 {
		return "", plugin.StateDef{}, false
	}
	return names[0], m.Spec.States[names[0]], true
}

// TestP5_AllModulesResolvableByValidator — каждый зарегистрированный core-модуль
// и каждый его state резолвится валидатором params через тот же реестр, что отдаёт
// manifest. Это base-инвариант: валидатор и manifest «видят» один и тот же набор
// модулей/states. Падение здесь = валидатор и manifest разошлись по источнику.
func TestP5_AllModulesResolvableByValidator(t *testing.T) {
	reg := coremanifest.Default()
	for _, mod := range allRegisteredModules() {
		m, ok := reg.Lookup(mod)
		if !ok {
			t.Errorf("%s: есть в Names(), но Lookup вернул ok=false", mod)
			continue
		}
		if len(m.Spec.States) == 0 {
			t.Errorf("%s: манифест без states", mod)
			continue
		}
		for state := range m.Spec.States {
			def, ok := reg.State(mod, state)
			if !ok {
				t.Errorf("%s.%s: State() не нашёл задекларированный state", mod, state)
				continue
			}
			// def, который читает валидатор (config.module_params), — это ровно тот
			// же объект из Spec.States; проверяем непустоту контракта.
			if def.Input == nil && len(m.Spec.States[state].Input) != 0 {
				t.Errorf("%s.%s: State().Input расходится с Spec.States", mod, state)
			}
		}
	}
}

// TestP5_DeclaredTypesAreEnforceable — для каждого param КАЖДОГО state КАЖДОГО
// модуля: если manifest объявил `type`, валидатор params обязан этот тип реально
// интерпретировать. Проверка поведенческая — через публичный config-путь
// (LoadDestinyTasksFromBytes), без обращения к приватным хелперам config.
//
// Метод: подаём задаче литерал заведомо ДРУГОГО типа в этот param и заполняем все
// прочие required-поля валидными значениями (чтобы missing_required_param не
// зашумлял картину). Ожидаем param_type_mismatch ровно по проверяемому param.
// Если его нет — тип в манифесте необязателен (валидатор его игнорирует): это
// либо тихая дыра, либо CEL-обёртка/особый тип. Все такие случаи table-driven
// собираются в один отчёт.
func TestP5_DeclaredTypesAreEnforceable(t *testing.T) {
	reg := coremanifest.Default()

	type miss struct {
		addr string // core.<mod>.<state>.<param>
		typ  string
		why  string
	}
	var unenforced []miss

	for _, mod := range allRegisteredModules() {
		m, _ := reg.Lookup(mod)
		for state, def := range m.Spec.States {
			for pname, p := range def.Input {
				if p.Type == "" {
					continue // тип не объявлен — нечего сверять.
				}
				mismatch, buildable := mismatchLiteralFor(p.Type)
				if !buildable {
					// Тип, под который мы не умеем строить мисматч-литерал. Это
					// сигнал: либо новый тип в манифесте, не покрытый ни тестом, ни
					// (возможно) валидатором. Фиксируем как находку, не как pass.
					unenforced = append(unenforced, miss{
						addr: fmt.Sprintf("core.%s.%s.%s", strings.TrimPrefix(mod, "core."), state, pname),
						typ:  p.Type,
						why:  "тест не умеет строить мисматч-литерал для этого типа (новый тип?)",
					})
					continue
				}

				src := buildTaskProbing(mod, state, def, pname, mismatch)
				_, diags, _ := config.LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), config.ValidateOptions{})

				if !hasTypeMismatchFor(diags, pname) {
					unenforced = append(unenforced, miss{
						addr: fmt.Sprintf("core.%s.%s.%s", strings.TrimPrefix(mod, "core."), state, pname),
						typ:  p.Type,
						why:  fmt.Sprintf("валидатор НЕ дал param_type_mismatch на литерал %q; diags=%v", mismatch, codes(diags)),
					})
				}
			}
		}
	}

	if len(unenforced) > 0 {
		t.Errorf("найдено %d param-ов, чей manifest-type валидатор не проверяет (дрейф manifest↔валидатор):", len(unenforced))
		for _, u := range unenforced {
			side := "soul-side"
			modName := "core." + strings.SplitN(strings.TrimPrefix(u.addr, "core."), ".", 2)[0]
			if keeperSideModules[modName] {
				side = "keeper-side"
			}
			t.Errorf("  - %s (%s, type=%s): %s", u.addr, side, u.typ, u.why)
		}
	}
}

// TestP5_RequiredEnforcedByValidator — для каждого state с хотя бы одним required
// param: задача БЕЗ params: должна дать missing_required_param по каждому такому
// полю. Стережёт расхождение «manifest пометил required, валидатор не требует».
// Источник required — manifest; проверяем, что валидатор его уважает.
func TestP5_RequiredEnforcedByValidator(t *testing.T) {
	reg := coremanifest.Default()
	for _, mod := range allRegisteredModules() {
		m, _ := reg.Lookup(mod)
		for state, def := range m.Spec.States {
			req := requiredNames(def)
			if len(req) == 0 {
				continue
			}
			// Адрес без params: — все required отсутствуют по определению.
			src := fmt.Sprintf("- name: probe\n  module: %s.%s\n", mod, state)
			_, diags, _ := config.LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), config.ValidateOptions{})
			for _, name := range req {
				if !hasMissingRequiredFor(diags, name) {
					t.Errorf("%s.%s: manifest пометил %q required, но валидатор не дал missing_required_param; diags=%v",
						mod, state, name, codes(diags))
				}
			}
		}
	}
}

// buildTaskProbing собирает синтетическую задачу destiny: проверяемому param
// подсовывает мисматч-литерал, остальным required-полям — валидные заглушки по их
// типу, чтобы missing_required_param не перекрывал ожидаемый param_type_mismatch.
func buildTaskProbing(mod, state string, def plugin.StateDef, probeParam, probeLiteral string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- name: probe\n  module: %s.%s\n  params:\n", mod, state)
	fmt.Fprintf(&b, "    %s: %s\n", probeParam, probeLiteral)
	// Прочие required заполняем валидными значениями их типа.
	for _, name := range requiredNames(def) {
		if name == probeParam {
			continue
		}
		fmt.Fprintf(&b, "    %s: %s\n", name, validLiteralFor(def.Input[name].Type))
	}
	return b.String()
}

// validLiteralFor — валидный YAML-литерал для типа (для заполнения соседних
// required-полей в probing-задаче).
func validLiteralFor(t string) string {
	switch t {
	case "int", "integer", "number":
		return "1"
	case "bool", "boolean":
		return "true"
	case "list", "array":
		return "[x]"
	case "map", "object":
		return "{k: v}"
	default: // string и неизвестные — строка безопасна.
		return "\"x\""
	}
}

func requiredNames(def plugin.StateDef) []string {
	var out []string
	for name, p := range def.Input {
		if p.Required {
			out = append(out, name)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func hasTypeMismatchFor(diags []diag.Diagnostic, param string) bool {
	suffix := ".params." + param
	for _, d := range diags {
		if d.Code == "param_type_mismatch" && strings.HasSuffix(d.YAMLPath, suffix) {
			return true
		}
	}
	return false
}

func hasMissingRequiredFor(diags []diag.Diagnostic, param string) bool {
	suffix := ".params." + param
	for _, d := range diags {
		if d.Code == "missing_required_param" && strings.HasSuffix(d.YAMLPath, suffix) {
			return true
		}
	}
	return false
}

func codes(diags []diag.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}
