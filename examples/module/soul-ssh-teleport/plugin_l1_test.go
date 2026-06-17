package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// L1 — provider-as-plugin через РЕАЛЬНЫЙ gRPC server+client. Симметрично
// soul-ssh-vault/plugin_l1_test.go: проверка, что TeleportProvider корректно
// работает по proto-контракту SshProvider поверх настоящего gRPC-стрима, а не
// in-proc вызова метода. Handshake-spawn под Sigil-gate покрыт на host-стороне
// (общий pluginhost); здесь — RPC-контракт самого провайдера, включая
// round-trip нового only-add поля SignReply.proxy_jump.

func serveProviderGRPC(t *testing.T, impl *TeleportProvider) (pluginv1.SshProviderClient, func()) {
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
	impl *TeleportProvider
}

func (a *l1Adapter) Sign(ctx context.Context, r *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return a.impl.Sign(ctx, r)
}

func (a *l1Adapter) Authorize(ctx context.Context, r *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return a.impl.Authorize(ctx, r)
}

func TestL1_SignOverGRPC_WithProxyJump(t *testing.T) {
	const expectedProxy = "teleport.example.com:3023"
	const expectedCert = "ssh-ed25519-cert-v01@openssh.com AAAA-fake-cert host-1@teleport"
	mock := &mockTeleportClient{signedCert: expectedCert}
	tp := &TeleportProvider{
		cfg:       params{ProxyAddr: expectedProxy, IdentityFile: "/x", Roles: []string{"node-admin"}},
		newClient: mockFactory(mock, nil),
	}
	client, teardown := serveProviderGRPC(t, tp)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reply, err := client.Sign(ctx, &pluginv1.SignRequest{
		Host:      "web-1",
		User:      "soul",
		PublicKey: "ssh-ed25519 AAAA-test-pub",
	})
	if err != nil {
		t.Fatalf("Sign rpc: %v", err)
	}
	if reply.GetCertificate() != expectedCert {
		t.Errorf("certificate изменился при marshaling по gRPC: got %q", reply.GetCertificate())
	}
	if reply.GetPrivateKey() != "" {
		t.Errorf("private_key должен быть пустым в Teleport flow, got %q", reply.GetPrivateKey())
	}
	// Главный round-trip check для only-add поля proto.
	if reply.GetProxyJump() != expectedProxy {
		t.Errorf("proxy_jump round-trip: got %q want %q", reply.GetProxyJump(), expectedProxy)
	}
	if mock.gotPubkey != "ssh-ed25519 AAAA-test-pub" {
		t.Errorf("Teleport не получил pubkey через gRPC: %q", mock.gotPubkey)
	}
}

func TestL1_AuthorizeDenyOverGRPC(t *testing.T) {
	client, teardown := serveProviderGRPC(t, &TeleportProvider{cfg: params{
		Deny: []denyRule{{Host: "prod-1", User: "root"}},
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
	if !strings.HasPrefix(deny.GetReason(), string(sshprovider.DenyExplicitDeny)) {
		t.Errorf("reason=%q, ждали %q-префикс", deny.GetReason(), sshprovider.DenyExplicitDeny)
	}

	allow, err := client.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: "prod-1", User: "soul"})
	if err != nil {
		t.Fatalf("Authorize rpc: %v", err)
	}
	if !allow.GetAllowed() {
		t.Errorf("ждали allow для user вне deny-list")
	}
}

func TestL1_SignRejectsEmptyPubkeyOverGRPC(t *testing.T) {
	client, teardown := serveProviderGRPC(t, &TeleportProvider{cfg: params{
		ProxyAddr: "p:3023", IdentityFile: "/x",
	}})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.Sign(ctx, &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: ""})
	if err == nil {
		t.Fatal("ждали ошибку на пустой public_key через gRPC (Teleport = Keeper-ephemeral)")
	}
}
