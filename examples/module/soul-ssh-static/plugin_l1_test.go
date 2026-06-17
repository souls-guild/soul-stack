package main

import (
	"context"
	"net"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// L1 — provider-as-plugin через РЕАЛЬНЫЙ gRPC server+client (тот же
// RegisterSshProviderServer, что sdk/sshprovider.Serve навешивает после
// handshake). Проверяет, что StaticProvider корректно работает по proto-контракту
// SshProvider поверх настоящего gRPC-стрима, а не in-proc вызова метода. Это L1
// для пилота: handshake-spawn под Sigil-gate покрыт на keeper/host-стороне
// (общий pluginhost); здесь — RPC-контракт самого провайдера, симметрично
// soul-cloud-aws/plugin_l1_test.go.

// serveProviderGRPC поднимает SshProvider-сервис на TCP-loopback, возвращает
// клиент + teardown.
func serveProviderGRPC(t *testing.T, impl *StaticProvider) (pluginv1.SshProviderClient, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pluginv1.RegisterSshProviderServer(srv, &l1Adapter{impl: impl})
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return pluginv1.NewSshProviderClient(conn), func() {
		_ = conn.Close()
		srv.Stop()
	}
}

// l1Adapter — мост impl→SshProviderServer (embed Unimplemented для forward-compat),
// идентичный sdk/sshprovider.serverAdapter (тот неэкспортирован).
type l1Adapter struct {
	pluginv1.UnimplementedSshProviderServer
	impl *StaticProvider
}

func (a *l1Adapter) Sign(ctx context.Context, r *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return a.impl.Sign(ctx, r)
}
func (a *l1Adapter) Authorize(ctx context.Context, r *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return a.impl.Authorize(ctx, r)
}

func TestL1_SignOverGRPC(t *testing.T) {
	keyPath, keyPEM := writeKey(t)
	client, teardown := serveProviderGRPC(t, &StaticProvider{cfg: params{KeyPath: keyPath}})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reply, err := client.Sign(ctx, &pluginv1.SignRequest{Host: "web-1", User: "soul"})
	if err != nil {
		t.Fatalf("Sign rpc: %v", err)
	}
	// private_key доехал через proto-стрим byte-exact и парсится keeper.push-ом.
	if reply.GetPrivateKey() != keyPEM {
		t.Errorf("private_key изменился при marshaling по gRPC")
	}
	if _, perr := ssh.ParsePrivateKey([]byte(reply.GetPrivateKey())); perr != nil {
		t.Errorf("private_key не парсится после gRPC: %v", perr)
	}
}

func TestL1_AuthorizeDenyOverGRPC(t *testing.T) {
	client, teardown := serveProviderGRPC(t, &StaticProvider{cfg: params{
		KeyPath: "/x",
		Deny:    []denyRule{{Host: "prod-1", User: "root"}},
	}})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	deny, err := client.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: "prod-1", User: "root"})
	if err != nil {
		t.Fatalf("Authorize rpc: %v", err)
	}
	if deny.GetAllowed() {
		t.Fatal("ждали deny по gRPC")
	}
	if deny.GetReason() == "" || deny.GetReason()[:len(sshprovider.DenyExplicitDeny)] != string(sshprovider.DenyExplicitDeny) {
		t.Errorf("reason=%q, ждали %q-префикс по gRPC", deny.GetReason(), sshprovider.DenyExplicitDeny)
	}

	allow, err := client.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: "prod-1", User: "soul"})
	if err != nil {
		t.Fatalf("Authorize rpc: %v", err)
	}
	if !allow.GetAllowed() {
		t.Errorf("ждали allow для user вне deny-list по gRPC")
	}
}
