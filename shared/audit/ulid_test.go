package audit

import (
	"testing"
)

func TestNewULID_FormatAndUniqueness(t *testing.T) {
	const n = 200
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewULID()
		if !IsValidULID(id) {
			t.Fatalf("NewULID()[%d] = %q, want valid ULID", i, id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewULID()[%d] = %q is duplicate within %d generations", i, id, n)
		}
		seen[id] = struct{}{}
	}
}

func TestIsValidULID(t *testing.T) {
	good := []string{
		"01HABCDEFGHJKMNPQRSTVWXYZ0",
		"00000000000000000000000000",
		"ZZZZZZZZZZZZZZZZZZZZZZZZZZ",
	}
	for _, s := range good {
		if !IsValidULID(s) {
			t.Errorf("IsValidULID(%q) = false, want true", s)
		}
	}
	bad := []string{
		"",                            // empty
		"01HABC",                      // too short
		"01HABCDEFGHJKMNPQRSTVWXYZ",   // 25 chars (one short)
		"01HABCDEFGHJKMNPQRSTVWXYZ00", // 27 chars
		"01HABCDEFGHJKMNPQRSTVWXYZI",  // I forbidden in Crockford
		"01HABCDEFGHJKMNPQRSTVWXYZL",  // L forbidden
		"01HABCDEFGHJKMNPQRSTVWXYZO",  // O forbidden
		"01HABCDEFGHJKMNPQRSTVWXYZU",  // U forbidden
		"01habcdefghjkmnpqrstvwxyz0",  // lower-case
		"01HABCDEFGHJKMNPQRSTVWXYZ-",  // non-base32 char
	}
	for _, s := range bad {
		if IsValidULID(s) {
			t.Errorf("IsValidULID(%q) = true, want false", s)
		}
	}
}
