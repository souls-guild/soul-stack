package pluginhost

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"google.golang.org/protobuf/types/known/structpb"
)

// sigilFor signs a valid SigilRecord for the binary+manifest from Discovered,
// using the same helper Keeper uses for Sign (BuildSigilBlock +
// NormalizeManifestBytes — sign/verify symmetry). Returns a trust-anchor and
// a lookup with the single grant, ready to attach to a Host.
func sigilFor(t *testing.T, d Discovered) (ed25519.PublicKey, sharedhost.SigilLookup) {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join(d.Dir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest for sigil: %v", err)
	}
	binDigest := fileSHA256Hex(t, d.BinaryPath)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	binRaw, _ := hex.DecodeString(binDigest)
	manDigest := sha256.Sum256(sharedhost.NormalizeManifestBytes(manifest))
	const ref = "v1.0.0"
	block := sharedhost.BuildSigilBlock(d.Manifest.Namespace, d.Manifest.Name, ref, binRaw, manDigest[:])
	rec := &sharedhost.SigilRecord{
		Namespace:       d.Manifest.Namespace,
		Name:            d.Manifest.Name,
		Ref:             ref,
		BinarySHA256hex: binDigest,
		Signature:       ed25519.Sign(priv, block),
		Manifest:        manifest,
	}
	return pub, testLookup{d.Manifest.Namespace + "." + d.Manifest.Name: rec}
}

// testLookup is a minimal sharedhost.SigilLookup backed by a map.
type testLookup map[string]*sharedhost.SigilRecord

func (l testLookup) Get(ns, name string) *sharedhost.SigilRecord { return l[ns+"."+name] }

func fileSHA256Hex(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// buildEchoPlugin builds the testdata/echo-plugin test plugin and places it in
// outDir as `soul-mod-echo`. Returns the absolute path to the binary.
//
// Builds with GOWORK=off because the plugin is a separate go.mod module under
// testdata/ (not formally part of the workspace; otherwise go tooling would
// require its inclusion in the root go.work).
//
// On darwin, Unix socket sun_path length is capped at ~104 bytes; outDir must
// be short (use /tmp/ss-host-, not t.TempDir).
func buildEchoPlugin(t *testing.T, outDir string) string {
	t.Helper()
	srcDir, err := filepath.Abs("testdata/echo-plugin")
	if err != nil {
		t.Fatalf("abs testdata: %v", err)
	}
	binPath := filepath.Join(outDir, "soul-mod-echo")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build echo plugin: %v\n%s", err, out)
	}
	return binPath
}

// shortHostDir is a short /tmp directory for socket+modules: on darwin
// `t.TempDir()` lives under /var/folders/... and exceeds the unix sun_path
// length limit. Safe on linux too, but a single approach is simpler.
func shortHostDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func setupHostAndDiscovered(t *testing.T) (*Host, Discovered, func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin host requires Unix sockets")
	}
	modulesRoot := shortHostDir(t, "ss-mods-")
	socketDir := shortHostDir(t, "ss-sock-")
	moduleDir := filepath.Join(modulesRoot, "wb-echo")
	if err := os.Mkdir(moduleDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binPath := buildEchoPlugin(t, moduleDir)
	if err := os.WriteFile(filepath.Join(moduleDir, "manifest.yaml"), []byte(`kind: soul_module
protocol_version: 1
namespace: wb
name: echo
required_capabilities: []
side_effects: []
spec:
  states:
    applied:
      description: Echo applied.
      input:
        name: { type: string, required: true }
    fail:
      description: Force failure for tests.
      input: {}
`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	found, warns, err := Discover(modulesRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warns) != 0 {
		t.Logf("discovery warnings: %v", warns)
	}
	if len(found) != 1 {
		t.Fatalf("expected 1 discovered plugin, got %d", len(found))
	}

	pub, sigils := sigilFor(t, found[0])
	h := &Host{Host: &sharedhost.Host{
		SocketDir:      socketDir,
		StartupTimeout: 10 * time.Second,
		ShutdownGrace:  3 * time.Second,
		SigilAnchors:   sharedhost.NewAnchorSet([]ed25519.PublicKey{pub}),
		Sigils:         sigils,
	}}
	_ = binPath // marks it used
	return h, found[0], func() {}
}

func TestSpawnApplyHappyPath(t *testing.T) {
	h, d, cleanup := setupHostAndDiscovered(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := h.Spawn(ctx, d)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	}()

	if p.Manifest().Address() != "wb.echo" {
		t.Errorf("Manifest.Address = %q", p.Manifest().Address())
	}

	params, _ := structpb.NewStruct(map[string]any{"name": "world"})
	vr, err := p.Validate(ctx, &pluginv1.ValidateRequest{State: "applied", Params: params})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !vr.GetOk() {
		t.Errorf("Validate.Ok = false, errors=%v", vr.GetErrors())
	}

	planStream, err := p.Plan(ctx, &pluginv1.PlanRequest{State: "applied", Params: params})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var planMsgs []string
	for {
		ev, err := planStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("plan recv: %v", err)
		}
		planMsgs = append(planMsgs, ev.GetMessage())
	}
	if len(planMsgs) != 2 {
		t.Errorf("plan messages = %d, want 2: %v", len(planMsgs), planMsgs)
	}

	applyStream, err := p.Apply(ctx, &pluginv1.ApplyRequest{State: "applied", Params: params})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var (
		applied bool
		echoed  string
	)
	for {
		ev, err := applyStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("apply recv: %v", err)
		}
		if ev.GetChanged() {
			applied = true
			if v, ok := ev.GetOutput().GetFields()["echo"]; ok {
				echoed = v.GetStringValue()
			}
		}
	}
	if !applied {
		t.Errorf("apply did not report changed=true")
	}
	if echoed != "world" {
		t.Errorf("apply output echo = %q, want %q", echoed, "world")
	}
}

func TestSpawnApplyValidationFailure(t *testing.T) {
	h, d, cleanup := setupHostAndDiscovered(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := h.Spawn(ctx, d)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	vr, err := p.Validate(ctx, &pluginv1.ValidateRequest{State: "applied"}) // no name
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if vr.GetOk() {
		t.Errorf("expected Ok=false (missing name), got Ok=true")
	}
}

func TestSpawnCloseIdempotent(t *testing.T) {
	h, d, cleanup := setupHostAndDiscovered(t)
	defer cleanup()

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
	h, d, cleanup := setupHostAndDiscovered(t)
	defer cleanup()

	// The plugin manifest in our fixture has required_capabilities: [].
	// Feed it a non-empty required: vault_access, while the host only allows
	// network_outbound.
	d.Manifest.RequiredCapabilities = []string{"vault_access"}
	h.AllowedCapabilities = map[pluginv1.Capability]struct{}{
		pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND: {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.Spawn(ctx, d); err == nil {
		t.Fatal("expected denial for vault_access not in allowed-list")
	}
}

// TestSpawnParallel verifies multiple concurrent Spawns work correctly
// (distinct sockets, no name collisions).
func TestSpawnParallel(t *testing.T) {
	h, d, cleanup := setupHostAndDiscovered(t)
	defer cleanup()

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
			params, _ := structpb.NewStruct(map[string]any{"name": "x"})
			if _, err := p.Validate(ctx, &pluginv1.ValidateRequest{State: "applied", Params: params}); err != nil {
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
