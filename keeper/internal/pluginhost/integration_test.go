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

// sigilFor подписывает валидный SigilRecord под бинарь+manifest из Discovered
// тем же общим helper-ом, что keeper-Signer при Sign (BuildSigilBlock +
// NormalizeManifestBytes — симметрия sign↔verify). Возвращает trust-anchor и
// lookup с единственным допуском, готовые к навешиванию на Host. После S6b
// verify-gate Spawn fail-closed без валидного допуска — без этого ни один
// happy-path-тест не пройдёт.
func sigilFor(t *testing.T, d Discovered) (ed25519.PublicKey, sharedhost.SigilLookup) {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join(d.Dir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest for sigil: %v", err)
	}
	binBytes, err := os.ReadFile(d.BinaryPath)
	if err != nil {
		t.Fatalf("read binary for sigil: %v", err)
	}
	binSum := sha256.Sum256(binBytes)
	binHex := hex.EncodeToString(binSum[:])
	binRaw, _ := hex.DecodeString(binHex)

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	manDigest := sha256.Sum256(sharedhost.NormalizeManifestBytes(manifest))
	const ref = "v1.0.0"
	block := sharedhost.BuildSigilBlock(d.Manifest.Namespace, d.Manifest.Name, ref, binRaw, manDigest[:])
	rec := &sharedhost.SigilRecord{
		Namespace:       d.Manifest.Namespace,
		Name:            d.Manifest.Name,
		Ref:             ref,
		BinarySHA256hex: binHex,
		Signature:       ed25519.Sign(priv, block),
		Manifest:        manifest,
	}
	return pub, testLookup{d.Manifest.Namespace + "." + d.Manifest.Name: rec}
}

// testLookup — минимальный sharedhost.SigilLookup поверх map.
type testLookup map[string]*sharedhost.SigilRecord

func (l testLookup) Get(ns, name string) *sharedhost.SigilRecord { return l[ns+"."+name] }

// buildTestPlugin собирает плагин из testdata/<dir> и кладёт его в outDir с
// именем outName. testdata-модуль имеет отдельный go.mod (replace на наши
// proto/plugin и sdk), поэтому собираем с GOWORK=off — он не должен быть
// частью корневого workspace.
//
// На darwin длина sun_path Unix-сокета ограничена ~104 байтами, поэтому
// outDir должен быть коротким (используем /tmp/ss-keeper-host-, не t.TempDir).
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

// makeNestedSlot создаёт R-nested-слот (A1-S1): <cacheRoot>/<key>/<commit>/ +
// current → <commit>. Возвращает каталог commit-слота, куда тест кладёт
// manifest+бинарь. commit — синтетический фиксированный 40-hex.
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

func setupCloudDriverPlugin(t *testing.T) (*Host, Discovered) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin host requires Unix sockets")
	}
	cacheRoot := shortHostDir(t, "ss-kpr-mods-")
	socketDir := shortHostDir(t, "ss-kpr-sock-")
	moduleDir := makeNestedSlot(t, cacheRoot, "soulstack-fake")
	buildTestPlugin(t, "cloud-plugin", moduleDir, "soul-cloud-fake")
	if err := os.WriteFile(filepath.Join(moduleDir, "manifest.yaml"), []byte(`kind: cloud_driver
protocol_version: 1
namespace: soulstack
name: fake
required_capabilities: []
side_effects: []
spec:
  provider_kind: fake
  profile_schema:
    type: object
    properties:
      region: { type: string }
`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	found, warns, err := Discover(cacheRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warns) != 0 {
		t.Logf("discovery warnings: %v", warns)
	}
	if len(found) != 1 {
		t.Fatalf("expected 1 discovered plugin, got %d", len(found))
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

func setupSshProviderPlugin(t *testing.T) (*Host, Discovered) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin host requires Unix sockets")
	}
	cacheRoot := shortHostDir(t, "ss-kpr-mods-")
	socketDir := shortHostDir(t, "ss-kpr-sock-")
	moduleDir := makeNestedSlot(t, cacheRoot, "soulstack-fake")
	buildTestPlugin(t, "ssh-plugin", moduleDir, "soul-ssh-fake")
	if err := os.WriteFile(filepath.Join(moduleDir, "manifest.yaml"), []byte(`kind: ssh_provider
protocol_version: 1
namespace: soulstack
name: fake
required_capabilities: []
side_effects: []
spec:
  provider_kind: static_key
`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	found, warns, err := Discover(cacheRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warns) != 0 {
		t.Logf("discovery warnings: %v", warns)
	}
	if len(found) != 1 {
		t.Fatalf("expected 1 discovered plugin, got %d", len(found))
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

func TestSpawnCloudDriverHappyPath(t *testing.T) {
	h, d := setupCloudDriverPlugin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := h.Spawn(ctx, d)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	cd, err := NewCloudDriverPlugin(p)
	if err != nil {
		t.Fatalf("NewCloudDriverPlugin: %v", err)
	}
	defer func() {
		if err := cd.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	}()

	if cd.Manifest().Address() != "soulstack.fake" {
		t.Errorf("Manifest.Address = %q", cd.Manifest().Address())
	}

	// Schema.
	sr, err := cd.Schema(ctx, &pluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if sr.GetProfileSchema() == nil {
		t.Errorf("Schema reply has nil ProfileSchema")
	}

	// Validate.
	profile, _ := structpb.NewStruct(map[string]any{"region": "us-east-1"})
	vr, err := cd.Validate(ctx, &pluginv1.ValidateProfileRequest{Profile: profile})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !vr.GetOk() {
		t.Errorf("Validate.Ok = false: %v", vr.GetErrors())
	}

	// Create stream — три события (две диагностики + финал с vms[]).
	createStream, err := cd.Create(ctx, &pluginv1.CreateRequest{Profile: profile})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var (
		events   int
		finalVms int
	)
	for {
		ev, err := createStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("create recv: %v", err)
		}
		events++
		if len(ev.GetVms()) > 0 {
			finalVms += len(ev.GetVms())
		}
	}
	if events != 3 {
		t.Errorf("create events = %d, want 3", events)
	}
	if finalVms != 1 {
		t.Errorf("final vms = %d, want 1", finalVms)
	}

	// Status — точечный запрос.
	st, err := cd.Status(ctx, &pluginv1.StatusRequest{VmId: "vm-1"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.GetState() != "running" {
		t.Errorf("Status.State = %q, want running", st.GetState())
	}

	// List stream — два VmInfo.
	listStream, err := cd.List(ctx, &pluginv1.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var listVms []string
	for {
		vm, err := listStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("list recv: %v", err)
		}
		listVms = append(listVms, vm.GetVmId())
	}
	if len(listVms) != 2 {
		t.Errorf("list vms = %v, want 2", listVms)
	}

	// Destroy.
	destroyStream, err := cd.Destroy(ctx, &pluginv1.DestroyRequest{VmIds: []string{"vm-x"}})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	var destroyMsgs int
	for {
		_, err := destroyStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("destroy recv: %v", err)
		}
		destroyMsgs++
	}
	if destroyMsgs != 1 {
		t.Errorf("destroy events = %d, want 1", destroyMsgs)
	}
}

func TestSpawnCloudDriverValidationFailure(t *testing.T) {
	h, d := setupCloudDriverPlugin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := h.Spawn(ctx, d)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	cd, err := NewCloudDriverPlugin(p)
	if err != nil {
		t.Fatalf("NewCloudDriverPlugin: %v", err)
	}
	defer cd.Close()

	// Без profile — Validate должен вернуть Ok=false.
	vr, err := cd.Validate(ctx, &pluginv1.ValidateProfileRequest{})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if vr.GetOk() {
		t.Errorf("expected Ok=false for empty profile")
	}
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

	if sp.Manifest().Kind != KindSSHProvider {
		t.Errorf("Manifest.Kind = %q", sp.Manifest().Kind)
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
	h, d := setupCloudDriverPlugin(t)
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
	h, d := setupCloudDriverPlugin(t)
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

// TestSpawnFailsClosedNoSigil — keeper-host без допуска для (ns, name) →
// Spawn fail-closed (VerifyReasonNoSigil), бинарь не запускается.
func TestSpawnFailsClosedNoSigil(t *testing.T) {
	h, d := setupCloudDriverPlugin(t)
	// Подменяем lookup на пустой (trust-anchor остаётся валидным): допуска нет.
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

// TestSpawnFailsClosedNoTrustAnchor — Sigil выключен на keeper (пустой набор
// якорей) → Spawn fail-closed (VerifyReasonNoTrustAnchor). Интенсионально:
// оператор с cloud/ssh обязан настроить Sigil (G-sigil-5).
func TestSpawnFailsClosedNoTrustAnchor(t *testing.T) {
	h, d := setupCloudDriverPlugin(t)
	// Допуск есть (lookup из setup), но набор trust-anchor-ов пуст.
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

func TestNewCloudDriverPluginRejectsWrongKind(t *testing.T) {
	// Подсовываем «фейк» Plugin с manifest kind=ssh_provider в Cloud-обёртку.
	p := &Plugin{BasePlugin: sharedhost.NewBasePluginForTest(
		&Manifest{Kind: KindSSHProvider, Namespace: "x", Name: "y"},
	)}
	if _, err := NewCloudDriverPlugin(p); err == nil {
		t.Fatal("expected error when wrapping ssh_provider Plugin as CloudDriverPlugin")
	}
}

func TestNewSshProviderPluginRejectsWrongKind(t *testing.T) {
	p := &Plugin{BasePlugin: sharedhost.NewBasePluginForTest(
		&Manifest{Kind: KindCloudDriver, Namespace: "x", Name: "y"},
	)}
	if _, err := NewSshProviderPlugin(p); err == nil {
		t.Fatal("expected error when wrapping cloud_driver Plugin as SshProviderPlugin")
	}
}

// TestSpawnParallel — несколько Spawn-ов параллельно работают корректно
// (разные сокеты, нет коллизий имён).
func TestSpawnParallel(t *testing.T) {
	h, d := setupCloudDriverPlugin(t)

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
			cd, err := NewCloudDriverPlugin(p)
			if err != nil {
				errs[i] = err
				return
			}
			if _, err := cd.Schema(ctx, &pluginv1.SchemaRequest{}); err != nil {
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
