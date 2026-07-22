//go:build integration

// Integration tests for sigil.Service (Allow → cache-read → sign → Insert; Revoke;
// List) on real PG (testcontainers) + temp cache directory. Shares TestMain /
// integrationPool / reset with store_integration_test.go.

package sigil

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
)

const cloudManifestIntegration = `kind: cloud_driver
protocol_version: 1
namespace: cloud
name: hetzner
spec:
  profile_schema:
    type: object
`

// writeCacheSlot creates an R-nested slot (A1-S1): <root>/<ns>-<name>/<commit>/ +
// current → <commit> with manifest.yaml and binary per convention soul-cloud-<name>.
// ReadSlot reads active slot via current.
func writeCacheSlot(t *testing.T, root, ns, name string, manifest, binary []byte) {
	t.Helper()
	const commit = "0123456789abcdef0123456789abcdef01234567"
	pluginDir := filepath.Join(root, ns+"-"+name)
	dir := filepath.Join(pluginDir, commit)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir slot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), manifest, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "soul-cloud-"+name), binary, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.Symlink(commit, filepath.Join(pluginDir, pluginhost.CurrentLink)); err != nil {
		t.Fatalf("symlink current: %v", err)
	}
}

func newIntegrationService(t *testing.T, cacheRoot string) *Service {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer, err := NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	svc, err := NewService(ServiceDeps{
		Signer: signer,
		Store:  NewPGStore(integrationPool),
		Slots:  NewCacheSlotReader(cacheRoot),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestIntegration_Service_Allow_List_Revoke(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	cacheRoot := t.TempDir()
	binary := []byte("integration-cloud-binary")
	writeCacheSlot(t, cacheRoot, "cloud", "hetzner", []byte(cloudManifestIntegration), binary)

	svc := newIntegrationService(t, cacheRoot)

	// Allow: reads slot, signs, inserts; returns sha256.
	sha, err := svc.Allow(ctx, AllowInput{Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0", CallerAID: aid})
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !reSHA256Hex.MatchString(sha) {
		t.Errorf("Allow returned invalid sha256 %q", sha)
	}

	// List: one active record, no signature/manifest in SigilView (by type).
	views, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("List len = %d, want 1", len(views))
	}
	if views[0].SHA256 != sha || views[0].Namespace != "cloud" || views[0].Ref != "v1.0.0" {
		t.Errorf("view = %+v", views[0])
	}
	if views[0].AllowedByAID != aid {
		t.Errorf("allowed_by_aid = %q, want %q", views[0].AllowedByAID, aid)
	}

	// DB record carries real signature and manifest JSONB — verify directly
	// via GetActive (S3 lookup path).
	rec, err := GetActive(ctx, integrationPool, "cloud", "hetzner", "v1.0.0")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if len(rec.Signature) != ed25519.SignatureSize {
		t.Errorf("signature len = %d, want %d", len(rec.Signature), ed25519.SignatureSize)
	}
	if len(rec.Manifest) == 0 {
		t.Error("manifest JSONB is empty")
	}
	// A1-S4: commit_sha from current-target slot is written to plugin_sigils
	// (audit origin marker, outside signature).
	if rec.CommitSHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("commit_sha = %q, want current-target slot", rec.CommitSHA)
	}

	// Re-allow without revoke → 409 (ErrSigilAlreadyActive).
	if _, err := svc.Allow(ctx, AllowInput{Namespace: "cloud", Name: "hetzner", Ref: "v1.0.0", CallerAID: aid}); err == nil {
		t.Error("repeated Allow of active record must return ErrSigilAlreadyActive")
	}

	// Revoke: no active record left → List is empty.
	if err := svc.Revoke(ctx, "cloud", "hetzner", "v1.0.0", aid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	views, err = svc.List(ctx)
	if err != nil {
		t.Fatalf("List after Revoke: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("after Revoke List len = %d, want 0", len(views))
	}

	// Revoke of non-existent active record → ErrSigilNotFound.
	if err := svc.Revoke(ctx, "cloud", "hetzner", "v1.0.0", aid); err == nil {
		t.Error("Revoke without active record must return ErrSigilNotFound")
	}
}

func TestIntegration_Service_Allow_PluginNotInCache(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	cacheRoot := t.TempDir() // empty cache, no slot
	svc := newIntegrationService(t, cacheRoot)

	_, err := svc.Allow(ctx, AllowInput{Namespace: "cloud", Name: "absent", Ref: "v1.0.0", CallerAID: aid})
	if err == nil {
		t.Fatal("Allow without slot should return ErrPluginNotInCache")
	}
}
