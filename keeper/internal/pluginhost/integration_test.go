package pluginhost

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// sigilFor signs a valid SigilRecord over the artifact behind Discovered, through the
// SAME helpers keeper-Signer uses at Sign (BuildSigilBlock + SchemaDigest — sign↔verify
// symmetry). Returns a trust-anchor and a lookup holding the single grant, ready to
// mount on a Host. After the S6b verify-gate, Spawn fails closed without a valid grant,
// so no happy-path test passes without this.
//
// The block is keyed on (source, ref): the artifact carries no self-name, so where it
// came from is the only identity a signature can be over. The ALIAS is not in the block
// — it is only the key the lookup is stored under.
func sigilFor(t *testing.T, d Discovered) (ed25519.PublicKey, sharedhost.SigilLookup) {
	t.Helper()
	schemaBytes, err := schema.ReadTrailerFile(d.BinaryPath)
	if err != nil {
		t.Fatalf("read schema trailer for sigil: %v", err)
	}
	binBytes, err := os.ReadFile(d.BinaryPath)
	if err != nil {
		t.Fatalf("read artifact for sigil: %v", err)
	}
	binSum := sha256.Sum256(binBytes)
	binHex := hex.EncodeToString(binSum[:])

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	schemaDigest := sharedhost.SchemaDigest(schemaBytes)
	const (
		source = "https://example.com/soul-fake.git"
		ref    = "v1.0.0"
	)
	// A git-resolved slot: one artifact, no platform stated, so it answers for every
	// platform this test may run on.
	artifacts := []sharedhost.SigilArtifact{{
		OS: sharedhost.AnyPlatform, Arch: sharedhost.AnyPlatform, SHA256: binHex,
	}}
	block, err := sharedhost.BuildSigilBlock(source, sharedplugin.SourceKindGit, ref, schemaDigest[:], artifacts)
	if err != nil {
		t.Fatalf("build sigil block: %v", err)
	}
	rec := &sharedhost.SigilRecord{
		Alias:     d.Alias,
		Source:    source,
		Ref:       ref,
		Kind:      sharedplugin.SourceKindGit,
		Artifacts: artifacts,
		Signature: ed25519.Sign(priv, block),
		Schema:    schemaBytes,
	}
	return pub, testLookup{d.Alias: rec}
}

// testLookup is minimal sharedhost.SigilLookup over map, keyed by alias.
type testLookup map[string]*sharedhost.SigilRecord

func (l testLookup) Get(_ context.Context, alias string) (*sharedhost.SigilRecord, error) {
	return l[alias], nil
}

// stampBuilt appends a schema trailer to an already-built test artifact — what
// `soul-mod stamp` does in a real build. Without it the slot has no readable
// disclosure and Discover skips it, which is the fail-closed behaviour under test
// elsewhere in this package.
func stampBuilt(t *testing.T, path string, doc schema.Document) {
	t.Helper()
	payload, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if err := schema.WriteTrailerFile(path, payload); err != nil {
		t.Fatalf("stamp artifact: %v", err)
	}
}

// buildTestPlugin builds plugin from testdata/<dir> and places it in outDir with
// name outName. testdata module has separate go.mod (replace to our
// proto/plugin and sdk), so build with GOWORK=off — shouldn't be
// part of root workspace.
//
// On darwin Unix socket sun_path length limited to ~104 bytes, so
// outDir must be short (use /tmp/ss-keeper-host-, not t.TempDir).
func buildTestPlugin(t *testing.T, testdataSubdir, outDir, outName string) string {
	t.Helper()
	srcDir, err := filepath.Abs(filepath.Join("testdata", testdataSubdir))
	if err != nil {
		t.Fatalf("abs testdata: %v", err)
	}
	binPath := filepath.Join(outDir, outName)
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build %s: %v\n%s", testdataSubdir, err, out)
	}
	return binPath
}

// makeNestedSlot creates the R-nested slot (A1-S1) <cacheRoot>/<alias>/<commit>/ +
// current → <commit>, and returns the commit-slot directory the test builds its
// artifact into. commit is a synthetic fixed 40-hex.
func makeNestedSlot(t *testing.T, cacheRoot, key string) string {
	t.Helper()
	const commit = "0123456789abcdef0123456789abcdef01234567"
	pluginDir := filepath.Join(cacheRoot, key)
	slot := filepath.Join(pluginDir, commit)
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatalf("mkdir nested slot: %v", err)
	}
	if err := os.Symlink(commit, filepath.Join(pluginDir, CurrentLink)); err != nil {
		t.Fatalf("symlink current: %v", err)
	}
	return slot
}

func shortHostDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func setupSshProviderPlugin(t *testing.T) (*Host, Discovered) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin host requires Unix sockets")
	}
	cacheRoot := shortHostDir(t, "ss-kpr-mods-")
	socketDir := shortHostDir(t, "ss-kpr-sock-")
	moduleDir := makeNestedSlot(t, cacheRoot, "fake")
	binPath := buildTestPlugin(t, "ssh-plugin", moduleDir, "fake")
	stampBuilt(t, binPath, schema.Document{
		Kind:            schema.KindSSHProvider,
		ProtocolVersion: 1,
		ProviderKind:    "static_key",
	})

	found, warns, err := Discover(cacheRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// Discovery skips a slot it cannot read (unstamped artifact, invalid schema
	// document, several executables) and says why in warns. When the count is
	// wrong those warnings ARE the diagnosis, so they belong in the failure rather
	// than in a Logf the reader has to go looking for.
	if len(found) != 1 {
		t.Fatalf("expected 1 discovered plugin, got %d; discovery warnings: %v", len(found), warns)
	}

	pub, lookup := sigilFor(t, found[0])
	h := &Host{Host: &sharedhost.Host{
		SocketDir:      socketDir,
		StartupTimeout: 10 * time.Second,
		ShutdownGrace:  3 * time.Second,
		SigilAnchors:   sharedhost.NewAnchorSet([]ed25519.PublicKey{pub}),
		Sigils:         lookup,
	}}
	return h, found[0]
}

func TestSpawnSshProviderHappyPath(t *testing.T) {
	h, d := setupSshProviderPlugin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := h.Spawn(ctx, d)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	sp, err := NewSshProviderPlugin(p)
	if err != nil {
		t.Fatalf("NewSshProviderPlugin: %v", err)
	}
	defer func() {
		if err := sp.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	}()

	if sp.Discovered().Kind() != KindSSHProvider {
		t.Errorf("Kind = %q", sp.Discovered().Kind())
	}

	signReply, err := sp.Sign(ctx, &pluginv1.SignRequest{Host: "soul-1.example.com", User: "soul"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if signReply.GetCertificate() != "cert-for-soul-1.example.com" {
		t.Errorf("Sign.Certificate = %q", signReply.GetCertificate())
	}
	if signReply.GetTtlSeconds() != 1800 {
		t.Errorf("Sign.TtlSeconds = %d", signReply.GetTtlSeconds())
	}

	authReply, err := sp.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: "soul-1.example.com", User: "soul"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !authReply.GetAllowed() {
		t.Errorf("Authorize.Allowed = false, reason=%q", authReply.GetReason())
	}

	deny, err := sp.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: "soul-1.example.com", User: "denied"})
	if err != nil {
		t.Fatalf("Authorize denied: %v", err)
	}
	if deny.GetAllowed() {
		t.Errorf("Authorize.Allowed = true for denied user")
	}
	if deny.GetReason() == "" {
		t.Errorf("Authorize.Reason empty for denied user")
	}
}

func TestSpawnCloseIdempotent(t *testing.T) {
	h, d := setupSshProviderPlugin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := h.Spawn(ctx, d)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestSpawnRejectsCapabilityNotAllowed(t *testing.T) {
	h, d := setupSshProviderPlugin(t)
	// Capabilities are declared PER MODULE now, so the check needs a module entry to
	// read them from — a single-endpoint kind declares none.
	doc := schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name:         "fake",
			Capabilities: []schema.Capability{schema.VaultAccess},
			States:       map[string]schema.State{"present": {Description: "exists"}},
		}},
	}
	d.Doc = &doc
	d.Module = "fake"
	h.AllowedCapabilities = map[pluginv1.Capability]struct{}{
		pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND: {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.Spawn(ctx, d); err == nil {
		t.Fatal("expected denial for vault_access not in allowed-list")
	}
}

// TestSpawnFailsClosedNoSigil verifies a keeper-host with no grant for the alias →
// Spawn fails closed (VerifyReasonNoSigil), the artifact is not started.
func TestSpawnFailsClosedNoSigil(t *testing.T) {
	h, d := setupSshProviderPlugin(t)
	// Replace lookup with empty one (trust-anchor stays valid): no permission.
	h.Sigils = testLookup{}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.Spawn(ctx, d)
	if err == nil {
		t.Fatal("expected fail-closed Spawn without sigil")
	}
	var ve *sharedhost.VerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("error is not VerifyError: %v", err)
	}
	if ve.Reason != sharedhost.VerifyReasonNoSigil {
		t.Errorf("reason = %q, want %q", ve.Reason, sharedhost.VerifyReasonNoSigil)
	}
}

// TestSpawnFailsClosedNoTrustAnchor verifies Sigil disabled on keeper (empty anchor set)
// → Spawn fail-closed (VerifyReasonNoTrustAnchor). Intentional:
// operator with cloud/ssh must configure Sigil (G-sigil-5).
func TestSpawnFailsClosedNoTrustAnchor(t *testing.T) {
	h, d := setupSshProviderPlugin(t)
	// Permission exists (lookup from setup), but trust-anchor set empty.
	h.SigilAnchors = sharedhost.NewAnchorSet(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.Spawn(ctx, d)
	if err == nil {
		t.Fatal("expected fail-closed Spawn without trust-anchor")
	}
	var ve *sharedhost.VerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("error is not VerifyError: %v", err)
	}
	if ve.Reason != sharedhost.VerifyReasonNoTrustAnchor {
		t.Errorf("reason = %q, want %q", ve.Reason, sharedhost.VerifyReasonNoTrustAnchor)
	}
}

func TestNewSshProviderPluginRejectsWrongKind(t *testing.T) {
	// A soul_module artifact fed into the ssh wrapper. Since the CloudDriver
	// contract was removed (NIM-761) soul_module is the only other kind the keeper
	// host handles, so it is what the wrapper must still refuse.
	doc := schema.Document{Kind: schema.KindSoulModule, ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name:   "acl",
			States: map[string]schema.State{"present": {Description: "the resource exists"}},
		}}}
	p := &Plugin{BasePlugin: sharedhost.NewBasePluginForTest(
		Discovered{Alias: "x", Doc: &doc},
	)}
	if _, err := NewSshProviderPlugin(p); err == nil {
		t.Fatal("expected error when wrapping soul_module Plugin as SshProviderPlugin")
	}
}

// TestSpawnParallel verifies multiple Spawns work correctly in parallel
// (different sockets, no name collisions).
func TestSpawnParallel(t *testing.T) {
	h, d := setupSshProviderPlugin(t)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p, err := h.Spawn(ctx, d)
			if err != nil {
				errs[i] = err
				return
			}
			defer p.Close()
			sp, err := NewSshProviderPlugin(p)
			if err != nil {
				errs[i] = err
				return
			}
			if _, err := sp.Authorize(ctx, &pluginv1.AuthorizeRequest{
				Host: "soul-1.example.com", User: "deploy",
			}); err != nil {
				errs[i] = err
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
}
