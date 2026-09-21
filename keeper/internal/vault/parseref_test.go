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
		// normalization: repeated slashes collapse into one.
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
		"secret/keeper/x",                 // no vault: prefix
		"vault:",                          // empty body
		"vault:keeper-only",               // no mount separator
		"vault:/",                         // only the mount separator
		"vault:secret/",                   // trailing slash with no rel
		"file:/etc/keeper/key.bin",        // different scheme
		"vault:secret/./keeper/x",         // `.` segment is rejected
		"vault:secret/keeper/../keeper/x", // `..` segment is rejected (climb)
		"vault:secret/../keeper/x",        // `..` right after the mount
		"vault:secret/keeper/..",          // trailing `..`
		"vault:..",                        // body is just `..`
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
