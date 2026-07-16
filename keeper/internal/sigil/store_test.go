package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// validRecord — template of a valid record for testing Insert guards. db is
// not needed: guards work BEFORE calling ExecQueryRower.
func validRecord() *Sigil {
	digest := sha256.Sum256([]byte("binary"))
	return &Sigil{
		Namespace:    "cloud",
		Name:         "hetzner",
		Ref:          "v1.0.0",
		SHA256:       hex.EncodeToString(digest[:]),
		Signature:    make([]byte, ed25519.SignatureSize),
		ManifestRaw:  []byte("kind: cloud_driver\n"),
		Manifest:     []byte(`{"kind":"cloud_driver"}`),
		AllowedByAID: "archon-a",
	}
}

// TestInsert_GuardEmptyManifestRaw — empty ManifestRaw is rejected BEFORE DB query:
// signature is placed precisely over these bytes, fallback does not apply
// (Normalize("{}") != Normalize("")). nil-db ensures guard triggered before
// QueryRow (otherwise would panic on nil).
func TestInsert_GuardEmptyManifestRaw(t *testing.T) {
	rec := validRecord()
	rec.ManifestRaw = nil
	err := Insert(context.Background(), nil, rec)
	if err == nil {
		t.Fatal("Insert with empty ManifestRaw must return error")
	}
	if !strings.Contains(err.Error(), "manifest_raw") {
		t.Errorf("error = %q, expected manifest_raw mention", err)
	}

	rec.ManifestRaw = []byte{}
	if err := Insert(context.Background(), nil, rec); err == nil {
		t.Fatal("Insert with zero-length ManifestRaw must return error")
	}
}
