package vault

import (
	"errors"
	"testing"
)

func TestParseRef_HappyPath(t *testing.T) {
	cases := []struct {
		ref      string
		wantPath string
	}{
		{"vault:secret/keeper/postgres", "secret/keeper/postgres"},
		{"vault:secret/keeper/jwt-signing-key", "secret/keeper/jwt-signing-key"},
		{"vault:kv/foo/bar/baz", "kv/foo/bar/baz"},
		{"vault:/secret/keeper/k", "secret/keeper/k"},
		// нормализация: повторные слэши схлопываются в один.
		{"vault:secret//keeper/x", "secret/keeper/x"},
		{"vault:secret///keeper//x", "secret/keeper/x"},
		{"vault:secret/services/redis/prod", "secret/services/redis/prod"},
	}
	for _, c := range cases {
		t.Run(c.ref, func(t *testing.T) {
			p, err := ParseRef(c.ref)
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", c.ref, err)
			}
			if p != c.wantPath {
				t.Errorf("path = %q, want %q", p, c.wantPath)
			}
		})
	}
}

func TestParseRef_Errors(t *testing.T) {
	bad := []string{
		"",
		"secret/keeper/x",                 // нет префикса vault:
		"vault:",                          // пустое тело
		"vault:keeper-only",               // нет mount-разделителя
		"vault:/",                         // только mount-разделитель
		"vault:secret/",                   // trailing slash без rel
		"file:/etc/keeper/key.bin",        // другой scheme
		"vault:secret/./keeper/x",         // сегмент `.` отвергается
		"vault:secret/keeper/../keeper/x", // сегмент `..` отвергается (подъём)
		"vault:secret/../keeper/x",        // `..` сразу после mount
		"vault:secret/keeper/..",          // trailing `..`
		"vault:..",                        // тело только из `..`
	}
	for _, ref := range bad {
		t.Run(ref, func(t *testing.T) {
			_, err := ParseRef(ref)
			if err == nil {
				t.Fatalf("ParseRef(%q): expected error, got nil", ref)
			}
			if !errors.Is(err, ErrInvalidVaultRef) {
				t.Errorf("err = %v, want errors.Is ErrInvalidVaultRef", err)
			}
		})
	}
}
