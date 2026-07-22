package cel

import (
	"testing"
)

// FuzzFlowControlCompile fuzzes compilation of untrusted CEL predicates in the
// Soul-side flow-control sandbox ([NewFlowControl], [ADR-012(d)]). This is the
// parse boundary for Destiny expressions: the author writes when:/changed_when:/
// failed_when:, and the string reaches the cel-go parser/compiler.
//
// EvalPredicate with empty Vars runs the whole compile path (guard → rewrite →
// env.Compile → env.Program) and then eval. We care about robustness: any string
// must yield either a bool result or an error (ErrCompile/ErrUnsupported/ErrEval)
// — but NEVER a panic and NEVER a hang.
//
// A hang is caught by the fuzzer itself: on hypothetical non-termination of eval
// for a single input, Go-fuzz records the timeout as a crash and saves the input
// to testdata. There is no separate timeout in the API (CEL is a sandbox by
// design, without sleep/I/O), so we do not emulate one here.
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
		"vault('secret/x')",              // blocked by the guard → ErrUnsupported
		"soulprint.hosts.where(h, h.up)", // flow-control isolation → error
		"__internal",                     // reserved prefix → guard
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
		// Engine is thread-safe and caches compilation; a shared e across all
		// inputs is the intended usage. We only care about the absence of a
		// panic/hang. We don't check the result or error class: for an arbitrary
		// string any of the outcomes (bool / ErrCompile / ErrUnsupported /
		// ErrEval) is correct.
		_, _ = e.EvalPredicate(expr, Vars{})
	})
}
