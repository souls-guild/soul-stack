package module

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
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

// shortSockPath — Unix-socket names on darwin are limited to ~104 bytes of
// sun_path. Mirrors the helper in sdk/handshake.
func shortSockPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ss-mod-")
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

// captureStdout swaps os.Stdout for a pipe and returns a channel with the
// first line (the handshake line), plus a restore function.
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

// recordingModule is an impl that records call arguments via the real
// gRPC stack (not the in-process adapter): Serve→handshake→register→adapter end-to-end.
type recordingModule struct {
	validateState string
	planState     string
	applyState    string
}

func (m *recordingModule) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	m.validateState = req.State
	return &pluginv1.ValidateReply{Ok: false, Errors: []string{"bad " + req.State}}, nil
}

func (m *recordingModule) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	m.planState = req.State
	return stream.Send(&pluginv1.PlanEvent{})
}

func (m *recordingModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.applyState = req.State
	return stream.Send(&pluginv1.ApplyEvent{})
}

// TestServeEndToEnd brings up Serve(impl) on a real unix socket, checks the
// handshake line, drives all three RPCs with a real gRPC client (including
// reading events off the server stream), and shuts down via SIGTERM.
func TestServeEndToEnd(t *testing.T) {
	addr := shortSockPath(t, "m.sock")
	t.Setenv(handshake.SocketEnv, addr)

	lineCh, restore := captureStdout(t)
	defer restore()

	impl := &recordingModule{}
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
	if hs["kind"] != "KIND_SOUL_MODULE" {
		t.Fatalf("kind=%v want KIND_SOUL_MODULE", hs["kind"])
	}
	if v, ok := hs["protocol_version"].(float64); !ok || int32(v) != protocolVersion {
		t.Fatalf("protocol_version=%v want %d", hs["protocol_version"], protocolVersion)
	}
	if hs["address"] != addr {
		t.Fatalf("address=%v want %q", hs["address"], addr)
	}

	conn := dialPlugin(t, addr)
	defer conn.Close()
	client := pluginv1.NewSoulModuleClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	vReply, err := client.Validate(ctx, &pluginv1.ValidateRequest{State: "installed"})
	if err != nil {
		t.Fatalf("Validate RPC: %v", err)
	}
	if vReply.Ok || len(vReply.Errors) != 1 || vReply.Errors[0] != "bad installed" {
		t.Fatalf("Validate reply=%+v", vReply)
	}
	if impl.validateState != "installed" {
		t.Fatalf("impl.validateState=%q", impl.validateState)
	}

	if n := drainStream(t, "Plan", func() (interface {
		Recv() (*pluginv1.PlanEvent, error)
	}, error) {
		return client.Plan(ctx, &pluginv1.PlanRequest{State: "p"})
	}); n != 1 {
		t.Fatalf("Plan events=%d want 1", n)
	}
	if impl.planState != "p" {
		t.Fatalf("impl.planState=%q", impl.planState)
	}

	if n := drainStream(t, "Apply", func() (interface {
		Recv() (*pluginv1.ApplyEvent, error)
	}, error) {
		return client.Apply(ctx, &pluginv1.ApplyRequest{State: "a"})
	}); n != 1 {
		t.Fatalf("Apply events=%d want 1", n)
	}
	if impl.applyState != "a" {
		t.Fatalf("impl.applyState=%q", impl.applyState)
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

// TestServePropagatesHandshakeError checks that the Serve wrapper propagates
// an error from the underlying handshake.Serve (missing socket address).
func TestServePropagatesHandshakeError(t *testing.T) {
	t.Setenv(handshake.SocketEnv, "")
	err := Serve(&BaseModule{})
	if err == nil || !strings.Contains(err.Error(), handshake.SocketEnv) {
		t.Fatalf("err=%v want mention of %s", err, handshake.SocketEnv)
	}
}

// drainStream reads all events from the server stream until io.EOF and returns their count.
func drainStream[E any](t *testing.T, name string, open func() (interface{ Recv() (*E, error) }, error)) int {
	t.Helper()
	stream, err := open()
	if err != nil {
		t.Fatalf("%s open: %v", name, err)
	}
	count := 0
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return count
		}
		if err != nil {
			t.Fatalf("%s Recv: %v", name, err)
		}
		count++
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
