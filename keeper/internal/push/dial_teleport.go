package push

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"

	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proxy"
	apissh "github.com/gravitational/teleport/api/ssh"
	"golang.org/x/crypto/ssh"
)

// TeleportDialerConfig — Teleport-creds для by-name bootstrap-транспорта
// (ADR-063 amendment «Teleport by-name transport»). Источник — keeper.yml
// push-блок (`push.transport: teleport` + `push.teleport.*`), НЕ плагин:
// в teleport-режиме доставка идёт целиком через Teleport identity-file, а
// плагин soul-ssh-teleport в этом флоу не участвует.
type TeleportDialerConfig struct {
	// ProxyAddr — `host:port` Teleport Proxy (gRPC sshgrpc-listener, обычно
	// `<proxy>:443`). Обязателен.
	ProxyAddr string
	// IdentityFile — путь к Teleport identity-file (выписан `tctl auth sign`
	// для bot/role с доступом к целевым нодам). Несёт TLS-cert+key для mTLS к
	// Proxy И SSH user-cert + known_hosts (host-CA) для target-handshake.
	// Обязателен.
	IdentityFile string
	// Cluster — имя Teleport-кластера, в котором резолвятся node-name-ы.
	// Обязателен (передаётся в DialHost как `cluster`).
	Cluster string
}

// NewTeleportDialer строит [Dialer], который открывает SSH-сессию к target
// ЧЕРЕЗ Teleport Proxy BY-NAME (target = node-name = SID/FQDN, НЕ IP).
//
// Зачем отдельный путь от [Dial]: generic-ProxyJump ([dialViaProxy]) ходит на
// target по IP через direct-tcpip-канал bastion-а — с Teleport это
// несовместимо (live-доказано: `tsh ssh root@<IP>` → node offline, `root@<FQDN>`
// → ok; Teleport адресует ноды по зарегистрированному имени, не по IP).
//
// Auth-модель (a), ADR-063 amendment: транспорт + user-auth + host-verify
// делаются ЦЕЛИКОМ через Teleport identity-file:
//   - mTLS-gRPC к Proxy — из creds.TLSConfig() (client-cert + Proxy-CA pool);
//   - DialHost(node-name, cluster) — Proxy резолвит ноду по имени и открывает
//     SSH-туннель;
//   - target SSH-handshake — creds.SSHClientConfig(): PublicKeyAuth.Signers
//     (user-cert от Teleport SSH CA) + HostKeyCallback (known_hosts из той же
//     identity, host-verify через Teleport host-CA).
//
// ★ Поля [DialConfig] Auth / HostAuthorities / ProxyJump / ProxyHostAuthority
// в teleport-режиме ИГНОРИРУЮТСЯ: вся аутентификация и host-verify приходят из
// identity-file, а не из SignReply/Vault-host-CA. Caller (core.bootstrap.delivered
// в teleport-ветке) их и не заполняет — пропускает Authorize/Sign/ephemeral.
// Используются только [DialConfig.Host] (= SID, node-name), [DialConfig.Port],
// [DialConfig.User] и [DialConfig.Timeout].
//
// Connect ленивый per-Dial (новый proxy.Client на каждую сессию): доставка
// токена — редкая oneshot-операция (несколько VM за cloud-create-прогон),
// удержание долгоживущего proxy-клиента не оправдано. proxy.Client закрывается
// сразу после установления target-канала — туннель уже мультиплексирован в
// возвращённый net.Conn и переживёт закрытие gRPC-клиента.
func NewTeleportDialer(cfg TeleportDialerConfig) (Dialer, error) {
	if cfg.ProxyAddr == "" {
		return nil, errors.New("push: NewTeleportDialer: proxy_addr пуст")
	}
	if cfg.IdentityFile == "" {
		return nil, errors.New("push: NewTeleportDialer: identity_file пуст")
	}
	if cfg.Cluster == "" {
		return nil, errors.New("push: NewTeleportDialer: cluster пуст")
	}

	// Preflight: один раз убеждаемся, что identity-file существует и парсится
	// (TLSConfig() И SSHClientConfig() поднимаются). Без этого отсутствующий/битый
	// файл всплыл бы только на первом Dial (поздно — на каждой VM в прогоне);
	// ловим на старте keeper-а → fail-closed через buildBootstrapTeleportDialer →
	// errSetupFailed.
	//
	// ★ Загруженные creds НЕ кешируются для самих Dial-ов: identity протухает
	// (~8-12ч), оператор перевыпускает файл, и Dial обязан читать СВЕЖИЙ — поэтому
	// preflight load+проверка+discard, а Dial-замыкание ниже делает свой
	// LoadIdentityFile на каждый вызов. apiclient.LoadIdentityFile ленивый (объект
	// читает файл при первом TLSConfig/SSHClientConfig), и preflight-объект здесь
	// не шарится с Dial-замыканием.
	preflight := apiclient.LoadIdentityFile(cfg.IdentityFile)
	if _, err := preflight.TLSConfig(); err != nil {
		return nil, fmt.Errorf("push: NewTeleportDialer: identity %q: TLS-config: %w", cfg.IdentityFile, err)
	}
	if _, err := preflight.SSHClientConfig(); err != nil {
		return nil, fmt.Errorf("push: NewTeleportDialer: identity %q: SSH-client-config: %w", cfg.IdentityFile, err)
	}

	return func(ctx context.Context, dialCfg DialConfig) (Session, error) {
		creds := apiclient.LoadIdentityFile(cfg.IdentityFile)

		tlsConfig, err := creds.TLSConfig()
		if err != nil {
			return nil, fmt.Errorf("push: teleport identity %q: TLS-config: %w", cfg.IdentityFile, err)
		}
		sshConfig, err := creds.SSHClientConfig()
		if err != nil {
			return nil, fmt.Errorf("push: teleport identity %q: SSH-client-config: %w", cfg.IdentityFile, err)
		}

		proxyClient, err := proxy.NewClient(ctx, proxy.ClientConfig{
			ProxyAddress: cfg.ProxyAddr,
			SSHConfig:    sshConfig,
			TLSConfigFunc: func(string) (*tls.Config, error) {
				return tlsConfig, nil
			},
			DialTimeout: dialCfg.Timeout,
		})
		if err != nil {
			return nil, fmt.Errorf("push: teleport proxy-client %s: %w", cfg.ProxyAddr, err)
		}

		// ★ target = node-name = SID (НЕ primary_ip). Proxy резолвит ноду по
		// имени в Teleport-реестре; IP здесь привёл бы к 'offline or does not
		// exist'.
		target := net.JoinHostPort(dialCfg.Host, strconv.Itoa(dialCfg.Port))
		conn, _, err := proxyClient.DialHost(ctx, target, cfg.Cluster, nil)
		if err != nil {
			_ = proxyClient.Close()
			return nil, fmt.Errorf("push: teleport dial-host %s (cluster %q): %w", target, cfg.Cluster, err)
		}

		client, err := newTeleportSSHClient(ctx, conn, target, dialCfg.User, proxyClient)
		if err != nil {
			_ = conn.Close()
			_ = proxyClient.Close()
			return nil, err
		}

		// proxy.Client больше не нужен: target-туннель живёт в conn, обёрнутом
		// в *ssh.Client. Закрываем gRPC-клиент сразу, чтобы не утекали его
		// соединения за время доставки токена.
		_ = proxyClient.Close()
		return &sshSession{client: client}, nil
	}, nil
}

// newTeleportSSHClient проводит SSH-handshake к target поверх уже открытого
// Teleport-туннеля (conn от DialHost). User-auth и host-verify берутся из
// proxyClient.SSHConfig(user) — той же identity, что у proxy-клиента.
func newTeleportSSHClient(ctx context.Context, conn net.Conn, addr, user string, proxyClient *proxy.Client) (*ssh.Client, error) {
	sshClientConfig := proxyClient.SSHConfig(user)
	sshConn, chans, reqs, err := apissh.NewClientConn(ctx, conn, addr, sshClientConfig)
	if err != nil {
		return nil, fmt.Errorf("push: teleport SSH-handshake с %s: %w", addr, err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}
