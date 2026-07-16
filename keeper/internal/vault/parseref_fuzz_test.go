package vault

import (
	"strings"
	"testing"
)

// FuzzNormalizeLogical fuzzes the parser of an untrusted logical path (the
// vault:-ref body after the prefix/leading slash is stripped).
// normalizeLogical is a security boundary: it collapses `//`, rejects
// `.`/`..`, and closes a deny-list bypass (`secret//keeper/x` → canonical
// `secret/keeper/x`). This checks property invariants, not specific outputs.
//
// Invariants:
//   - the function NEVER panics on any input;
//   - on success, the result does NOT contain a `.`/`..` segment and doesn't
//     traverse upward (no deny-list bypass);
//   - idempotence: normalize(normalize(x)) == normalize(x) if the first
//     call succeeded.
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
			// Invalid input — rejected normally, no panic. The output isn't
			// used (contract: on error, result is empty).
			return
		}

		assertNoTraversal(t, body, out)

		// Idempotence: re-normalizing the canonical form must yield the same
		// canonical form and not fail with an error.
		out2, err2 := normalizeLogical(out)
		if err2 != nil {
			t.Fatalf("normalizeLogical неидемпотентна: на канон %q (из %q) вернула ошибку %v", out, body, err2)
		}
		if out2 != out {
			t.Fatalf("normalizeLogical неидемпотентна: normalize(%q)=%q, повторно=%q (исходный вход %q)", out, out2, out, body)
		}
	})
}

// FuzzParseRef fuzzes the full vault:-ref parser starting from the external
// string (including the prefix/leading slash/mount-path separator). The
// same security boundary, but through the public ParseRef API — checks that
// the wrapper around normalizeLogical doesn't introduce its own bypass holes.
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

		// The ParseRef result is already a canonical logical path;
		// re-normalizing its body must be idempotent (no residual `//` or
		// dot segments slipping through).
		out2, err2 := normalizeLogical(out)
		if err2 != nil {
			t.Fatalf("ParseRef(%q)=%q, но normalizeLogical на результате упала: %v", ref, out, err2)
		}
		if out2 != out {
			t.Fatalf("ParseRef(%q)=%q не канонична: повторная нормализация дала %q", ref, out, out2)
		}
	})
}

// assertNoTraversal checks the security invariant of a successful result:
// no dot segments, no empty segments (`//`), no leading slash — i.e. no
// scope/deny-list bypass. src is the original input (for diagnostics).
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
