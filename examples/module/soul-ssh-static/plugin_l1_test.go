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

// L1 is provider-as-plugin through a REAL gRPC server+client (the same
// RegisterSshProviderServer that sdk/sshprovider.Serve attaches after
// handshake). It verifies that StaticProvider correctly works through the
// SshProvider proto contract over a real gRPC stream, not an in-proc method
// call. This is L1 for the pilot: handshake-spawn under Sigil-gate is covered on
// the keeper/host side (shared pluginhost); here we cover the provider RPC
// contract itself, symmetrically with soul-cloud-aws/plugin_l1_test.go.

// serveProviderGRPC starts the SshProvider service on TCP loopback and returns a
// client + teardown.
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

// l1Adapter bridges impl -> SshProviderServer (embedding Unimplemented for
// forward compatibility), matching sdk/sshprovider.serverAdapter (not exported).
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
	// private_key arrived through the proto stream byte-exact and can be parsed by
	// keeper.push.
	if reply.GetPrivateKey() != keyPEM {
		t.Errorf("private_key changed during gRPC marshaling")
	}
	if _, perr := ssh.ParsePrivateKey([]byte(reply.GetPrivateKey())); perr != nil {
		t.Errorf("private_key is not parseable after gRPC: %v", perr)
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
		t.Fatal("expected deny over gRPC")
	}
	if deny.GetReason() == "" || deny.GetReason()[:len(sshprovider.DenyExplicitDeny)] != string(sshprovider.DenyExplicitDeny) {
		t.Errorf("reason=%q, expected %q prefix over gRPC", deny.GetReason(), sshprovider.DenyExplicitDeny)
	}

	allow, err := client.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: "prod-1", User: "soul"})
	if err != nil {
		t.Fatalf("Authorize rpc: %v", err)
	}
	if !allow.GetAllowed() {
		t.Errorf("expected allow for user outside deny-list over gRPC")
	}
}
