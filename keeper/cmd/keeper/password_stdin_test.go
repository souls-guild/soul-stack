package main

import (
	"strings"
	"testing"
)

func TestReadPasswordStdin(t *testing.T) {
	for in, want := range map[string]string{
		"correct horse battery":     "correct horse battery",
		"correct horse battery\n":   "correct horse battery",
		"correct horse battery\r\n": "correct horse battery",
		// Only one line ending goes: the rest is the password.
		" spaced \n\n": " spaced \n",
	} {
		got, err := readPasswordStdin(strings.NewReader(in))
		if err != nil || got != want {
			t.Errorf("readPasswordStdin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "\n", strings.Repeat("x", maxPasswordStdin+1)} {
		if _, err := readPasswordStdin(strings.NewReader(in)); err == nil {
			t.Errorf("readPasswordStdin(%d bytes) accepted", len(in))
		}
	}
}
