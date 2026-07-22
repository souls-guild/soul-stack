package pluginhost

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// bytesTestEnv — in-memory artifact bytes + a consistent valid SigilRecord
// (install-flow core.module.installed, ADR-065: verify BEFORE materialization).
func setupBytesEnv(t *testing.T) ([]byte, *SigilRecord, *AnchorSet) {
	t.Helper()
	const (
		ns       = "acme"
		name     = "x"
		ref      = "v1.0.0"
		manifest = "kind: soul_module\nnamespace: acme\nname: x\nprotocol_version: 1\n"
	)
	data := []byte("#!/bin/sh\nexit 0\n")
	sum := sha256.Sum256(data)
	digestHex := hex.EncodeToString(sum[:])

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	rec := &SigilRecord{
		Namespace:       ns,
		Name:            name,
		Ref:             ref,
		BinarySHA256hex: digestHex,
		Signature:       signFixture(t, priv, ns, name, ref, digestHex, []byte(manifest)),
		Manifest:        []byte(manifest),
	}
	return data, rec, NewAnchorSet([]ed25519.PublicKey{pub})
}

func TestVerifyArtifactBytesSuccess(t *testing.T) {
	data, rec, anchors := setupBytesEnv(t)
	if err := VerifyArtifactBytes(data, rec, anchors); err != nil {
		t.Fatalf("VerifyArtifactBytes: %v", err)
	}
}

func TestVerifyArtifactBytesFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(data []byte, rec *SigilRecord, anchors *AnchorSet) ([]byte, *SigilRecord, *AnchorSet)
		reason VerifyReason
	}{
		{
			name: "digest mismatch",
			mutate: func(data []byte, rec *SigilRecord, anchors *AnchorSet) ([]byte, *SigilRecord, *AnchorSet) {
				return append(data, '!'), rec, anchors
			},
			reason: VerifyReasonDigestMismatch,
		},
		{
			name: "bad signature",
			mutate: func(data []byte, rec *SigilRecord, anchors *AnchorSet) ([]byte, *SigilRecord, *AnchorSet) {
				rec.Signature = make([]byte, ed25519.SignatureSize)
				return data, rec, anchors
			},
			reason: VerifyReasonBadSignature,
		},
		{
			name: "manifest tampered",
			mutate: func(data []byte, rec *SigilRecord, anchors *AnchorSet) ([]byte, *SigilRecord, *AnchorSet) {
				rec.Manifest = append(rec.Manifest, []byte("\n# tampered\n")...)
				return data, rec, anchors
			},
			reason: VerifyReasonBadSignature,
		},
		{
			name: "no trust anchors",
			mutate: func(data []byte, rec *SigilRecord, _ *AnchorSet) ([]byte, *SigilRecord, *AnchorSet) {
				return data, rec, NewAnchorSet(nil)
			},
			reason: VerifyReasonNoTrustAnchor,
		},
		{
			name: "nil record",
			mutate: func(data []byte, _ *SigilRecord, anchors *AnchorSet) ([]byte, *SigilRecord, *AnchorSet) {
				return data, nil, anchors
			},
			reason: VerifyReasonNoSigil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, rec, anchors := setupBytesEnv(t)
			data, rec, anchors = tc.mutate(data, rec, anchors)

			err := VerifyArtifactBytes(data, rec, anchors)
			if !errors.Is(err, ErrSigilVerify) {
				t.Fatalf("err = %v; expected ErrSigilVerify", err)
			}
			var verr *VerifyError
			if !errors.As(err, &verr) || verr.Reason != tc.reason {
				t.Fatalf("reason = %v; want %v", verr, tc.reason)
			}
		})
	}
}
