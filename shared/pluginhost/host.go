// Package pluginhost — общий runtime для host-ов плагинов Soul Stack
// (ADR-020, docs/keeper/plugins.md).
//
// Пакет реализует kind-agnostic часть жизненного цикла субпроцесса плагина:
// fork → handshake → dial gRPC → graceful shutdown. Kind-specific обёртки
// (SoulModule / CloudDriver / SshProvider) живут в host-side пакетах
// (`soul/internal/pluginhost`, `keeper/internal/pluginhost`) и строятся
// поверх [BasePlugin].
//
// Парсинг manifest.yaml — в [shared/plugin]; этот пакет принимает уже
// провалидированный manifest через [Discovered].
package pluginhost

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Host — общий runtime для запуска любого kind-а плагина. Безопасен для
// конкурентных вызовов [Host.Spawn].
//
// Конфиг приходит из `<binary>.yml::plugin_runtime` (shared/config.PluginRuntime).
// При nil-конфиге применяются defaults ADR-020(d), за исключением SocketDir —
// он kind-host-specific и должен задаваться через [NewHost] параметром
// defaultSocketDir.
type Host struct {
	// SocketDir — где host создаёт Unix-сокеты для плагинов. Mode 0700,
	// owned by service user. Создаётся mkdir-ом при первом Spawn (idempotent).
	SocketDir string
	// StartupTimeout — окно от exec до появления handshake-строки.
	StartupTimeout time.Duration
	// ShutdownGrace — окно от SIGTERM до SIGKILL.
	ShutdownGrace time.Duration
	// AllowedCapabilities — closed-set capabilities, разрешённых на этом
	// host-е. Содержит pluginv1.Capability-значения для быстрого сравнения.
	// nil/empty = «все capabilities разрешены» (default ADR-020(d)).
	AllowedCapabilities map[pluginv1.Capability]struct{}
	// SigilAnchors — НАБОР trust-anchor-ов verify Sigil (ADR-026(h), R3
	// multi-anchor): атомарно-сменяемый holder публичных ed25519-ключей подписи
	// допусков плагинов, доехавших в bootstrap. verify проходит, если подпись
	// подтвердил ЛЮБОЙ якорь из набора (OR — безразрывная ротация ключа). Пустой
	// набор (или nil-holder) = Sigil не настроен на Keeper → verify любого
	// custom-плагина fail-closed (reason no_trust_anchor). Holder читается
	// snapshot-ом в [Host.Spawn]; S6 заменит набор в рантайме через
	// [AnchorSet.SetAnchors] без перезапуска. Core-модули статические,
	// Spawn-verify не проходят — этим полем не затронуты.
	SigilAnchors *AnchorSet
	// Sigils — поверхность чтения активных допусков Sigil по (ns, name) для
	// fail-closed verify в [Host.Spawn]. nil = допусков нет → verify
	// fail-closed (reason no_sigil). DI: Soul-side адаптер оборачивает
	// runtime-кеш Sigil-ов, shared НЕ тянет keeper-proto.
	Sigils SigilLookup
}

// Defaults ADR-020(d). DefaultSocketDir намеренно НЕ экспортируется: каждый
// host (soul / keeper) задаёт свой через [NewHost], чтобы под разными
// service-user-ами не было коллизий путей.
const (
	DefaultStartupTimeout = 10 * time.Second
	DefaultShutdownGrace  = 10 * time.Second
)

// sockCounter — монотонный счётчик для уникальности имени Unix-сокета при
// конкурентных Spawn (UnixNano на одной machine может совпасть для горутин
// внутри одной ms, особенно под race-детектором).
var sockCounter atomic.Uint64

// NewHost конструирует Host из shared-config-блока. Применяет defaults
// ADR-020(d) при отсутствующих полях. duration-строки уже провалидированы в
// semantic-фазе shared/config; здесь повторный ParseDuration используется
// как defense-in-depth (host инициируется только с валидным конфигом).
//
// defaultSocketDir — kind-host-specific дефолт (например,
// `/var/run/soul-stack/plugins` для soul и `/var/run/soul-stack-keeper/plugins`
// для keeper); используется, если cfg.SocketDir пуст.
func NewHost(cfg *config.PluginRuntime, defaultSocketDir string) (*Host, error) {
	h := &Host{
		SocketDir:      defaultSocketDir,
		StartupTimeout: DefaultStartupTimeout,
		ShutdownGrace:  DefaultShutdownGrace,
	}
	if cfg == nil {
		return h, nil
	}
	if cfg.SocketDir != "" {
		h.SocketDir = cfg.SocketDir
	}
	if cfg.StartupTimeout != "" {
		d, err := parseDuration(cfg.StartupTimeout)
		if err != nil {
			return nil, fmt.Errorf("pluginhost: plugin_runtime.startup_timeout: %w", err)
		}
		h.StartupTimeout = d
	}
	if cfg.ShutdownGrace != "" {
		d, err := parseDuration(cfg.ShutdownGrace)
		if err != nil {
			return nil, fmt.Errorf("pluginhost: plugin_runtime.shutdown_grace: %w", err)
		}
		h.ShutdownGrace = d
	}
	if len(cfg.AllowedCapabilities) > 0 {
		h.AllowedCapabilities = make(map[pluginv1.Capability]struct{}, len(cfg.AllowedCapabilities))
		for _, s := range cfg.AllowedCapabilities {
			c, ok := sharedplugin.CapabilityFromString(s)
			if !ok {
				return nil, fmt.Errorf("pluginhost: plugin_runtime.allowed_capabilities: unknown %q", s)
			}
			h.AllowedCapabilities[c] = struct{}{}
		}
	}
	return h, nil
}

// CheckCapabilities сверяет required_capabilities плагина с allowed-списком
// host-а. Возвращает error при первой не-разрешённой capability. nil
// AllowedCapabilities = всё разрешено (default).
func (h *Host) CheckCapabilities(m *sharedplugin.Manifest) error {
	if h.AllowedCapabilities == nil {
		return nil
	}
	for _, s := range m.RequiredCapabilities {
		c, ok := sharedplugin.CapabilityFromString(s)
		if !ok {
			// Не должно случиться — manifest уже валидировался; но host
			// проверяет на всякий случай, чтобы не молча игнорировать.
			return fmt.Errorf("pluginhost: unknown capability %q in %s", s, m.Address())
		}
		if _, allowed := h.AllowedCapabilities[c]; !allowed {
			return fmt.Errorf("pluginhost: capability %q is not allowed by host (manifest %s)", s, m.Address())
		}
	}
	return nil
}

// SpawnOption — functional-опция [Host.Spawn]: caller прокидывает
// дополнительные настройки spawn-цикла, не расширяя сигнатуру метода и не
// мутируя [Discovered] (тот — результат discovery, общий для всех spawn-ов).
//
// Пилотный пример (ADR-020 amendment l, 2026-05-26) — env-payload provider-
// специфичных params для SshProvider-плагинов через [WithEnv]: caller
// сериализует params в JSON и кладёт в env-переменную с зафиксированным
// именем `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS`, плагин читает её на старте.
type SpawnOption func(*spawnOpts)

type spawnOpts struct {
	extraEnv []string
}

// WithEnv добавляет к окружению spawned-плагина дополнительные пары
// `KEY=VALUE`. Применяется поверх [os.Environ] и [handshake.SocketEnv]; при
// коллизии имени выигрывает запись из extraEnv (последняя в `cmd.Env` имеет
// приоритет по семантике exec). Используется в env-convention-режиме
// доставки params плагина (ADR-020 amendment l).
func WithEnv(env []string) SpawnOption {
	return func(o *spawnOpts) { o.extraEnv = append(o.extraEnv, env...) }
}

// Spawn — fork plugin binary, чтение handshake, dial gRPC. Возвращает
// [BasePlugin], готовый принимать RPC через [BasePlugin.Conn]. При любой
// ошибке (timeout, handshake-drift, dial-fail) процесс плагина останавливается
// и удаляется сокет — BasePlugin не возвращается.
//
// Контракт one-shot per Spawn (ADR-020(d)): caller вызывает Spawn перед
// каждой серией RPC и [BasePlugin.Close] после. Connection-pooling не
// предусмотрен — это упрощает изоляцию между задачами и устраняет proxy
// состояние между RPC.
//
// Kind-specific gRPC-client-stubs (SoulModuleClient / CloudDriverClient /
// SshProviderClient) создаются caller-ом из [BasePlugin.Conn] — этот
// пакет kind-agnostic.
//
// opts — опц. functional-настройки spawn-цикла (например, [WithEnv] для
// env-payload params плагина).
func (h *Host) Spawn(ctx context.Context, d Discovered, opts ...SpawnOption) (*BasePlugin, error) {
	var so spawnOpts
	for _, opt := range opts {
		opt(&so)
	}
	if err := h.CheckCapabilities(d.Manifest); err != nil {
		return nil, err
	}
	// Integrity-gate (ADR-026, S6b): fail-closed verify бинаря против печати
	// доверия Sigil ДО любого exec. Заменяет TOFU-ветку first-load: бинарь без
	// валидного допуска (no_sigil), без trust-anchor (no_trust_anchor), с
	// несовпадающим digest-ом (digest_mismatch) или битой подписью
	// (bad_signature) → *VerifyError, плагин НЕ запускается. re-exec-сверка по
	// sidecar остаётся внутри как defense-in-depth. См.
	// docs/keeper/plugins.md → Integrity-model.
	if err := verifySigilAndSeal(d.Dir, d.BinaryPath, d.Manifest.Namespace, d.Manifest.Name, h.SigilAnchors.snapshot(), h.Sigils); err != nil {
		return nil, fmt.Errorf("pluginhost: %s: %w", d.Manifest.Address(), err)
	}
	if err := os.MkdirAll(h.SocketDir, 0o700); err != nil {
		return nil, fmt.Errorf("pluginhost: mkdir socket dir %q: %w", h.SocketDir, err)
	}
	// Имя сокета пока без pid плагина — pid известен только после exec.
	// Используем pid host-процесса + atomic-counter для уникальности; формат
	// имени для логов важен, для протокола — нет (плагин читает путь из env).
	sockName := fmt.Sprintf("%s-%s-%d-%d.sock",
		d.Manifest.Namespace, d.Manifest.Name, os.Getpid(), sockCounter.Add(1))
	sockPath := filepath.Join(h.SocketDir, sockName)

	cmd := exec.CommandContext(ctx, d.BinaryPath)
	cmd.Env = append(os.Environ(), handshake.SocketEnv+"="+sockPath)
	// Доп. env от caller-а (ADR-020 amendment l, env-convention для SshProvider).
	// Дописываем ПОСЛЕ handshake-сокета: последняя запись в `cmd.Env` имеет
	// приоритет по семантике exec, поэтому caller теоретически может перебить
	// что угодно — это сознательная зона ответственности caller-а
	// (host не интроспектирует payload, только прокидывает).
	if len(so.extraEnv) > 0 {
		cmd.Env = append(cmd.Env, so.extraEnv...)
	}
	cmd.Dir = d.Dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pluginhost: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("pluginhost: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pluginhost: start plugin %s: %w", d.BinaryPath, err)
	}

	// stderr плагина — diagnostics-канал (ADR-020 → plugins.md → Handshake).
	// Тейлим последние 4KB в кольцевой буфер для отчёта при сбое.
	errTail := newTailBuffer(4 * 1024)
	go func() { _, _ = io.Copy(errTail, stderr) }()

	hs, err := readHandshake(stdout, h.StartupTimeout)
	if err != nil {
		_ = killAndWait(cmd, h.ShutdownGrace)
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("pluginhost: %s: %w (stderr-tail: %s)", d.Manifest.Address(), err, errTail.String())
	}

	if err := validateHandshake(d.Manifest, hs, sockPath); err != nil {
		_ = killAndWait(cmd, h.ShutdownGrace)
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("pluginhost: %s: %w (stderr-tail: %s)", d.Manifest.Address(), err, errTail.String())
	}

	conn, err := grpc.NewClient("unix:"+sockPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = killAndWait(cmd, h.ShutdownGrace)
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("pluginhost: %s: dial unix %s: %w", d.Manifest.Address(), sockPath, err)
	}

	return &BasePlugin{
		manifest:   d.Manifest,
		cmd:        cmd,
		conn:       conn,
		sockPath:   sockPath,
		stderrTail: errTail,
		grace:      h.ShutdownGrace,
	}, nil
}

// parseDuration — wrap time.ParseDuration с расширением `<N>d` для дней
// (соглашение docs/keeper/config.md → Конвенции типов). semantic-фаза
// shared/config делает то же; здесь дублирование для defence-in-depth.
func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// killAndWait — best-effort остановка плагина: SIGTERM, ждём grace, SIGKILL.
// Возвращает ошибку cmd.Wait для логирования (но не для control-flow:
// в hard-fail-пути ошибка handshake важнее).
func killAndWait(cmd *exec.Cmd, grace time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		return <-done
	}
}
