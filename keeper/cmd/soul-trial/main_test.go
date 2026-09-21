package main

// The `run` argument line (NIM-790).
//
// `soul-trial` grew `--modules` so that an L0 case is not green over a plugin step
// nobody checked. stdlib `flag` stops parsing at the first non-flag word, and
// soul-lint's usage line puts the flag AFTER the path, so without [hoistFlags] the
// documented form would leave the binding unparsed — the binary would run, print
// PASS, and have checked nothing. That failure is silent by construction, which is
// why the word order is pinned here rather than left to the usage text.

import (
	"reflect"
	"strings"
	"testing"
)

func TestHoistFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{
			// THE case: the form the usage line and the Makefile both write.
			"flag after the path, joined form",
			[]string{"./svc", "--modules=redis=/m/schema.json"},
			[]string{"--modules=redis=/m/schema.json", "./svc"},
		},
		{
			"flag after the path, two-word form",
			[]string{"./svc", "--modules", "redis=/m/schema.json"},
			[]string{"--modules", "redis=/m/schema.json", "./svc"},
		},
		{
			"flag before the path is left where it is",
			[]string{"--modules", "redis=/m", "./svc"},
			[]string{"--modules", "redis=/m", "./svc"},
		},
		{
			"repeatable",
			[]string{"./svc", "--modules", "a=/1", "--modules=b=/2"},
			[]string{"--modules", "a=/1", "--modules=b=/2", "./svc"},
		},
		{
			// A trailing `--modules` has no value to take. Taking the path instead
			// would report "no path" for a line whose defect is a missing binding.
			"trailing valueless flag does not eat the path",
			[]string{"./svc", "--modules"},
			[]string{"--modules", "./svc"},
		},
		{
			// `--` ends flag parsing: the separator is dropped and what follows is
			// positional. Hoisting it with the flags would hide the path.
			"double dash ends flag parsing",
			[]string{"--modules=a=/1", "--", "./-weird-path"},
			[]string{"--modules=a=/1", "./-weird-path"},
		},
		{
			"nothing to reorder",
			[]string{"./svc"},
			[]string{"./svc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hoistFlags(tc.args); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hoistFlags(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// Fail closed. A binding the operator NAMED and that cannot be read must stop the
// run, never degrade into `plugin_params_unchecked` and a green table: that is
// "checked and clean" printed over "could not look", the failure the flag exists to
// remove. Same rule, same exit class as soul-lint's.
func TestRunTrial_UnreadableBindingIsFatal(t *testing.T) {
	code := runTrial([]string{t.TempDir(), "--modules=redis=" + t.TempDir() + "/gone/schema.json"})
	if code != exitUsage {
		t.Errorf("exit %d, want %d — an unreadable binding must not let the run start", code, exitUsage)
	}
}

// A reserved alias cannot be registered in any cluster, so binding one would let a
// document of the operator's choosing define what `core.*` accepts.
func TestRunTrial_ReservedAliasIsRefused(t *testing.T) {
	if code := runTrial([]string{t.TempDir(), "--modules=core=/whatever"}); code != exitUsage {
		t.Errorf("exit %d, want %d for a reserved alias", code, exitUsage)
	}
}

// A trailing `--modules` reports the missing BINDING, not a missing path.
func TestRunTrial_TrailingModulesReportsTheMissingBinding(t *testing.T) {
	if code := runTrial([]string{t.TempDir(), "--modules"}); code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
}

func TestPrintUsage_DocumentsTheFlag(t *testing.T) {
	// The usage text is the only place an operator learns the flag exists, and a
	// flag nobody knows about is a check nobody runs.
	var b strings.Builder
	printUsage(&b)
	for _, want := range []string{"--modules", "plugin_params_unchecked"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("usage does not mention %q\n%s", want, b.String())
		}
	}
}
