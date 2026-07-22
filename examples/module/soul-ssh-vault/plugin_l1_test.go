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

// L1 is provider-as-plugin through a REAL gRPC server+client. Symmetrical with
// soul-ssh-static/plugin_l1_test.go: verifies that VaultProvider correctly works
// through the SshProvider proto contract over a real gRPC stream, not an in-proc
// method call. Handshake-spawn under Sigil-gate is covered on the host side
// (shared pluginhost); here we cover the provider RPC contract itself.

func serveProviderGRPC(t *testing.T, impl *VaultProvider) (pluginv1.SshProviderClient, func()) {
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

// l1Adapter bridges impl -> SshProviderServer (embedding Unimplemented for
// forward compatibility), matching sdk/sshprovider.serverAdapter (not exported).
type l1Adapter struct {
	pluginv1.UnimplementedSshProviderServer
	impl *VaultProvider
}

func (a *l1Adapter) Sign(ctx context.Context, r *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return a.impl.Sign(ctx, r)
}

func (a *l1Adapter) Authorize(ctx context.Context, r *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return a.impl.Authorize(ctx, r)
}

func TestL1_SignOverGRPC(t *testing.T) {
	mock := &mockVault{
		signMount: "ssh", signRole: "keeper-push",
		signedKey:    "ssh-ed25519-cert-v01@openssh.com AAAA-fake-cert host-1@keeper",
		requireToken: true,
	}
	srv := mock.start(t)

	p := params{
		VaultAddr: srv.URL, VaultMount: "ssh", Role: "keeper-push",
		AuthMethod: authMethodToken, Token: "test-token",
	}
	client, teardown := serveProviderGRPC(t, &VaultProvider{cfg: p, newClient: realClientForMock(srv.URL, p.Token)})
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
	if reply.GetCertificate() != mock.signedKey {
		t.Errorf("certificate changed during gRPC marshaling: got %q", reply.GetCertificate())
	}
	if reply.GetPrivateKey() != "" {
		t.Errorf("private_key must be empty in Vault SSH CA flow, got %q", reply.GetPrivateKey())
	}
	if mock.gotBody["public_key"] != "ssh-ed25519 AAAA-test-pub" {
		t.Errorf("Vault did not receive pubkey over gRPC: %v", mock.gotBody["public_key"])
	}
}

func TestL1_AuthorizeDenyOverGRPC(t *testing.T) {
	client, teardown := serveProviderGRPC(t, &VaultProvider{cfg: params{
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
		t.Fatal("expected deny over gRPC")
	}
	if !strings.HasPrefix(deny.GetReason(), string(sshprovider.DenyExplicitDeny)) {
		t.Errorf("reason=%q, expected %q prefix", deny.GetReason(), sshprovider.DenyExplicitDeny)
	}

	allow, err := client.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: "prod-1", User: "soul"})
	if err != nil {
		t.Fatalf("Authorize rpc: %v", err)
	}
	if !allow.GetAllowed() {
		t.Errorf("expected allow for user outside deny-list")
	}
}

func TestL1_SignRejectsEmptyPubkeyOverGRPC(t *testing.T) {
	client, teardown := serveProviderGRPC(t, &VaultProvider{cfg: params{
		VaultAddr: "http://nowhere", Role: "r", AuthMethod: authMethodToken, Token: "t",
	}})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.Sign(ctx, &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: ""})
	if err == nil {
		t.Fatal("expected error for empty public_key over gRPC (Vault SSH CA = Keeper-ephemeral)")
	}
}
