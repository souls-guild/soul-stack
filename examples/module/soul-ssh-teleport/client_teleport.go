package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	tpclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
)

// realTeleportClient — production-реализация teleportClient поверх
// github.com/gravitational/teleport/api/client. Узкая обёртка: только
// GenerateUserCerts (минимальная поверхность под Sign) + Close. Не
// рекурсивно тянет teleport-server-codebase: модуль `teleport/api` —
// standalone (специально вынесен Gravitational под плагин-сценарии).
type realTeleportClient struct {
	c *tpclient.Client
	// defaultTTL — TTL запрашиваемого user-cert (TTL Teleport-роли,
	// назначенной identity/bot, бьёт верхним потолком).
	defaultTTL time.Duration
	// clusterName — RouteToCluster (multi-cluster trust). Пустой =
	// текущий кластер identity-file.
	clusterName string
}

// defaultClient — фабрика production-teleportClient: открывает gRPC-коннект
// в Teleport Auth через Teleport proxy по identity-file / tbot-сокету.
//
// Creds-flow B (PM-decision): плагин аутентифицируется сам.
//   - identity_file → tpclient.LoadIdentityFile (выпускается `tctl auth sign`).
//   - tbot_socket   → tpclient.LoadIdentityFile поверх renewable-bundle,
//     который tbot пишет в свой dest (поведение совместимо с identity-file
//     форматом — tbot пишет тот же формат). Реальные deployments tbot
//     обычно указывают на ту же directory; полное native-tbot подключение
//     через unix-socket — расширение (не пилот).
//
// Returns: client + nil → ok; nil + err → fail (Sign-ветка обернёт
// в SignFailIssue).
func defaultClient(ctx context.Context, p params) (teleportClient, error) {
	if p.IdentityFile == "" && p.TbotSocket == "" {
		// loadParams уже это проверил, но на defense-in-depth: фабрика
		// не должна тихо стартовать без credentials.
		return nil, errors.New("нет credentials: identity_file/tbot_socket пусты")
	}
	credPath := p.IdentityFile
	if credPath == "" {
		credPath = p.TbotSocket
	}
	creds := tpclient.LoadIdentityFile(credPath)

	c, err := tpclient.New(ctx, tpclient.Config{
		Addrs:       []string{p.ProxyAddr},
		Credentials: []tpclient.Credentials{creds},
	})
	if err != nil {
		return nil, fmt.Errorf("teleport client: %w", err)
	}
	return &realTeleportClient{
		c:           c,
		defaultTTL:  12 * time.Hour, // потолок Teleport-роли всё равно сожмёт
		clusterName: p.ClusterName,
	}, nil
}

func (r *realTeleportClient) GenerateUserSSHCert(ctx context.Context, pubkey, principal string, roles []string) (string, error) {
	resp, err := r.c.GenerateUserCerts(ctx, proto.UserCertsRequest{
		// UserCertsRequest содержит ПАРУ ключей (SSH + TLS) для будущих
		// сценариев combined-cert; для чистого SSH-flow заполняем только
		// SSHPublicKey, TLSPublicKey оставляем пустым — Teleport под
		// эту комбинацию выдаст только SSH-cert (см. lib/auth GenerateUserCerts).
		SSHPublicKey:   []byte(pubkey),
		Username:       principal,
		Expires:        time.Now().Add(r.defaultTTL),
		RouteToCluster: r.clusterName,
	})
	if err != nil {
		return "", err
	}
	if resp == nil || len(resp.SSH) == 0 {
		return "", errors.New("teleport: empty SSH cert in response")
	}
	_ = roles // Teleport применяет роли identity, additional-roles запрашиваются через RoleRequests на cert request — отложено (не пилот).
	return string(resp.SSH), nil
}

func (r *realTeleportClient) Close() error {
	if r.c == nil {
		return nil
	}
	return r.c.Close()
}
