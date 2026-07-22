package sshprovider

import (
	"context"
	"errors"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// TestBaseProviderSignEmpty verifies the BaseProvider.Sign default returns
// an empty SignReply without error (no-op before the author overrides it).
func TestBaseProviderSignEmpty(t *testing.T) {
	var b BaseProvider
	reply, err := b.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "root"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if reply == nil || reply.Certificate != "" || reply.PrivateKey != "" || reply.TtlSeconds != 0 {
		t.Fatalf("Sign reply=%+v, want empty", reply)
	}
}

// TestBaseProviderAuthorizeAllowed verifies the BaseProvider.Authorize
// default returns allowed=true (allow-by-default for smoke tests).
func TestBaseProviderAuthorizeAllowed(t *testing.T) {
	var b BaseProvider
	reply, err := b.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "h", User: "root"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !reply.Allowed || reply.Reason != "" {
		t.Fatalf("Authorize reply=%+v, want allowed", reply)
	}
}

// TestServerAdapterDelegates verifies the adapter proxies calls to the
// user-impl with the correct parameters and propagates errors.
func TestServerAdapterDelegates(t *testing.T) {
	wantErr := errors.New("vault down")
	impl := &fakeProvider{
		signErr: wantErr,
		authorizeReply: &pluginv1.AuthorizeReply{
			Allowed: false,
			Reason:  "policy denied",
		},
	}
	adapter := &serverAdapter{impl: impl}

	if _, err := adapter.Sign(context.Background(), &pluginv1.SignRequest{Host: "h1", User: "ops"}); !errors.Is(err, wantErr) {
		t.Fatalf("Sign err=%v want %v", err, wantErr)
	}
	if impl.signHost != "h1" || impl.signUser != "ops" {
		t.Fatalf("Sign host=%q user=%q", impl.signHost, impl.signUser)
	}

	reply, err := adapter.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "h2", User: "deploy"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if impl.authHost != "h2" || impl.authUser != "deploy" {
		t.Fatalf("Authorize host=%q user=%q", impl.authHost, impl.authUser)
	}
	if reply.Allowed || reply.Reason != "policy denied" {
		t.Fatalf("Authorize reply=%+v", reply)
	}
}

// fakeProvider is a mock SshProvider implementation for adapter tests.
type fakeProvider struct {
	signHost       string
	signUser       string
	signErr        error
	authHost       string
	authUser       string
	authorizeReply *pluginv1.AuthorizeReply
}

func (f *fakeProvider) Sign(_ context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	f.signHost = req.Host
	f.signUser = req.User
	return nil, f.signErr
}

func (f *fakeProvider) Authorize(_ context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	f.authHost = req.Host
	f.authUser = req.User
	return f.authorizeReply, nil
}
