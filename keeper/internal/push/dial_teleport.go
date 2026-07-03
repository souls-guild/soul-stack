package push

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proxy"
	"github.com/gravitational/teleport/api/identityfile"
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
	// UseSystemTrust меняет верификацию proxy-server-cert на mTLS-gRPC к Proxy
	// (ADR-063 amendment «Teleport-proxy за L7-TLS-балансировщиком»). Опционально,
	// default false = текущее поведение бит-в-бит.
	//
	// false: proxy-cert верифицируется через identity-CA-pool из creds.TLSConfig()
	// + sentinel-ServerName `teleport.cluster.local` (Teleport API client). Валидно,
	// когда proxy презентует Teleport-issued cert.
	//
	// true: proxy стоит ЗА публичным L7-TLS-балансировщиком, который презентует
	// собственный публичный cert (напр. `*.tp.rwb.ru`, без SAN `teleport.cluster.local`).
	// RootCAs обнуляется → Go берёт системный trust store, ServerName ставится в host
	// из ProxyAddr → публичный cert балансировщика верифицируется штатно. Client-cert
	// (mTLS-auth на proxy) СОХРАНЯЕТСЯ. Это НЕ InsecureSkipVerify: верификация cert не
	// отключается, лишь смещается на системный trust + реальный host — server-cert
	// proxy не граница доверия Soul Stack (auth = client-mTLS + SSH host-CA из identity).
	UseSystemTrust bool
	// AlpnUpgrade оборачивает gRPC-коннект к Proxy в ALPN-conn-upgrade
	// (WebSocket-туннель на `/webapi/connectionupgrade`), ADR-063 amendment
	// «Teleport-proxy за L7-TLS-балансировщиком». Опционально, default false =
	// текущее поведение бит-в-бит.
	//
	// Нужен, когда Proxy стоит ЗА L7-TLS-балансировщиком, который терминирует TLS
	// и НЕ проксирует raw gRPC/SSH-stream (отдаёт HTTP вместо gRPC → DialHost
	// падает на `403 / content-type "text/plain"`). ALPN-обёртка туннелирует
	// stream поверх HTTP, который L7-LB пропускает.
	//
	// При alpn-upgrade соединение = ДВА независимых TLS-слоя:
	//   - ВНЕШНИЙ TLS к LB (websocket-upgrade) — захардкожен в вендоре на
	//     системный trust + ServerName=host(proxy_addr); наш TLSConfigFunc сюда
	//     не доходит, публичный cert балансировщика верифицируется системным trust.
	//   - ВНУТРЕННИЙ gRPC-mTLS к Teleport-proxy поверх туннеля — строится из
	//     нашего TLSConfigFunc. Proxy предъявляет Teleport-CA-issued cert (SAN
	//     `teleport.cluster.local`), поэтому внутренний слой получает ОБЪЕДИНЁННЫЙ
	//     trust: identity-CA-pool ∪ системный (применяется applyProxyTLSTrustALPN).
	//
	// alpn-режим САМ задаёт внутренний trust → UseSystemTrust при AlpnUpgrade=true
	// игнорируется (alpn-ветка приоритетна). Teleport сам добавляет
	// WithALPNConnUpgradePing внутри newDialerForGRPCClient.
	//
	// NextProtos сбрасывается в buildProxyClientConfig — см. комментарий там.
	AlpnUpgrade bool
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
// удержание долгоживущего proxy-клиента не оправдано. proxy.Client живёт
// столько же, сколько SSH-сессия (target-conn — мультиплекс поверх его
// gRPC-стрима), закрывается в Session.Close() — см. [teleportSession].
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

	// При UseSystemTrust ServerName proxy-cert = host из ProxyAddr (без порта).
	// Резолвим один раз на старте: битый proxy_addr (нет `:port`) — конструктор-
	// ошибка через buildBootstrapTeleportDialer → errSetupFailed, не поздний Dial.
	var proxyHost string
	if cfg.UseSystemTrust {
		h, _, err := net.SplitHostPort(cfg.ProxyAddr)
		if err != nil {
			return nil, fmt.Errorf("push: NewTeleportDialer: proxy_addr %q: %w", cfg.ProxyAddr, err)
		}
		proxyHost = h
	}

	return func(ctx context.Context, dialCfg DialConfig) (Session, error) {
		creds := apiclient.LoadIdentityFile(cfg.IdentityFile)

		tlsConfig, err := creds.TLSConfig()
		if err != nil {
			return nil, fmt.Errorf("push: teleport identity %q: TLS-config: %w", cfg.IdentityFile, err)
		}
		if cfg.AlpnUpgrade {
			// alpn-режим: внутренний gRPC-mTLS идёт к настоящему Teleport-proxy
			// (Teleport-CA cert, SAN `teleport.cluster.local`) — нужен объединённый
			// trust. RootCAs из creds.TLSConfig() непрозрачен (сертификаты обратно
			// не достать), поэтому RAW identity-CA берём из identity-file напрямую:
			// CACerts.TLS — те же PEM-блоки, что apiclient.LoadIdentityFile кладёт в
			// RootCAs под капотом (identityfile.ReadFile → idFile.TLSConfig()).
			idFile, ierr := identityfile.ReadFile(cfg.IdentityFile)
			if ierr != nil {
				return nil, fmt.Errorf("push: teleport identity %q: read identity-CA: %w", cfg.IdentityFile, ierr)
			}
			if terr := applyProxyTLSTrustALPN(tlsConfig, idFile.CACerts.TLS); terr != nil {
				return nil, fmt.Errorf("push: teleport identity %q: %w", cfg.IdentityFile, terr)
			}
		} else {
			applyProxyTLSTrust(tlsConfig, cfg.UseSystemTrust, proxyHost)
		}
		sshConfig, err := creds.SSHClientConfig()
		if err != nil {
			return nil, fmt.Errorf("push: teleport identity %q: SSH-client-config: %w", cfg.IdentityFile, err)
		}

		proxyClient, err := proxy.NewClient(ctx, buildProxyClientConfig(cfg, tlsConfig, sshConfig, dialCfg.Timeout))
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

		// Ownership proxyClient переходит сессии (закрытие — в teleportSession.Close()
		// после client): ранний Close убил бы gRPC-стрим под conn — см. teleportSession.
		return newTeleportSession(&sshSession{client: client}, proxyClient), nil
	}, nil
}

// buildProxyClientConfig собирает proxy.ClientConfig для одного Dial-а. Выделено
// чистой функцией (без сети) ради guard-теста: proxy.NewClient требует живой
// Teleport-сети, а здесь проверяется лишь маппинг полей TeleportDialerConfig в
// proxy.ClientConfig — в частности ALPNConnUpgradeRequired (ADR-063 amendment
// «Teleport-proxy за L7-TLS-балансировщиком»).
//
// AlpnUpgrade=true → ALPNConnUpgradeRequired:true (ALPN-conn-upgrade WebSocket-
// туннель для L7-LB перед Proxy). WithALPNConnUpgradePing Teleport включает сам
// внутри newDialerForGRPCClient — здесь не дублируется. Прочие поля
// (TLSRoutingEnabled / InsecureSkipVerify) НЕ трогаются: первое влияет только на
// путь к Auth, не на DialHost; второе отключило бы верификацию публичного cert
// (MITM-дыра).
func buildProxyClientConfig(cfg TeleportDialerConfig, tlsConfig *tls.Config, sshConfig apissh.ClientConfig, dialTimeout time.Duration) proxy.ClientConfig {
	// ★ h2-first ALPN (из creds.TLSConfig()) уводит TLS-routing proxy в web-стек → 403; сброс — vendor добавит teleport-proxy-ssh-grpc, grpc-go допишет h2 в конец (порядок tsh).
	tlsConfig.NextProtos = nil
	return proxy.ClientConfig{
		ProxyAddress: cfg.ProxyAddr,
		SSHConfig:    sshConfig,
		TLSConfigFunc: func(string) (*tls.Config, error) {
			return tlsConfig, nil
		},
		DialTimeout:             dialTimeout,
		ALPNConnUpgradeRequired: cfg.AlpnUpgrade,
	}
}

// applyProxyTLSTrust настраивает верификацию proxy-server-cert на mTLS-gRPC к
// Teleport Proxy (ADR-063 amendment «Teleport-proxy за L7-TLS-балансировщиком»).
//
// useSystemTrust=false (дефолт): tls.Config не трогается — proxy-cert
// верифицируется через identity-CA-pool (RootCAs из creds.TLSConfig()) + sentinel-
// ServerName `teleport.cluster.local`, проставленные самим Teleport API client.
//
// useSystemTrust=true: proxy за публичным L7-TLS-балансировщиком. RootCAs=nil → Go
// берёт системный trust store (верифицирует публичный балансировщик-cert);
// ServerName=proxyHost снимает sentinel `teleport.cluster.local` (его в SAN
// балансировщика нет). Certificates/GetClientCertificate (mTLS client-cert для
// auth на proxy) НЕ трогаются — без них коннект отвергается. Это НЕ
// InsecureSkipVerify: верификация cert сохранена, лишь смещена на системный trust
// + реальный host.
//
// Выделено отдельной чистой функцией ради guard-теста: applyProxyTLSTrust
// тестируется напрямую (живая Teleport-сеть для proxy.NewClient в тесте недоступна).
func applyProxyTLSTrust(tlsConfig *tls.Config, useSystemTrust bool, proxyHost string) {
	if !useSystemTrust {
		return
	}
	tlsConfig.RootCAs = nil
	tlsConfig.ServerName = proxyHost
}

// applyProxyTLSTrustALPN настраивает ВНУТРЕННИЙ gRPC-mTLS-слой при alpn-conn-
// upgrade (ADR-063 amendment «Teleport-proxy за L7-TLS-балансировщиком», решение
// «a» — объединённый trust).
//
// При alpn внутренний слой упирается в настоящий Teleport-proxy, который
// предъявляет Teleport-CA-issued cert (SAN `teleport.cluster.local`). Чтобы
// внутренний trust знал И Teleport-CA (из identity), И публичные CA (на случай,
// если за туннелем встретится публичный cert), RootCAs = системный пул ∪
// identity-CA:
//   - системный пул берётся клоном x509.SystemCertPool() (если nil — пустой);
//   - identity-CA добавляются из RAW PEM-блоков identityCAPEM (CACerts.TLS
//     identity-file — те же CA, что creds.TLSConfig() кладёт в RootCAs, но в
//     прозрачном виде, пригодном для объединения).
//
// ServerName НЕ трогается — остаётся sentinel `teleport.cluster.local` от
// creds.TLSConfig() (внутренний идёт к настоящему Teleport-proxy с этим SAN).
// Certificates/GetClientCertificate (mTLS client-cert) НЕ трогаются. Это НЕ
// InsecureSkipVerify: верификация cert сохранена, лишь trust расширен.
//
// Выделено отдельной чистой функцией ради guard-теста (живая Teleport-сеть для
// proxy.NewClient в тесте недоступна); identity-CA передаются параметром, а не
// читаются из FS, чтобы тест не зависел от файла.
func applyProxyTLSTrustALPN(tlsConfig *tls.Config, identityCAPEM [][]byte) error {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, caPEM := range identityCAPEM {
		if !pool.AppendCertsFromPEM(caPEM) {
			return fmt.Errorf("alpn-trust: невалидный identity-CA PEM")
		}
	}
	tlsConfig.RootCAs = pool
	return nil
}

// teleportSession — Session, владеющая proxy.Client-ом. conn от DialHost — не
// самостоятельный сокет, а мультиплекс поверх gRPC-стрима этого же proxy-клиента
// (streamutils.NewConn поверх ProxySSH): закрыть proxyClient раньше SSH-client-а
// = убить транспорт (первый NewSession → `ssh: unexpected packet in response to
// channel open: <nil>`). Поэтому proxyClient живёт до Close() сессии.
type teleportSession struct {
	Session
	proxyClient io.Closer
}

func newTeleportSession(sess Session, proxyClient io.Closer) Session {
	return &teleportSession{Session: sess, proxyClient: proxyClient}
}

// Close закрывает SSH-client первым, proxy-клиент после. Идемпотентен.
func (s *teleportSession) Close() error {
	err := s.Session.Close()
	if s.proxyClient != nil {
		if cerr := s.proxyClient.Close(); cerr != nil && err == nil {
			err = cerr
		}
		s.proxyClient = nil
	}
	return err
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
