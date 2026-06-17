package cel

import (
	"testing"
)

// FuzzFlowControlCompile фаззит компиляцию недоверенных CEL-предикатов в
// Soul-side flow-control-песочнице ([NewFlowControl], [ADR-012(d)]). Это
// граница разбора выражений из Destiny: автор пишет when:/changed_when:/
// failed_when:, строка доходит до cel-go-парсера/компилятора.
//
// EvalPredicate с пустыми Vars прогоняет весь путь compile (guard → rewrite →
// env.Compile → env.Program) и затем eval. Нас интересует устойчивость: любая
// строка обязана дать либо bool-результат, либо ошибку (ErrCompile/
// ErrUnsupported/ErrEval) — но НИКОГДА панику и НИКОГДА зависание.
//
// Зависание ловится самим фаззером: при гипотетическом не-завершении eval
// одного входа Go-fuzz зафиксирует таймаут как краш и сохранит вход в
// testdata. Отдельного таймаута в API нет (CEL — sandbox by design, без
// sleep/I/O), поэтому здесь его не эмулируем.
func FuzzFlowControlCompile(f *testing.F) {
	seeds := []string{
		"has(input.x)",
		"register.self.changed",
		"register.probe.exit_code == 0",
		"input.do_restart && register.self.changed",
		"soulprint.self.os.family == 'debian'",
		"",
		"1 + ",
		"((((",
		")(",
		"vault('secret/x')",              // запрещён guard-ом → ErrUnsupported
		"soulprint.hosts.where(h, h.up)", // изоляция flow-control → ошибка
		"__internal",                     // зарезервированный префикс → guard
		"\"" + "x" + "\"",
		"size([1,2,3]) > 0",
		"a" + "\x00" + "b",
		"input." + "x.y.z.w.v.u.t",
		"[1].map(x, x).filter(y, y > 0)",
		"'unterminated",
		"input[",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	e, err := NewFlowControl()
	if err != nil {
		f.Fatalf("NewFlowControl: %v", err)
	}

	f.Fuzz(func(t *testing.T, expr string) {
		// Engine потокобезопасен и кеширует compile; общий e на все входы —
		// штатный режим использования. Нам важно лишь отсутствие паники/
		// зависания. Результат и класс ошибки не проверяем: для произвольной
		// строки любой из исходов (bool / ErrCompile / ErrUnsupported /
		// ErrEval) корректен.
		_, _ = e.EvalPredicate(expr, Vars{})
	})
}
