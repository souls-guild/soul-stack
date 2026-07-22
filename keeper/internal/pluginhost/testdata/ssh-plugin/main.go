// Минимальный SshProvider-плагин для integration-теста pluginhost-а.
// Сборка: `go build -o soul-ssh-fake .` в эту директорию.
//
// Поведение:
//   - Sign: возвращает Certificate="cert-for-<host>", TtlSeconds=1800.
//   - Authorize: Allowed=true, если User != "denied", иначе Allowed=false с Reason.
package main

import (
	"context"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
)

type FakeSsh struct {
	sshprovider.BaseProvider
}

func (FakeSsh) Sign(_ context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return &pluginv1.SignReply{
		Certificate: "cert-for-" + req.GetHost(),
		TtlSeconds:  1800,
	}, nil
}

func (FakeSsh) Authorize(_ context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	if req.GetUser() == "denied" {
		return &pluginv1.AuthorizeReply{Allowed: false, Reason: "user denied by policy"}, nil
	}
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}

func main() {
	if err := sshprovider.Serve(&FakeSsh{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-ssh-fake:", err)
		os.Exit(1)
	}
}
