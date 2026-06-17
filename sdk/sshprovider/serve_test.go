package sshprovider

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// shortSockPath — Unix-socket-имена на darwin ограничены ~104 байтами sun_path,
// поэтому стандартный t.TempDir() (под /var/folders/...) не подходит.
// Симметрично хелперу из sdk/handshake.
func shortSockPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ss-ssh-")
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

// captureStdout подменяет os.Stdout на pipe и возвращает канал с первой строкой,
// записанной плагином (handshake-строка), + restore-функцию.
func captureStdout(t *testing.T) (<-chan string, func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(r)
		if sc.Scan() {
			lineCh <- sc.Text()
		} else {
			lineCh <- ""
		}
	}()

	restore := func() {
		os.Stdout = orig
		_ = w.Close()
		_ = r.Close()
	}
	return lineCh, restore
}

// recordingProvider — impl, фиксирующий аргументы вызовов через реальный
// gRPC-стек (не in-process adapter), чтобы Serve→handshake→register→adapter
// были покрыты end-to-end.
type recordingProvider struct {
	signHost string
	authHost string
	authUser string
}

func (p *recordingProvider) Sign(_ context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	p.signHost = req.Host
	return &pluginv1.SignReply{Certificate: "cert-" + req.Host, TtlSeconds: 3600}, nil
}

func (p *recordingProvider) Authorize(_ context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	p.authHost = req.Host
	p.authUser = req.User
	return &pluginv1.AuthorizeReply{Allowed: false, Reason: "denied " + req.User}, nil
}

// TestServeEndToEnd поднимает Serve(impl) на реальном unix-socket, проверяет
// handshake-строку, дёргает оба RPC реальным gRPC-клиентом и завершает по SIGTERM.
func TestServeEndToEnd(t *testing.T) {
	addr := shortSockPath(t, "p.sock")
	t.Setenv(handshake.SocketEnv, addr)

	lineCh, restore := captureStdout(t)
	defer restore()

	impl := &recordingProvider{}
	doneCh := make(chan error, 1)
	go func() { doneCh <- Serve(impl) }()

	var line string
	select {
	case line = <-lineCh:
	case err := <-doneCh:
		t.Fatalf("Serve exited before handshake: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatalf("no handshake line within deadline")
	}

	var hs map[string]any
	if err := json.Unmarshal([]byte(line), &hs); err != nil {
		t.Fatalf("handshake unmarshal: %v\nline=%s", err, line)
	}
	if hs["kind"] != "KIND_SSH_PROVIDER" {
		t.Fatalf("kind=%v want KIND_SSH_PROVIDER", hs["kind"])
	}
	if v, ok := hs["protocol_version"].(float64); !ok || int32(v) != protocolVersion {
		t.Fatalf("protocol_version=%v want %d", hs["protocol_version"], protocolVersion)
	}
	if hs["address"] != addr {
		t.Fatalf("address=%v want %q", hs["address"], addr)
	}

	conn := dialPlugin(t, addr)
	defer conn.Close()
	client := pluginv1.NewSshProviderClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	signReply, err := client.Sign(ctx, &pluginv1.SignRequest{Host: "web-1", User: "root"})
	if err != nil {
		t.Fatalf("Sign RPC: %v", err)
	}
	if signReply.Certificate != "cert-web-1" || signReply.TtlSeconds != 3600 {
		t.Fatalf("Sign reply=%+v", signReply)
	}
	if impl.signHost != "web-1" {
		t.Fatalf("impl.signHost=%q want web-1", impl.signHost)
	}

	authReply, err := client.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: "web-2", User: "deploy"})
	if err != nil {
		t.Fatalf("Authorize RPC: %v", err)
	}
	if authReply.Allowed || authReply.Reason != "denied deploy" {
		t.Fatalf("Authorize reply=%+v", authReply)
	}
	if impl.authHost != "web-2" || impl.authUser != "deploy" {
		t.Fatalf("impl auth host=%q user=%q", impl.authHost, impl.authUser)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill SIGTERM: %v", err)
	}
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Serve did not return after SIGTERM")
	}
}

// TestServePropagatesHandshakeError — обёртка Serve пробрасывает ошибку
// нижележащего handshake.Serve (здесь — отсутствие адреса сокета).
func TestServePropagatesHandshakeError(t *testing.T) {
	t.Setenv(handshake.SocketEnv, "")
	err := Serve(&BaseProvider{})
	if err == nil || !strings.Contains(err.Error(), handshake.SocketEnv) {
		t.Fatalf("err=%v want mention of %s", err, handshake.SocketEnv)
	}
}

func dialPlugin(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(
		"unix:"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "unix", strings.TrimPrefix(target, "unix:"))
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return conn
}
