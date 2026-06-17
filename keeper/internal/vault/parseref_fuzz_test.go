package vault

import (
	"strings"
	"testing"
)

// FuzzNormalizeLogical фаззит парсер недоверенного logical-path (тело
// vault:-ref после среза префикса/leading slash). normalizeLogical — security-
// граница: она схлопывает `//`, отвергает `.`/`..` и закрывает deny-list
// bypass (`secret//keeper/x` → канон `secret/keeper/x`). Здесь проверяются
// property-инварианты, не конкретные выходы.
//
// Инварианты:
//   - функция НИКОГДА не паникует на любом входе;
//   - при успехе результат НЕ содержит сегмент `.`/`..` и не делает обход
//     вверх (deny-list bypass не проходит);
//   - идемпотентность: normalize(normalize(x)) == normalize(x), если первый
//     вызов успешен.
func FuzzNormalizeLogical(f *testing.F) {
	seeds := []string{
		"secret/keeper/x",
		"secret//keeper/x",
		"./secret",
		"../etc",
		"",
		strings.Repeat("a/", 100000) + "b",
		"secret/./keeper/x",
		"secret/keeper/../keeper/x",
		"secret/",
		"/secret/keeper",
		"secret/keeper/",
		"a/b",
		"...//..//...",
		"secret/\x00/keeper",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body string) {
		out, err := normalizeLogical(body)
		if err != nil {
			// Невалидный ввод — штатно отвергнут, без паники. Выход не
			// используется (контракт: при ошибке result пустой).
			return
		}

		assertNoTraversal(t, body, out)

		// Идемпотентность: повторная нормализация канона обязана дать тот же
		// канон и не упасть в ошибку.
		out2, err2 := normalizeLogical(out)
		if err2 != nil {
			t.Fatalf("normalizeLogical неидемпотентна: на канон %q (из %q) вернула ошибку %v", out, body, err2)
		}
		if out2 != out {
			t.Fatalf("normalizeLogical неидемпотентна: normalize(%q)=%q, повторно=%q (исходный вход %q)", out, out2, out, body)
		}
	})
}

// FuzzParseRef фаззит полный парсер vault:-ref от внешней строки (включая
// префикс/leading slash/разделитель mount/path). Та же security-граница, но
// через публичный API ParseRef — проверяет, что обёртка над normalizeLogical
// не вносит собственных дыр обхода.
func FuzzParseRef(f *testing.F) {
	seeds := []string{
		"vault:secret/keeper/postgres",
		"vault:secret//keeper/x",
		"vault:/secret/keeper/k",
		"vault:secret/./keeper/x",
		"vault:secret/keeper/../keeper/x",
		"vault:",
		"vault:/",
		"vault:secret/",
		"vault:/secret",
		"",
		"secret/keeper/x",
		"vault:" + strings.Repeat("a/", 100000) + "b",
		"vault:../etc/passwd",
		"vault:secret/keeper/..",
		"VAULT:secret/keeper/x",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, ref string) {
		out, err := ParseRef(ref)
		if err != nil {
			return
		}
		assertNoTraversal(t, ref, out)

		// Результат ParseRef — уже канонический logical-path; повторная
		// нормализация его тела обязана быть тождественной (нет остаточных
		// `//` или dot-сегментов, протёкших мимо).
		out2, err2 := normalizeLogical(out)
		if err2 != nil {
			t.Fatalf("ParseRef(%q)=%q, но normalizeLogical на результате упала: %v", ref, out, err2)
		}
		if out2 != out {
			t.Fatalf("ParseRef(%q)=%q не канонична: повторная нормализация дала %q", ref, out, out2)
		}
	})
}

// assertNoTraversal проверяет security-инвариант успешного результата:
// нет dot-сегментов, нет пустых сегментов (`//`), нет leading slash —
// то есть нет обхода scope/deny-list. src — исходный вход (для диагностики).
func assertNoTraversal(t *testing.T, src, out string) {
	t.Helper()
	if out == "" {
		t.Fatalf("успешный результат пуст при входе %q (контракт: успех ⇒ непустой канон)", src)
	}
	if strings.HasPrefix(out, "/") {
		t.Fatalf("результат %q начинается с '/' (обход вверх) при входе %q", out, src)
	}
	for _, seg := range strings.Split(out, "/") {
		switch seg {
		case "":
			t.Fatalf("результат %q содержит пустой сегмент (несхлопнутый '//') при входе %q", out, src)
		case ".", "..":
			t.Fatalf("результат %q содержит dot-сегмент %q (обход scope) при входе %q", out, seg, src)
		}
	}
}
