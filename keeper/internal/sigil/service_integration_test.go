//go:build integration

// Integration tests for sigil.Service (Allow → cache-read → sign → Insert; Revoke;
// List) on real PG (testcontainers) + a temp cache directory. Shares TestMain /
// integrationPool / reset with store_integration_test.go.

package sigil

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/sdk/schema"
)

// integrationCommitSHA is the synthetic commit the fixture slot is named by.
const integrationCommitSHA = "0123456789abcdef0123456789abcdef01234567"

// writeCacheSlot creates an R-nested slot (A1-S1) `<root>/<alias>/<commit>/` +
// current → <commit>, holding ONE executable artifact with doc stamped into its
// trailer. That is the whole slot: the schema lives inside the artifact, so there is no
// second file for a reader to disagree with.
func writeCacheSlot(t *testing.T, root, alias string, doc schema.Document, body []byte) {
	t.Helper()
	pluginDir := filepath.Join(root, alias)
	dir := filepath.Join(pluginDir, integrationCommitSHA)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir slot: %v", err)
	}
	payload, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, alias), schema.AppendTrailer(body, payload), 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if err := os.Symlink(integrationCommitSHA, filepath.Join(pluginDir, pluginhost.CurrentLink)); err != nil {
		t.Fatalf("symlink current: %v", err)
	}
}

func integrationSSHDoc() schema.Document {
	return schema.Document{
		Kind:            schema.KindSSHProvider,
		ProtocolVersion: 1,
		ProviderKind:    "static_key",
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
	writeCacheSlot(t, cacheRoot, "hetzner", integrationSSHDoc(), []byte("integration-cloud-binary"))

	svc := newIntegrationService(t, cacheRoot)

	// Allow: reads the slot, signs, inserts; returns the approved release.
	approved, err := svc.Allow(ctx, AllowInput{Alias: "hetzner", Source: testSource, Ref: "v1.0.0", CallerAID: aid})
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if approved.Kind != sharedplugin.SourceKindGit || len(approved.Artifacts) != 1 {
		t.Fatalf("Allow returned %+v, want one git artifact", approved)
	}
	sha := approved.Artifacts[0].SHA256
	if !reSHA256Hex.MatchString(sha) {
		t.Errorf("Allow returned invalid sha256 %q", sha)
	}

	// List: one active grant, no signature/schema in SigilView (by type).
	views, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("List len = %d, want 1", len(views))
	}
	if len(views[0].Artifacts) != 1 || views[0].Artifacts[0].SHA256 != sha ||
		views[0].Kind != sharedplugin.SourceKindGit ||
		views[0].Alias != "hetzner" || views[0].Source != testSource || views[0].Ref != "v1.0.0" {
		t.Errorf("view = %+v", views[0])
	}
	if views[0].AllowedByAID != aid {
		t.Errorf("allowed_by_aid = %q, want %q", views[0].AllowedByAID, aid)
	}

	// The DB row carries the real signature and the byte-exact schema — read it back
	// through the lookup path (GetActive by alias).
	rec, err := GetActive(ctx, integrationPool, "hetzner")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if len(rec.Signature) != ed25519.SignatureSize {
		t.Errorf("signature len = %d, want %d", len(rec.Signature), ed25519.SignatureSize)
	}
	// The stored schema must survive the PG round-trip byte for byte: it is what the
	// signature covers and what a Soul re-hashes at verify.
	wantSchema, err := schema.Marshal(integrationSSHDoc())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(rec.Schema, wantSchema) {
		t.Errorf("schema round-trip:\n got=%q\nwant=%q", rec.Schema, wantSchema)
	}
	// A1-S4: the commit_sha of the current-target slot is written to plugin_sigils
	// (audit origin marker, outside the signature).
	if rec.CommitSHA != integrationCommitSHA {
		t.Errorf("commit_sha = %q, want the current-target slot", rec.CommitSHA)
	}

	// Re-allow without revoking → 409. Both keys are live here, and the alias one
	// trips first because this repeats the identical registration.
	if _, err := svc.Allow(ctx, AllowInput{Alias: "hetzner", Source: testSource, Ref: "v1.0.0", CallerAID: aid}); err == nil {
		t.Error("repeated Allow of an active grant must conflict")
	}

	// Revoke: no active grant left → List is empty.
	if err := svc.Revoke(ctx, "hetzner", aid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	views, err = svc.List(ctx)
	if err != nil {
		t.Fatalf("List after Revoke: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("after Revoke List len = %d, want 0", len(views))
	}

	// Revoking a non-existent active grant → ErrSigilNotFound.
	if err := svc.Revoke(ctx, "hetzner", aid); !errors.Is(err, ErrSigilNotFound) {
		t.Errorf("Revoke without an active grant: err = %v, want ErrSigilNotFound", err)
	}
}

// TestIntegration_Service_Allow_SecondAliasSameSourceRefConflicts is NIM-438 exercised
// end to end. The TRUST key is (source, ref): approving the same artifact identity a
// second time — even under a different alias — is a conflict, not a second silent grant
// that a revoke of the first would leave standing.
func TestIntegration_Service_Allow_SecondAliasSameSourceRefConflicts(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	cacheRoot := t.TempDir()
	writeCacheSlot(t, cacheRoot, "redis", integrationSSHDoc(), []byte("same-bytes"))
	writeCacheSlot(t, cacheRoot, "redis-community", integrationSSHDoc(), []byte("same-bytes"))

	svc := newIntegrationService(t, cacheRoot)

	if _, err := svc.Allow(ctx, AllowInput{Alias: "redis", Source: testSource, Ref: "v1.0.0", CallerAID: aid}); err != nil {
		t.Fatalf("first Allow: %v", err)
	}
	_, err := svc.Allow(ctx, AllowInput{Alias: "redis-community", Source: testSource, Ref: "v1.0.0", CallerAID: aid})
	if !errors.Is(err, ErrSigilAlreadyActive) {
		t.Fatalf("second alias on the same (source, ref): err = %v, want ErrSigilAlreadyActive", err)
	}
}

// TestIntegration_Service_Allow_AliasTakenByOtherSource — the registration invariant.
// One alias is one address space and one host slot, so a second live grant may not
// claim it even when it is on a different artifact.
func TestIntegration_Service_Allow_AliasTakenByOtherSource(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	cacheRoot := t.TempDir()
	writeCacheSlot(t, cacheRoot, "redis", integrationSSHDoc(), []byte("bytes"))

	svc := newIntegrationService(t, cacheRoot)

	if _, err := svc.Allow(ctx, AllowInput{Alias: "redis", Source: testSource, Ref: "v1.0.0", CallerAID: aid}); err != nil {
		t.Fatalf("first Allow: %v", err)
	}
	_, err := svc.Allow(ctx, AllowInput{
		Alias: "redis", Source: "https://example.com/somebody-elses-redis.git", Ref: "v2.0.0", CallerAID: aid,
	})
	if !errors.Is(err, ErrAliasAlreadyRegistered) {
		t.Fatalf("alias reuse: err = %v, want ErrAliasAlreadyRegistered", err)
	}
}

// TestIntegration_Service_Allow_ReservedAliasRejected — a reserved alias never reaches
// the registry, even with a perfectly good slot behind it.
func TestIntegration_Service_Allow_ReservedAliasRejected(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	cacheRoot := t.TempDir()
	writeCacheSlot(t, cacheRoot, "core", integrationSSHDoc(), []byte("bytes"))

	svc := newIntegrationService(t, cacheRoot)

	_, err := svc.Allow(ctx, AllowInput{Alias: "core", Source: testSource, Ref: "v1.0.0", CallerAID: aid})
	if !errors.Is(err, ErrAliasReserved) {
		t.Fatalf("alias core: err = %v, want ErrAliasReserved", err)
	}
	views, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("a reserved alias reached the registry: %+v", views)
	}
}

// TestIntegration_Service_Allow_UnstampedArtifactRejected — the slot holds an artifact
// with no trailer, so there is no disclosure to approve. Fail closed, and nothing is
// written.
func TestIntegration_Service_Allow_UnstampedArtifactRejected(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	cacheRoot := t.TempDir()
	pluginDir := filepath.Join(cacheRoot, "hetzner")
	dir := filepath.Join(pluginDir, integrationCommitSHA)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hetzner"), []byte("no trailer here"), 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if err := os.Symlink(integrationCommitSHA, filepath.Join(pluginDir, pluginhost.CurrentLink)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	svc := newIntegrationService(t, cacheRoot)
	if _, err := svc.Allow(ctx, AllowInput{Alias: "hetzner", Source: testSource, Ref: "v1.0.0", CallerAID: aid}); err == nil {
		t.Fatal("Allow on an unstamped artifact must fail")
	}
	views, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("an unstamped artifact reached the registry: %+v", views)
	}
}

func TestIntegration_Service_Allow_PluginNotInCache(t *testing.T) {
	aid := reset(t)
	ctx := context.Background()

	cacheRoot := t.TempDir() // empty cache, no slot
	svc := newIntegrationService(t, cacheRoot)

	_, err := svc.Allow(ctx, AllowInput{Alias: "absent", Source: testSource, Ref: "v1.0.0", CallerAID: aid})
	if !errors.Is(err, ErrPluginNotInCache) {
		t.Fatalf("Allow without a slot: err = %v, want ErrPluginNotInCache", err)
	}
}
