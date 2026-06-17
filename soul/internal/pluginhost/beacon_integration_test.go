package pluginhost

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"google.golang.org/protobuf/types/known/structpb"
)

// buildBeaconEchoPlugin собирает test-плагин testdata/beacon-plugin и кладёт
// его в outDir как `soul-beacon-echo`. Возвращает абсолютный путь к бинарю.
// Параллель buildEchoPlugin (integration_test.go) — те же inputs, тот же
// GOWORK=off-режим для отдельного go.mod-подмодуля testdata/.
func buildBeaconEchoPlugin(t *testing.T, outDir string) string {
	t.Helper()
	srcDir, err := filepath.Abs("testdata/beacon-plugin")
	if err != nil {
		t.Fatalf("abs testdata: %v", err)
	}
	binPath := filepath.Join(outDir, "soul-beacon-echo")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build beacon plugin: %v\n%s", err, out)
	}
	return binPath
}

// setupBeaconHostAndDiscovered — параллель setupHostAndDiscovered: поднимает
// modulesRoot c одним kind=soul_beacon плагином (testdata/beacon-plugin),
// собирает Sigil-trust для него и возвращает готовый Host + Discovered.
func setupBeaconHostAndDiscovered(t *testing.T) (*Host, Discovered, func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin host requires Unix sockets")
	}
	modulesRoot := shortHostDir(t, "ss-bmods-")
	socketDir := shortHostDir(t, "ss-bsock-")
	moduleDir := filepath.Join(modulesRoot, "wb-echo")
	if err := os.Mkdir(moduleDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binPath := buildBeaconEchoPlugin(t, moduleDir)
	// manifest.name=echo → BinaryName=soul-beacon-echo (конвенция Manifest.BinaryName).
	if err := os.WriteFile(filepath.Join(moduleDir, "manifest.yaml"), []byte(`kind: soul_beacon
protocol_version: 1
namespace: wb
name: echo
required_capabilities: []
side_effects: []
spec:
  params_schema:
    type: object
    required: [topic]
    properties:
      topic: { type: string }
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
	if found[0].Manifest.Kind != KindSoulBeacon {
		t.Fatalf("discovered kind = %q, want soul_beacon", found[0].Manifest.Kind)
	}

	pub, sigils := sigilFor(t, found[0])
	h := &Host{Host: &sharedhost.Host{
		SocketDir:      socketDir,
		StartupTimeout: 10 * time.Second,
		ShutdownGrace:  3 * time.Second,
		SigilAnchors:   sharedhost.NewAnchorSet([]ed25519.PublicKey{pub}),
		Sigils:         sigils,
	}}
	_ = binPath
	return h, found[0], func() {}
}

// TestSpawnBeaconHappyPath — full L1 roundtrip: Discover → Spawn (с реальным
// Sigil-verify) → Validate(ok) → Check(state=alerted, payload, state_cookie) →
// Close. Покрывает SDK contract в реальном subprocess-gRPC окружении.
func TestSpawnBeaconHappyPath(t *testing.T) {
	h, d, cleanup := setupBeaconHostAndDiscovered(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := h.SpawnBeacon(ctx, d)
	if err != nil {
		t.Fatalf("SpawnBeacon: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	}()

	if p.Manifest().Address() != "wb.echo" {
		t.Errorf("Manifest.Address = %q", p.Manifest().Address())
	}

	params, _ := structpb.NewStruct(map[string]any{"topic": "filesystem"})
	vr, err := p.Validate(ctx, &pluginv1.ValidateVigilRequest{Params: params})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !vr.GetOk() {
		t.Errorf("Validate.Ok = false, errors=%v", vr.GetErrors())
	}

	cr, err := p.Check(ctx, &pluginv1.CheckRequest{Params: params})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if cr.GetState() != "alerted" {
		t.Errorf("Check state = %q, want alerted", cr.GetState())
	}
	if got := cr.GetPayload().GetFields()["topic"].GetStringValue(); got != "filesystem" {
		t.Errorf("Check payload.topic = %q, want filesystem", got)
	}
	if string(cr.GetStateCookie()) != "echo-cookie" {
		t.Errorf("Check state_cookie = %q, want echo-cookie", string(cr.GetStateCookie()))
	}
}

// TestSpawnBeaconValidationFailure — Validate без topic возвращает Ok=false с
// ошибкой; Spawn проходит, RPC возвращает структурный негатив.
func TestSpawnBeaconValidationFailure(t *testing.T) {
	h, d, cleanup := setupBeaconHostAndDiscovered(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := h.SpawnBeacon(ctx, d)
	if err != nil {
		t.Fatalf("SpawnBeacon: %v", err)
	}
	defer p.Close()

	vr, err := p.Validate(ctx, &pluginv1.ValidateVigilRequest{})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if vr.GetOk() {
		t.Errorf("expected Ok=false (missing topic), got Ok=true")
	}
}

// TestSpawnBeaconRejectsKindMismatch — попытка SpawnBeacon на manifest kind=
// soul_module даёт ошибку до exec (защита от kind-cross на Soul-host-е).
func TestSpawnBeaconRejectsKindMismatch(t *testing.T) {
	h, modD, cleanup := setupHostAndDiscovered(t) // soul_module-плагин из echo-теста
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.SpawnBeacon(ctx, modD); err == nil {
		t.Fatal("expected kind-mismatch denial for soul_module under SpawnBeacon")
	}
}

// TestSpawnBeaconDigestMismatchRejected — security-ветка: integrity-gate
// блокирует beacon-плагин с подменённым бинарём. Параллель
// TestSpawnDigestMismatchRejected для soul_module — Sigil-verify не зависит
// от kind, общая логика shared/pluginhost.
func TestSpawnBeaconDigestMismatchRejected(t *testing.T) {
	h, d, cleanup := setupBeaconHostAndDiscovered(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Первый Spawn — verify+seal проходят (валидный допуск).
	p, err := h.SpawnBeacon(ctx, d)
	if err != nil {
		t.Fatalf("первый SpawnBeacon: %v", err)
	}
	_ = p.Close()

	// Подмена бинаря: digest перестаёт совпадать.
	tamperBinary(t, d.BinaryPath)

	// Второй Spawn должен отказать.
	_, err = h.SpawnBeacon(ctx, d)
	if err == nil {
		t.Fatal("SECURITY: SpawnBeacon запустил подменённый бинарь")
	}
	if !errors.Is(err, sharedhost.ErrSigilVerify) {
		t.Fatalf("ожидался ErrSigilVerify, got %v", err)
	}
	var ve *sharedhost.VerifyError
	if !errors.As(err, &ve) || ve.Reason != sharedhost.VerifyReasonDigestMismatch {
		t.Errorf("ожидался reason digest_mismatch, got %v", err)
	}
}
