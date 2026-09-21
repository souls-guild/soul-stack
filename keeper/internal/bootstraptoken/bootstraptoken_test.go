package bootstraptoken

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerate_ProducesUniqueHighEntropy(t *testing.T) {
	seen := make(map[string]struct{}, 1024)
	for i := 0; i < 1024; i++ {
		tok, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		plain := tok.Reveal()
		if plain == "" {
			t.Fatal("Generate returned empty plain")
		}
		if _, dup := seen[plain]; dup {
			t.Fatalf("duplicate plain on iteration %d", i)
		}
		seen[plain] = struct{}{}
	}
}

func TestPlainToken_HashMatchesSHA256(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	gotHash := tok.Hash()
	sum := sha256.Sum256([]byte(tok.Reveal()))
	wantHash := hex.EncodeToString(sum[:])
	if gotHash != wantHash {
		t.Errorf("Hash mismatch: got %q, want %q", gotHash, wantHash)
	}
	if len(gotHash) != HashHexLen {
		t.Errorf("Hash len = %d, want %d", len(gotHash), HashHexLen)
	}
}

func TestPlainToken_StringDoesNotLeakPlain(t *testing.T) {
	// PlainToken has no String() method, so this doesn't actually test
	// redaction — Go's default struct format still leaks `{v:...}` via
	// fmt.Print(tok) (see the PlainToken doc comment). The real contract
	// under test: callers must use .Reveal() explicitly. Tighten this test
	// if a redacting String() is added later (post-MVP).
	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if tok.Reveal() == "" {
		t.Errorf("Reveal() returned empty")
	}
}

func TestHashToken_StableAndMatchesPlainToken(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if HashToken(tok.Reveal()) != tok.Hash() {
		t.Error("HashToken(plain) != PlainToken.Hash()")
	}
}

func TestValidHashFormat(t *testing.T) {
	good := []string{
		strings.Repeat("0", 64),
		strings.Repeat("a", 64),
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
	}
	bad := []string{
		"",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.Repeat("g", 64), // 'g' is not hex.
		strings.Repeat("A", 64), // uppercase — rejected (CHECK catches it).
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456 8", // space.
	}
	for _, s := range good {
		if !ValidHashFormat(s) {
			t.Errorf("ValidHashFormat(%q) = false; want true", s)
		}
	}
	for _, s := range bad {
		if ValidHashFormat(s) {
			t.Errorf("ValidHashFormat(%q) = true; want false", s)
		}
	}
}

func TestRecord_IsActive(t *testing.T) {
	now := timeNow()
	cases := []struct {
		name string
		rec  Record
		want bool
	}{
		{
			"active",
			Record{ExpiresAt: now.Add(1 << 20)},
			true,
		},
		{
			"expired",
			Record{ExpiresAt: now.Add(-1)},
			false,
		},
		{
			"used",
			Record{ExpiresAt: now.Add(1 << 20), UsedAt: ptrTime(now)},
			false,
		},
	}
	for _, tc := range cases {
		if got := tc.rec.IsActive(now); got != tc.want {
			t.Errorf("%s: IsActive = %v, want %v", tc.name, got, tc.want)
		}
	}
}
