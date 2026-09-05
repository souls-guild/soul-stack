package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// validRecord — a template of a valid row for testing Insert's guards. No db is
// needed: the guards run BEFORE any ExecQueryRower call.
func validRecord() *Sigil {
	digest := sha256.Sum256([]byte("binary"))
	return &Sigil{
		Alias:        "hetzner",
		Source:       testSource,
		Ref:          "v1.0.0",
		SHA256:       hex.EncodeToString(digest[:]),
		Signature:    make([]byte, ed25519.SignatureSize),
		Schema:       []byte(`{"kind":"ssh_provider","protocol_version":1}`),
		AllowedByAID: "archon-a",
	}
}

// TestInsert_GuardEmptySchema — an empty Schema is rejected BEFORE the DB query: the
// signature was placed over exactly those bytes, and nothing could reconstruct them, so
// a row without them is a grant that can only ever fail closed later. A nil db proves
// the guard fires before QueryRow (it would panic otherwise).
func TestInsert_GuardEmptySchema(t *testing.T) {
	rec := validRecord()
	rec.Schema = nil
	err := Insert(context.Background(), nil, rec)
	if err == nil {
		t.Fatal("Insert with an empty schema must return an error")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error = %q, expected it to mention schema", err)
	}

	rec.Schema = []byte{}
	if err := Insert(context.Background(), nil, rec); err == nil {
		t.Fatal("Insert with a zero-length schema must return an error")
	}
}

// TestInsert_GuardEmptyIdentity — alias and source are both required, and for different
// reasons: without an alias no host could ever look the grant up, and without a source
// the signature covers no identity at all.
func TestInsert_GuardEmptyIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mangl func(*Sigil)
		want  string
	}{
		{"alias", func(s *Sigil) { s.Alias = "" }, "alias"},
		{"source", func(s *Sigil) { s.Source = "" }, "source"},
		{"ref", func(s *Sigil) { s.Ref = "" }, "ref"},
	} {
		rec := validRecord()
		tc.mangl(rec)
		err := Insert(context.Background(), nil, rec)
		if err == nil {
			t.Fatalf("Insert with an empty %s must return an error", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, expected it to mention %q", tc.name, err, tc.want)
		}
	}
}
