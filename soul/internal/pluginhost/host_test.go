package pluginhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// TestNewHostNilConfig — nil-конфиг даёт host с дефолтами ADR-020(d) и
// Soul-host-овским DefaultSocketDir (отличается от Keeper-host).
func TestNewHostNilConfig(t *testing.T) {
	h, err := NewHost(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewHost(nil): %v", err)
	}
	if h.SocketDir != DefaultSocketDir {
		t.Errorf("SocketDir = %q, want %q", h.SocketDir, DefaultSocketDir)
	}
	if h.StartupTimeout != DefaultStartupTimeout {
		t.Errorf("StartupTimeout = %v, want %v", h.StartupTimeout, DefaultStartupTimeout)
	}
	if h.ShutdownGrace != DefaultShutdownGrace {
		t.Errorf("ShutdownGrace = %v, want %v", h.ShutdownGrace, DefaultShutdownGrace)
	}
	if h.AllowedCapabilities != nil {
		t.Errorf("AllowedCapabilities = %v, want nil (всё разрешено)", h.AllowedCapabilities)
	}
}

// TestNewHostEmptyConfig — пустой (не nil) конфиг тоже падает на дефолтный
// SocketDir, потому что cfg.SocketDir == "".
func TestNewHostEmptyConfig(t *testing.T) {
	h, err := NewHost(&config.PluginRuntime{}, nil, nil)
	if err != nil {
		t.Fatalf("NewHost(empty): %v", err)
	}
	if h.SocketDir != DefaultSocketDir {
		t.Errorf("SocketDir = %q, want %q (cfg.SocketDir пуст → дефолт)", h.SocketDir, DefaultSocketDir)
	}
	if h.StartupTimeout != DefaultStartupTimeout || h.ShutdownGrace != DefaultShutdownGrace {
		t.Errorf("timeouts = (%v,%v), want defaults", h.StartupTimeout, h.ShutdownGrace)
	}
}

// TestNewHostFullConfig — все поля заданы и переопределяют дефолты, capabilities
// конвертируются в типизированный set.
func TestNewHostFullConfig(t *testing.T) {
	cfg := &config.PluginRuntime{
		SocketDir:           "/tmp/custom-sock",
		StartupTimeout:      "5s",
		ShutdownGrace:       "2s",
		AllowedCapabilities: []string{"network_outbound", "vault_access"},
	}
	h, err := NewHost(cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewHost(full): %v", err)
	}
	if h.SocketDir != "/tmp/custom-sock" {
		t.Errorf("SocketDir = %q, want override", h.SocketDir)
	}
	if h.StartupTimeout != 5*time.Second {
		t.Errorf("StartupTimeout = %v, want 5s", h.StartupTimeout)
	}
	if h.ShutdownGrace != 2*time.Second {
		t.Errorf("ShutdownGrace = %v, want 2s", h.ShutdownGrace)
	}
	want := map[pluginv1.Capability]struct{}{
		pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND: {},
		pluginv1.Capability_CAPABILITY_VAULT_ACCESS:     {},
	}
	if len(h.AllowedCapabilities) != len(want) {
		t.Fatalf("AllowedCapabilities len = %d, want %d", len(h.AllowedCapabilities), len(want))
	}
	for c := range want {
		if _, ok := h.AllowedCapabilities[c]; !ok {
			t.Errorf("AllowedCapabilities missing %v", c)
		}
	}
}

// TestNewHostBadStartupTimeout — невалидный startup_timeout → ошибка
// конструктора (defense-in-depth повторная валидация duration).
func TestNewHostBadStartupTimeout(t *testing.T) {
	_, err := NewHost(&config.PluginRuntime{StartupTimeout: "not-a-duration"}, nil, nil)
	if err == nil {
		t.Fatal("expected error for bad startup_timeout")
	}
}

// TestNewHostBadShutdownGrace — невалидный shutdown_grace → ошибка.
func TestNewHostBadShutdownGrace(t *testing.T) {
	_, err := NewHost(&config.PluginRuntime{ShutdownGrace: "12 parsecs"}, nil, nil)
	if err == nil {
		t.Fatal("expected error for bad shutdown_grace")
	}
}

// TestNewHostUnknownCapability — неизвестная capability в allowed-списке →
// ошибка (закрытый enum, host не должен молча игнорировать).
func TestNewHostUnknownCapability(t *testing.T) {
	_, err := NewHost(&config.PluginRuntime{AllowedCapabilities: []string{"summon_demons"}}, nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown capability")
	}
}

// TestSpawnRejectsKindMismatch — Spawn отвергает Discovered с manifest.kind !=
// soul_module ещё до exec (защита от drift Discover-фильтра / ручной
// конструкции Discovered). Покрывает errKindMismatch.
func TestSpawnRejectsKindMismatch(t *testing.T) {
	h, err := NewHost(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	d := Discovered{
		Manifest: &sharedplugin.Manifest{
			Kind:      KindCloudDriver,
			Namespace: "wb",
			Name:      "aws",
		},
		BinaryPath: "/nonexistent/soul-cloud-aws",
		Dir:        "/nonexistent",
	}
	_, err = h.Spawn(context.Background(), d)
	if err == nil {
		t.Fatal("expected kind-mismatch denial for cloud_driver under soul-host")
	}
	if !contains(err.Error(), "soul_module") || !contains(err.Error(), KindCloudDriver) {
		t.Errorf("error %q should mention expected kind soul_module и фактический %q", err.Error(), KindCloudDriver)
	}
}

// TestDiscoverMissingRoot — несуществующий modulesRoot → fatal error от
// sharedhost.Discover (ENOENT на ReadDir), а не пустой список.
func TestDiscoverMissingRoot(t *testing.T) {
	_, _, err := Discover("/nonexistent/soul-stack/modules/root")
	if err == nil {
		t.Fatal("expected error for nonexistent modules root")
	}
}

// TestDiscoverFiltersNonSoulKind — Discover на каталоге с одним cloud_driver и
// одним soul_module возвращает только soul_module; cloud-плагин уходит в
// warnings (FilterByKinds-ветка Soul-host-а).
func TestDiscoverFiltersNonSoulKind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin host requires Unix sockets")
	}
	root := shortHostDir(t, "ss-mixedmods-")

	// soul_module-плагин (echo) — должен пройти.
	soulDir := filepath.Join(root, "wb-echo")
	if err := os.Mkdir(soulDir, 0o755); err != nil {
		t.Fatalf("mkdir soul: %v", err)
	}
	buildEchoPlugin(t, soulDir)
	if err := os.WriteFile(filepath.Join(soulDir, "manifest.yaml"), []byte(`kind: soul_module
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
`), 0o644); err != nil {
		t.Fatalf("write soul manifest: %v", err)
	}

	// cloud_driver-плагин — должен быть отфильтрован в warnings. Бинарь —
	// копия echo под cloud-именем, чтобы Discover нашёл исполняемый файл.
	cloudDir := filepath.Join(root, "wb-aws")
	if err := os.Mkdir(cloudDir, 0o755); err != nil {
		t.Fatalf("mkdir cloud: %v", err)
	}
	echoBin := buildEchoPlugin(t, cloudDir) // soul-mod-echo
	if err := os.Rename(echoBin, filepath.Join(cloudDir, "soul-cloud-aws")); err != nil {
		t.Fatalf("rename cloud bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloudDir, "manifest.yaml"), []byte(`kind: cloud_driver
protocol_version: 1
namespace: wb
name: aws
required_capabilities: []
side_effects: []
`), 0o644); err != nil {
		t.Fatalf("write cloud manifest: %v", err)
	}

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("expected 1 soul_module discovered, got %d: %+v", len(found), found)
	}
	if found[0].Manifest.Kind != KindSoulModule {
		t.Errorf("discovered kind = %q, want soul_module", found[0].Manifest.Kind)
	}
	var sawCloudWarn bool
	for _, w := range warns {
		if contains(w, "wb-aws") {
			sawCloudWarn = true
		}
	}
	if !sawCloudWarn {
		t.Errorf("ожидался warning про отфильтрованный cloud_driver, warns=%v", warns)
	}
}

// TestSpawnBinaryNotFound — Discovered указывает на несуществующий бинарь;
// Spawn падает (cmd.Start / integrity-gate) и не возвращает Plugin.
// End-to-end error-путь Soul-обёртки через shared.Spawn.
func TestSpawnBinaryNotFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plugin host requires Unix sockets")
	}
	socketDir := shortHostDir(t, "ss-sock-nf-")
	h := &Host{Host: &sharedhost.Host{
		SocketDir:      socketDir,
		StartupTimeout: 3 * time.Second,
		ShutdownGrace:  1 * time.Second,
	}}
	d := Discovered{
		Manifest:   &sharedplugin.Manifest{Kind: KindSoulModule, Namespace: "wb", Name: "ghost"},
		BinaryPath: filepath.Join(socketDir, "does-not-exist"),
		Dir:        socketDir,
	}
	if _, err := h.Spawn(context.Background(), d); err == nil {
		t.Fatal("expected error spawning nonexistent binary")
	}
}

// TestSpawnDigestMismatchRejected — security-ветка integrity-gate end-to-end
// (ADR-026, S6b):
//   - первый Spawn: Sigil-verify проходит (валидный допуск), sidecar засилен;
//   - бинарь плагина подменяется (имитация tamper в кеше host-а);
//   - второй Spawn обязан отвергнуть запуск fail-closed: фактический digest
//     бинаря больше не совпадает с допущенным хешем в Sigil → digest_mismatch.
//
// Это центральный security-инвариант: подмена бинаря не приводит к exec.
func TestSpawnDigestMismatchRejected(t *testing.T) {
	h, d, cleanup := setupHostAndDiscovered(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Первый запуск — Sigil-verify проходит, печать sidecar, нормальная работа.
	p, err := h.Spawn(ctx, d)
	if err != nil {
		t.Fatalf("первый Spawn (verify+seal): %v", err)
	}
	if err := p.Close(); err != nil {
		t.Logf("Close после seal: %v", err)
	}

	sidecar := filepath.Join(d.Dir, ".sha256")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar .sha256 не создан первым Spawn: %v", err)
	}

	// Подмена бинаря: дописываем мусор → digest перестаёт совпадать с допуском.
	tamperBinary(t, d.BinaryPath)

	// Второй Spawn обязан отказать до exec — fail-closed по digest_mismatch.
	_, err = h.Spawn(ctx, d)
	if err == nil {
		t.Fatal("SECURITY: Spawn запустил подменённый бинарь (digest mismatch не отвергнут)")
	}
	if !errors.Is(err, sharedhost.ErrSigilVerify) {
		t.Fatalf("ожидался ErrSigilVerify, got %v", err)
	}
	var ve *sharedhost.VerifyError
	if !errors.As(err, &ve) || ve.Reason != sharedhost.VerifyReasonDigestMismatch {
		t.Errorf("ожидался reason digest_mismatch, got %v", err)
	}
}

// tamperBinary меняет содержимое бинаря плагина так, чтобы фактический digest
// перестал совпадать с допущенным в Sigil (имитация tamper в кеше host-а).
//
// Подмена атомарная: новый контент пишется во временный файл рядом и
// os.Rename'ом подставляется поверх старого пути. Прямое открытие
// исполняемого файла на запись (O_WRONLY) на Linux даёт ETXTBSY, пока
// предыдущий plugin-процесс ещё держит inode на exec — rename же создаёт
// новый inode и не конфликтует с исполняемым старым (кросс-платформенно:
// на macOS ETXTBSY-ограничения нет, rename корректен и там).
func tamperBinary(t *testing.T, path string) {
	t.Helper()
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read бинаря перед подменой: %v", err)
	}
	tampered := append(orig, []byte("\n// tampered\n")...)

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tamper-*")
	if err != nil {
		t.Fatalf("create temp для подмены: %v", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(tampered); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		t.Fatalf("write подмены: %v", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		t.Fatalf("close temp подмены: %v", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		t.Fatalf("chmod temp подмены: %v", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		t.Fatalf("rename подмены поверх бинаря: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
