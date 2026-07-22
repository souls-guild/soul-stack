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

// BasePlugin — a handle to one plugin spawn session, kind-agnostic.
//
// Not reusable across RPC series: there is no connection pool (one-shot per Spawn,
// ADR-020(d)). The caller must call [BasePlugin.Close] when done — otherwise a
// zombie process and socket file are left behind.
//
// Kind-specific wrappers (SoulModulePlugin / CloudDriverPlugin / SshProviderPlugin)
// embed *BasePlugin and add a kind-specific gRPC client over [BasePlugin.Conn].
type BasePlugin struct {
	manifest   *sharedplugin.Manifest
	cmd        *exec.Cmd
	conn       *grpc.ClientConn
	sockPath   string
	stderrTail *tailBuffer
	grace      time.Duration
	closed     bool
}

// Manifest — read-only access to the plugin manifest. Used by callsites that need
// namespace/name for logs or OTel attributes.
func (p *BasePlugin) Manifest() *sharedplugin.Manifest { return p.manifest }

// NewBasePluginForTest — a "manifest-only" BasePlugin constructor for use cases
// that need a kind cross-check without a real fork: e.g. tests of kind-specific
// wrappers ([keeper/internal/pluginhost.NewCloudDriverPlugin] rejecting a foreign
// kind).
//
// Use outside tests is a bug: the returned BasePlugin has neither Conn nor Cmd, any
// RPC over it will nil-pointer.
func NewBasePluginForTest(m *sharedplugin.Manifest) *BasePlugin {
	return &BasePlugin{manifest: m, closed: true}
}

// Conn — the gRPC conn to the plugin. Used by kind-specific wrappers to create a
// client (NewSoulModuleClient / NewCloudDriverClient / NewSshProviderClient).
func (p *BasePlugin) Conn() *grpc.ClientConn { return p.conn }

// StderrTail — the last ~4KB of the plugin's stderr at call time. Used when
// building TaskError / diagnosing plugin crashes.
func (p *BasePlugin) StderrTail() string { return p.stderrTail.String() }

// Close ends the plugin session: closes the gRPC conn, sends SIGTERM, waits
// shutdown_grace, and SIGKILL if needed. Idempotent. Returns the first non-nil
// error from conn.Close / cmd.Wait, or nil if all steps succeeded.
//
// Socket-file removal is best-effort; the plugin usually unlinks it itself in its
// signal handler (sdk/handshake → Lifecycle), but the host cleans up in case of a
// non-graceful exit.
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
		// A non-zero exit means "the plugin exited, but with an error"; for our
		// Close context that's not fatal (diagnostics go through StderrTail).
		return true
	}
	return false
}
