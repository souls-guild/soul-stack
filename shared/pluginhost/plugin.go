package pluginhost

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	"google.golang.org/grpc"
)

// BasePlugin — handle одной spawn-сессии плагина, kind-agnostic.
//
// Не reusable между сериями RPC: connection-pool отсутствует (one-shot per
// Spawn по ADR-020(d)). Caller обязан вызвать [BasePlugin.Close] после
// завершения работы — иначе остаются zombie-процесс и socket-файл.
//
// Kind-specific обёртки (SoulModulePlugin / CloudDriverPlugin /
// SshProviderPlugin) embed-ят *BasePlugin и добавляют kind-specific
// gRPC-client поверх [BasePlugin.Conn].
type BasePlugin struct {
	manifest   *sharedplugin.Manifest
	cmd        *exec.Cmd
	conn       *grpc.ClientConn
	sockPath   string
	stderrTail *tailBuffer
	grace      time.Duration
	closed     bool
}

// Manifest — read-only доступ к manifest-у плагина. Используется в callsite-ах,
// которым нужен namespace/name для логов или OTel-атрибутов.
func (p *BasePlugin) Manifest() *sharedplugin.Manifest { return p.manifest }

// NewBasePluginForTest — конструктор «manifest-only» BasePlugin для use-case-ов,
// где нужен kind-cross-check без реального fork-а: например, тесты
// kind-specific wrap-ов ([keeper/internal/pluginhost.NewCloudDriverPlugin]
// rejecting на чужой kind).
//
// Использование вне тестов — баг: возвращённый BasePlugin не имеет ни Conn-а,
// ни Cmd-а, любой RPC по нему упадёт nil-pointer-ом.
func NewBasePluginForTest(m *sharedplugin.Manifest) *BasePlugin {
	return &BasePlugin{manifest: m, closed: true}
}

// Conn — gRPC-conn к плагину. Используется kind-specific обёртками для
// создания клиента (NewSoulModuleClient / NewCloudDriverClient /
// NewSshProviderClient).
func (p *BasePlugin) Conn() *grpc.ClientConn { return p.conn }

// StderrTail — последние ~4KB stderr плагина к моменту вызова. Используется
// при формировании TaskError / диагностики crash-ей плагина.
func (p *BasePlugin) StderrTail() string { return p.stderrTail.String() }

// Close завершает сессию плагина: закрывает gRPC-conn, шлёт SIGTERM, ждёт
// shutdown_grace, при необходимости — SIGKILL. Идемпотентен. Возвращает
// первую ненулевую ошибку из conn.Close / cmd.Wait, если все остальные
// шаги прошли успешно — nil.
//
// Удаление сокет-файла — best-effort; плагин обычно unlink-ает сам в
// signal-handler-е (sdk/handshake → Lifecycle), но host подчищает на
// случай non-graceful exit.
func (p *BasePlugin) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true

	var firstErr error
	if p.conn != nil {
		if err := p.conn.Close(); err != nil {
			firstErr = fmt.Errorf("conn close: %w", err)
		}
	}
	if err := killAndWait(p.cmd, p.grace); err != nil && firstErr == nil {
		if !isExpectedExitErr(err) {
			firstErr = fmt.Errorf("wait: %w", err)
		}
	}
	_ = os.Remove(p.sockPath)
	return firstErr
}

func isExpectedExitErr(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// Не-zero exit — это «плагин завершился, но с ошибкой»; для
		// нашего контекста Close это не fatal (диагностика идёт через
		// StderrTail).
		return true
	}
	return false
}
