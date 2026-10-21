package config

import (
	"errors"
	"strings"
	"testing"
)

// TestServiceVarsStack_InlineIsRefusedWithTheHint — point 14 of the ADR-0082
// amendment of 2026-10-15: `inline:` is removed. Whatever shape its value takes,
// and whether or not a `file:` sits beside it, the parse refuses it as
// stack_step_invalid and says where the values go instead — not a bare
// unknown-key error that leaves the author guessing.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the `s.Inline != nil`
// case from Validate. "beside file" then parses, and the others fall through to
// "needs file:" without the hint.
func TestServiceVarsStack_InlineIsRefusedWithTheHint(t *testing.T) {
	for name, stack := range map[string]string{
		"map":         "stack:\n  - inline: {a: 1}\n",
		"list":        "stack:\n  - inline: [1, 2]\n",
		"scalar":      "stack:\n  - inline: x\n",
		"beside file": "stack:\n  - file: 00-base.yaml\n    inline: {a: 1}\n",
		"with when":   "stack:\n  - inline: {a: 1}\n    when: \"true\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseServiceVarsStack([]byte(stack))
			if !errors.Is(err, ErrStackStepInvalid) {
				t.Fatalf("want ErrStackStepInvalid, got %v", err)
			}
			for _, want := range []string{"step 1", "inline: is removed", "put the values in a file under vars/ and name it with file:"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("missing %q in %v", want, err)
				}
			}
		})
	}
}

// TestServiceVarsStack_StepNeedsAFile — with `inline:` gone, a step without
// `file:` carries no layer at all.
func TestServiceVarsStack_StepNeedsAFile(t *testing.T) {
	_, err := ParseServiceVarsStack([]byte("stack:\n  - when: \"true\"\n"))
	if !errors.Is(err, ErrStackStepInvalid) || !strings.Contains(err.Error(), "needs file:") {
		t.Fatalf("want a needs-file refusal, got %v", err)
	}
}
