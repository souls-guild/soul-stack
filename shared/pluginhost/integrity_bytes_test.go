package pluginhost

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// bytesTestEnv — in-memory artifact bytes + a consistent valid SigilRecord
// (install-flow core.module.installed, ADR-065: verify BEFORE materialization).
func setupBytesEnv(t *testing.T) ([]byte, *SigilRecord, *AnchorSet) {
	t.Helper()
	schemaDoc, err := schema.Marshal(soulModuleDoc(modDef("acl", nil, nil)))
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	data := []byte(exitScript)
	sum := sha256.Sum256(data)
	digestHex := hex.EncodeToString(sum[:])

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	rec := &SigilRecord{
		Alias:     testAlias,
		Source:    testSource,
		Ref:       testRef,
		Kind:      sharedplugin.SourceKindGit,
		Artifacts: gitArtifacts(digestHex),
		Signature: signFixture(t, priv, testSource, sharedplugin.SourceKindGit, testRef,
			gitArtifacts(digestHex), schemaDoc),
		Schema: schemaDoc,
	}
	return data, rec, NewAnchorSet([]ed25519.PublicKey{pub})
}

// approvedOf is what the install flow passes: the row it selected and fetched. The
// caller selects, verify checks — one decision, so a platform spelled two ways cannot
// surface as a digest mismatch.
func approvedOf(rec *SigilRecord) *SigilArtifact {
	if rec == nil || len(rec.Artifacts) == 0 {
		return nil
	}
	return &rec.Artifacts[0]
}

func TestVerifyArtifactBytesSuccess(t *testing.T) {
	data, rec, anchors := setupBytesEnv(t)
	if err := VerifyArtifactBytes(data, rec, approvedOf(rec), anchors); err != nil {
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
			name: "schema tampered",
			mutate: func(data []byte, rec *SigilRecord, anchors *AnchorSet) ([]byte, *SigilRecord, *AnchorSet) {
				rec.Schema = append(rec.Schema, ' ')
				return data, rec, anchors
			},
			reason: VerifyReasonBadSignature,
		},
		{
			name: "source tampered",
			mutate: func(data []byte, rec *SigilRecord, anchors *AnchorSet) ([]byte, *SigilRecord, *AnchorSet) {
				rec.Source = "https://evil.example.com/redis"
				return data, rec, anchors
			},
			reason: VerifyReasonBadSignature,
		},
		{
			// The caller found no row for this host: fail-closed, and NOT as a digest
			// mismatch — nothing was approved to compare against, so the operator's
			// fix is a release that covers the platform, not an investigation.
			name: "no artifact for this platform",
			mutate: func(data []byte, rec *SigilRecord, anchors *AnchorSet) ([]byte, *SigilRecord, *AnchorSet) {
				rec.Artifacts = nil
				return data, rec, anchors
			},
			reason: VerifyReasonNoArtifactForPlatform,
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

			err := VerifyArtifactBytes(data, rec, approvedOf(rec), anchors)
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
