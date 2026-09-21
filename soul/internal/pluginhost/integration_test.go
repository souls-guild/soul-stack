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
	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"google.golang.org/protobuf/types/known/structpb"
)

// testAlias is the registration the test slot is named by. It is the operator's
// choice, and the artifact knows nothing about it.
const testAlias = "acme-echo"

// sigilFor signs a valid SigilRecord for a discovered artifact, using the same helpers
// Keeper uses at Sign (BuildSigilBlock + SchemaDigest — sign/verify symmetry). The
// grant is filed under the registration ALIAS and signed over the SOURCE, which is the
// split the whole model rests on. Returns a trust-anchor and a lookup holding the
// single grant, ready to attach to a Host.
func sigilFor(t *testing.T, d Discovered) (ed25519.PublicKey, sharedhost.SigilLookup) {
	t.Helper()
	schemaDoc, err := schema.ReadTrailerFile(d.BinaryPath)
	if err != nil {
		t.Fatalf("read stamped schema: %v", err)
	}
	binDigest := fileSHA256Hex(t, d.BinaryPath)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	schemaDigest := sharedhost.SchemaDigest(schemaDoc)
	const (
		ref    = "v1.0.0"
		source = "https://github.com/souls-guild/echo"
	)
	// A git-resolved grant: one artifact, no platform stated, so it answers on
	// whatever platform this test runs on.
	artifacts := []sharedhost.SigilArtifact{{
		OS: sharedhost.AnyPlatform, Arch: sharedhost.AnyPlatform, SHA256: binDigest,
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
		Schema:    schemaDoc,
	}
	return pub, testLookup{d.Alias: rec}
}

// testLookup is a minimal sharedhost.SigilLookup backed by a map keyed by alias.
type testLookup map[string]*sharedhost.SigilRecord

func (l testLookup) Get(_ context.Context, alias string) (*sharedhost.SigilRecord, error) {
	return l[alias], nil
}

func fileSHA256Hex(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stampArtifact does what `soul-mod stamp` does: ask the artifact for its own schema
// document and append it as a trailer. Going through the artifact's `schema`
// subcommand rather than rebuilding the document in the test is the point — it is the
// same path the real build uses, so a drift between what the bundle serves and what it
// declares would show up here.
func stampArtifact(t *testing.T, binPath string) {
	t.Helper()
	out, err := exec.Command(binPath, schema.SchemaSubcommand).Output()
	if err != nil {
		t.Fatalf("%s schema: %v", binPath, err)
	}
	if err := schema.WriteTrailerFile(binPath, out); err != nil {
		t.Fatalf("stamp %s: %v", binPath, err)
	}
}

// buildEchoPlugin builds the testdata/echo-plugin test plugin and places it in
// outDir as `echo`. Returns the absolute path to the binary.
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
	binPath := filepath.Join(outDir, "echo")
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

// setupHostAndDiscovered builds and stamps the echo bundle into one slot, discovers
// it, and returns the host plus the discovered entries keyed by MODULE name. One
// artifact, two addressable modules — the shape everything below is about.
func setupHostAndDiscovered(t *testing.T) (*Host, map[string]Discovered, func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin host requires Unix sockets")
	}
	modulesRoot := shortHostDir(t, "ss-mods-")
	socketDir := shortHostDir(t, "ss-sock-")
	moduleDir := filepath.Join(modulesRoot, testAlias)
	if err := os.Mkdir(moduleDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binPath := buildEchoPlugin(t, moduleDir)
	stampArtifact(t, binPath)

	found, warns, err := Discover(modulesRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warns) != 0 {
		t.Logf("discovery warnings: %v", warns)
	}
	if len(found) != 2 {
		t.Fatalf("expected 2 discovered modules from the bundle, got %d", len(found))
	}
	mods := make(map[string]Discovered, len(found))
	for _, d := range found {
		mods[d.Module] = d
	}

	pub, sigils := sigilFor(t, found[0])
	h := &Host{Host: &sharedhost.Host{
		SocketDir:      socketDir,
		StartupTimeout: 10 * time.Second,
		ShutdownGrace:  3 * time.Second,
		SigilAnchors:   sharedhost.NewAnchorSet([]ed25519.PublicKey{pub}),
		Sigils:         sigils,
	}}
	return h, mods, func() {}
}

func TestSpawnApplyHappyPath(t *testing.T) {
	h, mods, cleanup := setupHostAndDiscovered(t)
	defer cleanup()
	d := mods["echo"]

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

	if got := p.Discovered().Address(); got != testAlias+".echo" {
		t.Errorf("Discovered.Address = %q, want %q", got, testAlias+".echo")
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
	h, mods, cleanup := setupHostAndDiscovered(t)
	defer cleanup()
	d := mods["echo"]

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
	h, mods, cleanup := setupHostAndDiscovered(t)
	defer cleanup()
	d := mods["echo"]

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
	h, mods, cleanup := setupHostAndDiscovered(t)
	defer cleanup()
	d := mods["echo"]

	// The bundle declares network_outbound for `echo` and vault_access for
	// `reverse`. Allowing only network_outbound must let `echo` through and stop
	// `reverse` — the check is per module, and a sibling's declaration neither
	// widens nor narrows it.
	h.AllowedCapabilities = map[pluginv1.Capability]struct{}{
		pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND: {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.Spawn(ctx, mods["reverse"]); err == nil {
		t.Fatal("expected denial for vault_access not in allowed-list")
	}
	p, err := h.Spawn(ctx, d)
	if err != nil {
		t.Fatalf("echo declares only network_outbound and must spawn: %v", err)
	}
	_ = p.Close()
}

// GUARD: dispatch reaches the module the host named, not "the only one" and not "the
// first one". Both modules answer the same input differently, so the output says which
// implementation actually ran — the strongest available statement that argv decided it.
func TestSpawnDispatchesToTheNamedModule(t *testing.T) {
	h, mods, cleanup := setupHostAndDiscovered(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for module, want := range map[string]string{"echo": "world", "reverse": "dlrow"} {
		p, err := h.Spawn(ctx, mods[module])
		if err != nil {
			t.Fatalf("Spawn(%s): %v", module, err)
		}
		params, _ := structpb.NewStruct(map[string]any{"name": "world"})
		stream, err := p.Apply(ctx, &pluginv1.ApplyRequest{State: "applied", Params: params})
		if err != nil {
			_ = p.Close()
			t.Fatalf("Apply(%s): %v", module, err)
		}
		var got string
		for {
			ev, rerr := stream.Recv()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				_ = p.Close()
				t.Fatalf("apply recv(%s): %v", module, rerr)
			}
			if v, ok := ev.GetOutput().GetFields()["echo"]; ok {
				got = v.GetStringValue()
			}
		}
		_ = p.Close()
		if got != want {
			t.Errorf("module %q produced %q, want %q - the wrong implementation ran", module, got, want)
		}
	}
}

// TestSpawnParallel verifies multiple concurrent Spawns work correctly
// (distinct sockets, no name collisions).
func TestSpawnParallel(t *testing.T) {
	h, mods, cleanup := setupHostAndDiscovered(t)
	defer cleanup()
	d := mods["echo"]

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
