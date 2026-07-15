package handshake

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// shortSockPath — Unix-socket names on darwin are limited to ~104 bytes of
// sun_path, so the standard t.TempDir() (under /var/folders/...) doesn't fit.
// We create a temp dir under /tmp with a short suffix and clean it up ourselves.
func shortSockPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ss-hs-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, name)
	if len(p) > 100 {
		t.Fatalf("socket path too long for darwin sun_path: %s", p)
	}
	return p
}

func TestMarshalHandshakeSingleLine(t *testing.T) {
	cfg := Config{
		ProtocolVersion: 1,
		Kind:            pluginv1.Kind_KIND_SOUL_MODULE,
	}
	line, err := marshalHandshake(cfg, "/var/run/soul-stack/plugins/x.sock")
	if err != nil {
		t.Fatalf("marshalHandshake: %v", err)
	}
	if strings.ContainsAny(line, "\n\r") {
		t.Fatalf("handshake line must be single-line, got %q", line)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("unmarshal handshake: %v\nline: %s", err, line)
	}

	if got["soul_stack"] != Magic {
		t.Fatalf("soul_stack=%v want %q", got["soul_stack"], Magic)
	}
	if got["network"] != NetworkUnix {
		t.Fatalf("network=%v want %q", got["network"], NetworkUnix)
	}
	if got["address"] != "/var/run/soul-stack/plugins/x.sock" {
		t.Fatalf("address=%v", got["address"])
	}
	if v, ok := got["protocol_version"].(float64); !ok || int32(v) != 1 {
		t.Fatalf("protocol_version=%v (%T)", got["protocol_version"], got["protocol_version"])
	}
	if got["kind"] != "KIND_SOUL_MODULE" {
		t.Fatalf("kind=%v want KIND_SOUL_MODULE", got["kind"])
	}
	if got["server_cert"] != "" {
		t.Fatalf("server_cert=%v want empty", got["server_cert"])
	}
}

func TestServeRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"nil version", Config{Kind: pluginv1.Kind_KIND_SOUL_MODULE}, "ProtocolVersion"},
		{"nil kind", Config{ProtocolVersion: 1}, "Kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Serve(tc.cfg, func(*grpc.Server) {})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want substring %q", err, tc.want)
			}
		})
	}

	if err := Serve(Config{ProtocolVersion: 1, Kind: pluginv1.Kind_KIND_SOUL_MODULE}, nil); err == nil {
		t.Fatalf("expected error for nil register")
	}
}

func TestServeMissingAddress(t *testing.T) {
	t.Setenv(SocketEnv, "")
	err := Serve(Config{
		ProtocolVersion: 1,
		Kind:            pluginv1.Kind_KIND_SOUL_MODULE,
	}, func(*grpc.Server) {})
	if err == nil || !strings.Contains(err.Error(), SocketEnv) {
		t.Fatalf("err=%v want mention of %s", err, SocketEnv)
	}
}

// TestServeAddressFromEnv checks that Address defaults to resolving from the
// SOUL_PLUGIN_SOCKET env var and that the socket actually comes up.
func TestServeAddressFromEnv(t *testing.T) {
	addr := shortSockPath(t, "x.sock")
	t.Setenv(SocketEnv, addr)

	var stdout safeBuffer
	stopCh := make(chan os.Signal, 1)

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- serveWithStop(Config{
			ProtocolVersion: 1,
			Kind:            pluginv1.Kind_KIND_SOUL_MODULE,
			ShutdownGrace:   500 * time.Millisecond,
		}, func(*grpc.Server) {}, &stdout, stopCh)
	}()

	line := waitHandshakeLineOrErr(t, &stdout, doneCh)
	if !strings.Contains(line, `"address":"`+addr+`"`) {
		t.Fatalf("handshake line missing env-addr; got: %s", line)
	}
	if !strings.Contains(line, `"soul_stack":"plugin-v1"`) {
		t.Fatalf("handshake missing magic; got: %s", line)
	}

	if _, err := net.DialTimeout("unix", addr, time.Second); err != nil {
		t.Fatalf("dial unix %s: %v", addr, err)
	}

	stopCh <- syscall.SIGTERM
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve did not return after stop signal")
	}
}

// TestServeExplicitAddressOverridesEnv checks that Config.Address takes priority.
func TestServeExplicitAddressOverridesEnv(t *testing.T) {
	envAddr := shortSockPath(t, "env.sock")
	explicitAddr := shortSockPath(t, "explicit.sock")
	t.Setenv(SocketEnv, envAddr)

	var stdout safeBuffer
	stopCh := make(chan os.Signal, 1)

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- serveWithStop(Config{
			ProtocolVersion: 1,
			Kind:            pluginv1.Kind_KIND_SOUL_MODULE,
			Address:         explicitAddr,
			ShutdownGrace:   500 * time.Millisecond,
		}, func(*grpc.Server) {}, &stdout, stopCh)
	}()

	line := waitHandshakeLineOrErr(t, &stdout, doneCh)
	if !strings.Contains(line, `"address":"`+explicitAddr+`"`) {
		t.Fatalf("handshake.address=%q want %q", line, explicitAddr)
	}

	stopCh <- syscall.SIGTERM
	<-doneCh
}

// TestServeShutdownGraceTriggersHardStop checks that an open client connection
// would normally hold up GracefulStop, but a short grace forces the SDK over
// to Stop instead.
func TestServeShutdownGraceTriggersHardStop(t *testing.T) {
	addr := shortSockPath(t, "x.sock")

	var stdout safeBuffer
	stopCh := make(chan os.Signal, 1)

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- serveWithStop(Config{
			ProtocolVersion: 1,
			Kind:            pluginv1.Kind_KIND_SOUL_MODULE,
			Address:         addr,
			ShutdownGrace:   200 * time.Millisecond,
		}, func(*grpc.Server) {}, &stdout, stopCh)
	}()

	_ = waitHandshakeLine(t, &stdout)

	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	start := time.Now()
	stopCh <- syscall.SIGTERM
	select {
	case serveErr := <-doneCh:
		if serveErr != nil {
			t.Fatalf("Serve err: %v", serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve hung past grace period")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("hard stop took too long: %s", elapsed)
	}
}

// safeBuffer is a thread-safe bytes.Buffer for receiving stdout from a goroutine.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitHandshakeLine(t *testing.T, b *safeBuffer) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := b.String()
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return s[:i]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("handshake line not produced within deadline; got: %q", b.String())
	return ""
}

func waitHandshakeLineOrErr(t *testing.T, b *safeBuffer, doneCh <-chan error) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-doneCh:
			t.Fatalf("Serve exited early: %v; stdout=%q", err, b.String())
		default:
		}
		s := b.String()
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return s[:i]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("handshake line not produced within deadline; got: %q", b.String())
	return ""
}
