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

// TeleportDialerConfig — Teleport creds for the by-name bootstrap transport
// (ADR-063 amendment "Teleport by-name transport"). Source — the keeper.yml
// push block (`push.transport: teleport` + `push.teleport.*`), NOT a plugin:
// in teleport mode delivery goes entirely through the Teleport identity
// file, and the soul-ssh-teleport plugin doesn't participate in this flow.
type TeleportDialerConfig struct {
	// ProxyAddr — `host:port` of the Teleport Proxy (gRPC sshgrpc listener,
	// usually `<proxy>:443`). Required.
	ProxyAddr string
	// IdentityFile — path to the Teleport identity file (issued by
	// `tctl auth sign` for a bot/role with access to target nodes). Carries
	// TLS cert+key for mTLS to the Proxy AND an SSH user-cert + known_hosts
	// (host-CA) for the target handshake. Required.
	IdentityFile string
	// Cluster — the name of the Teleport cluster where node names resolve.
	// Required (passed to DialHost as `cluster`).
	Cluster string
	// UseSystemTrust switches proxy-server-cert verification to mTLS-gRPC to
	// the Proxy (ADR-063 amendment "Teleport proxy behind an L7 TLS load
	// balancer"). Optional, default false = current behavior bit-for-bit.
	//
	// false: the proxy cert is verified via the identity-CA pool from
	// creds.TLSConfig() + sentinel ServerName `teleport.cluster.local`
	// (Teleport API client). Valid when the proxy presents a Teleport-issued
	// cert.
	//
	// true: the proxy sits BEHIND a public L7 TLS load balancer that presents
	// its own public cert (e.g. `*.teleport.example.com`, no `teleport.cluster.local`
	// SAN). RootCAs is zeroed → Go uses the system trust store, ServerName is
	// set to the host from ProxyAddr → the load balancer's public cert is
	// verified normally. The client cert (mTLS auth to the proxy) is KEPT.
	// This is NOT InsecureSkipVerify: cert verification isn't disabled, it's
	// just shifted to system trust + the real host — the proxy's server cert
	// isn't Soul Stack's trust boundary (auth = client-mTLS + SSH host-CA
	// from the identity).
	UseSystemTrust bool
	// AlpnUpgrade wraps the gRPC connection to the Proxy in an ALPN
	// conn-upgrade (a WebSocket tunnel on `/webapi/connectionupgrade`),
	// ADR-063 amendment "Teleport proxy behind an L7 TLS load balancer".
	// Optional, default false = current behavior bit-for-bit.
	//
	// Needed when the Proxy sits BEHIND an L7 TLS load balancer that
	// terminates TLS and does NOT proxy raw gRPC/SSH streams (returns HTTP
	// instead of gRPC → DialHost fails with `403 / content-type
	// "text/plain"`). The ALPN wrapper tunnels the stream over HTTP, which
	// the L7 LB passes through.
	//
	// With alpn-upgrade, the connection is TWO independent TLS layers:
	//   - the OUTER TLS to the LB (websocket-upgrade) — hardcoded in the
	//     vendor to system trust + ServerName=host(proxy_addr); our
	//     TLSConfigFunc doesn't reach here, the load balancer's public cert
	//     is verified via system trust.
	//   - the INNER gRPC-mTLS to the Teleport proxy over the tunnel — built
	//     from our TLSConfigFunc. The proxy presents a Teleport-CA-issued
	//     cert (SAN `teleport.cluster.local`), so the inner layer gets a
	//     MERGED trust: identity-CA-pool ∪ system (applied by
	//     applyProxyTLSTrustALPN).
	//
	// alpn mode sets the inner trust ITSELF → UseSystemTrust is ignored when
	// AlpnUpgrade=true (the alpn branch takes priority). Teleport adds
	// WithALPNConnUpgradePing itself inside newDialerForGRPCClient.
	//
	// NextProtos is reset in buildProxyClientConfig — see the comment there.
	AlpnUpgrade bool
}

// NewTeleportDialer builds a [Dialer] that opens an SSH session to a target
// THROUGH the Teleport Proxy BY-NAME (target = node-name = SID/FQDN, NOT IP).
//
// Why a separate path from [Dial]: the generic ProxyJump ([dialViaProxy])
// reaches the target by IP through the bastion's direct-tcpip channel — this
// is incompatible with Teleport (proven live: `tsh ssh root@<IP>` → node
// offline, `root@<FQDN>` → ok; Teleport addresses nodes by their registered
// name, not by IP).
//
// Auth model (a), ADR-063 amendment: transport + user-auth + host-verify are
// done ENTIRELY through the Teleport identity file:
//   - mTLS-gRPC to the Proxy — from creds.TLSConfig() (client-cert + Proxy-CA
//     pool);
//   - DialHost(node-name, cluster) — the Proxy resolves the node by name and
//     opens an SSH tunnel;
//   - target SSH handshake — creds.SSHClientConfig(): PublicKeyAuth.Signers
//     (user-cert from the Teleport SSH CA) + HostKeyCallback (known_hosts
//     from the same identity, host-verify via the Teleport host-CA).
//
// ★ The [DialConfig] fields Auth / HostAuthorities / ProxyJump /
// ProxyHostAuthority are IGNORED in teleport mode: all authentication and
// host-verify come from the identity file, not from SignReply/Vault-host-CA.
// The caller (core.bootstrap.delivered on the teleport branch) doesn't even
// fill them in — it skips Authorize/Sign/ephemeral. Only [DialConfig.Host]
// (= SID, node-name), [DialConfig.Port], [DialConfig.User], and
// [DialConfig.Timeout] are used.
//
// Connect is lazy per-Dial (a new proxy.Client per session): token delivery
// is a rare one-shot operation (a handful of VMs per cloud-create run),
// holding a long-lived proxy client isn't justified. proxy.Client lives as
// long as the SSH session (the target conn is a multiplex over its gRPC
// stream), closed in Session.Close() — see [teleportSession].
func NewTeleportDialer(cfg TeleportDialerConfig) (Dialer, error) {
	if cfg.ProxyAddr == "" {
		return nil, errors.New("push: NewTeleportDialer: proxy_addr is empty")
	}
	if cfg.IdentityFile == "" {
		return nil, errors.New("push: NewTeleportDialer: identity_file is empty")
	}
	if cfg.Cluster == "" {
		return nil, errors.New("push: NewTeleportDialer: cluster is empty")
	}

	// Preflight: confirm once that the identity file exists and parses
	// (TLSConfig() AND SSHClientConfig() both come up). Without this, a
	// missing/broken file would only surface on the first Dial (too late —
	// on every VM in the run); caught at keeper startup →
	// fail-closed via buildBootstrapTeleportDialer → errSetupFailed.
	//
	// ★ The loaded creds are NOT cached for the Dial calls themselves:
	// identity expires (~8-12h), the operator reissues the file, and Dial
	// must read the FRESH one — hence preflight load+check+discard, while the
	// Dial closure below does its own LoadIdentityFile on every call.
	// apiclient.LoadIdentityFile is lazy (the object reads the file on the
	// first TLSConfig/SSHClientConfig call), and the preflight object here
	// isn't shared with the Dial closure.
	preflight := apiclient.LoadIdentityFile(cfg.IdentityFile)
	if _, err := preflight.TLSConfig(); err != nil {
		return nil, fmt.Errorf("push: NewTeleportDialer: identity %q: TLS-config: %w", cfg.IdentityFile, err)
	}
	if _, err := preflight.SSHClientConfig(); err != nil {
		return nil, fmt.Errorf("push: NewTeleportDialer: identity %q: SSH-client-config: %w", cfg.IdentityFile, err)
	}

	// With UseSystemTrust (non-alpn branch), the proxy-cert ServerName = the
	// host from ProxyAddr (no port). Resolved once at startup: a broken
	// proxy_addr (no `:port`) is a constructor error via
	// buildBootstrapTeleportDialer → errSetupFailed, not a late Dial failure.
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
			// alpn mode: the inner gRPC-mTLS goes to the real Teleport proxy
			// (Teleport-CA cert, SAN `teleport.cluster.local`) — a merged trust
			// is needed. RootCAs from creds.TLSConfig() is opaque (certs can't
			// be pulled back out), so the RAW identity-CA is taken from the
			// identity file directly: CACerts.TLS is the same PEM blocks that
			// apiclient.LoadIdentityFile puts into RootCAs under the hood
			// (identityfile.ReadFile → idFile.TLSConfig()).
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

		// ★ target = node-name = SID (NOT primary_ip). The Proxy resolves the
		// node by name in the Teleport registry; an IP here would lead to
		// 'offline or does not exist'.
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

		// Ownership of proxyClient transfers to the session (closed in
		// teleportSession.Close() after client): an early Close would kill the
		// gRPC stream under conn — see teleportSession.
		return newTeleportSession(&sshSession{client: client}, proxyClient), nil
	}, nil
}

// buildProxyClientConfig assembles a proxy.ClientConfig for a single Dial.
// Pulled out as a pure function (no network) for the guard test:
// proxy.NewClient requires a live Teleport network, while this only checks
// the field mapping from TeleportDialerConfig to proxy.ClientConfig — in
// particular ALPNConnUpgradeRequired (ADR-063 amendment "Teleport proxy
// behind an L7 TLS load balancer").
//
// AlpnUpgrade=true → ALPNConnUpgradeRequired:true (an ALPN-conn-upgrade
// WebSocket tunnel for an L7 LB in front of the Proxy). Teleport turns on
// WithALPNConnUpgradePing itself inside newDialerForGRPCClient — not
// duplicated here. Other fields (TLSRoutingEnabled / InsecureSkipVerify) are
// NOT touched: the former only affects the path to Auth, not DialHost; the
// latter would disable public-cert verification (an MITM hole).
func buildProxyClientConfig(cfg TeleportDialerConfig, tlsConfig *tls.Config, sshConfig apissh.ClientConfig, dialTimeout time.Duration) proxy.ClientConfig {
	// ★ h2-first ALPN (from creds.TLSConfig()) sends the TLS-routing proxy into the web stack → 403; resetting it — the vendor adds teleport-proxy-ssh-grpc, grpc-go appends h2 at the end (the tsh ordering).
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

// applyProxyTLSTrust configures proxy-server-cert verification for mTLS-gRPC
// to the Teleport Proxy (ADR-063 amendment "Teleport proxy behind an L7 TLS
// load balancer").
//
// useSystemTrust=false (default): tls.Config isn't touched — the proxy cert
// is verified via the identity-CA pool (RootCAs from creds.TLSConfig()) +
// sentinel ServerName `teleport.cluster.local`, set by the Teleport API
// client itself.
//
// useSystemTrust=true: the proxy sits behind a public L7 TLS load balancer
// that proxies raw gRPC as-is. RootCAs=nil → Go uses the system trust store
// (verifies the load balancer's public cert); ServerName=proxyHost drops the
// sentinel `teleport.cluster.local` (not in the load balancer's SAN).
// Certificates/GetClientCertificate (the mTLS client cert for proxy auth)
// are NOT touched — without them the connect is rejected. This is NOT
// InsecureSkipVerify: cert verification is kept, just shifted to system
// trust + the real host.
//
// Not called when AlpnUpgrade=true: the alpn branch sets the inner trust
// itself (applyProxyTLSTrustALPN), UseSystemTrust is ignored — see
// TeleportDialerConfig.AlpnUpgrade.
//
// Pulled out as a separate pure function for the guard test:
// applyProxyTLSTrust is tested directly (a live Teleport network for
// proxy.NewClient isn't available in the test).
func applyProxyTLSTrust(tlsConfig *tls.Config, useSystemTrust bool, proxyHost string) {
	if !useSystemTrust {
		return
	}
	tlsConfig.RootCAs = nil
	tlsConfig.ServerName = proxyHost
}

// applyProxyTLSTrustALPN configures the INNER gRPC-mTLS layer for
// alpn-conn-upgrade (ADR-063 amendment "Teleport proxy behind an L7 TLS load
// balancer", decision "a" — merged trust).
//
// With alpn, the inner layer terminates at the real Teleport proxy, which
// presents a Teleport-CA-issued cert (SAN `teleport.cluster.local`). So the
// inner trust knows BOTH the Teleport-CA (from identity) AND public CAs (in
// case a public cert shows up behind the tunnel), RootCAs = system pool ∪
// identity-CA:
//   - the system pool is a clone of x509.SystemCertPool() (empty if nil);
//   - identity-CA is added from the RAW PEM blocks in identityCAPEM
//     (CACerts.TLS from the identity file — the same CAs that
//     creds.TLSConfig() puts into RootCAs, but in a transparent form
//     suitable for merging).
//
// ServerName is NOT touched — stays the sentinel `teleport.cluster.local`
// from creds.TLSConfig() (the inner layer goes to the real Teleport proxy
// with this SAN). Certificates/GetClientCertificate (the mTLS client cert)
// are NOT touched. This is NOT InsecureSkipVerify: cert verification is
// kept, only trust is widened.
//
// Pulled out as a separate pure function for the guard test (a live
// Teleport network for proxy.NewClient isn't available in the test);
// identity-CA is passed as a parameter rather than read from the FS, so the
// test doesn't depend on a file.
func applyProxyTLSTrustALPN(tlsConfig *tls.Config, identityCAPEM [][]byte) error {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, caPEM := range identityCAPEM {
		if !pool.AppendCertsFromPEM(caPEM) {
			return fmt.Errorf("alpn-trust: invalid identity-CA PEM")
		}
	}
	tlsConfig.RootCAs = pool
	return nil
}

// teleportSession — a Session that owns the proxy.Client. The conn from
// DialHost isn't an independent socket, it's a multiplex over this same
// proxy client's gRPC stream (streamutils.NewConn over ProxySSH): closing
// proxyClient before the SSH client kills the transport (the first
// NewSession → `ssh: unexpected packet in response to channel open: <nil>`).
// So proxyClient lives until the session's Close().
type teleportSession struct {
	Session
	proxyClient io.Closer
}

func newTeleportSession(sess Session, proxyClient io.Closer) Session {
	return &teleportSession{Session: sess, proxyClient: proxyClient}
}

// Close closes the SSH client first, the proxy client after. Idempotent.
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

// newTeleportSSHClient runs the SSH handshake to the target over the
// already-open Teleport tunnel (conn from DialHost). User-auth and
// host-verify come from proxyClient.SSHConfig(user) — the same identity as
// the proxy client's.
func newTeleportSSHClient(ctx context.Context, conn net.Conn, addr, user string, proxyClient *proxy.Client) (*ssh.Client, error) {
	sshClientConfig := proxyClient.SSHConfig(user)
	sshConn, chans, reqs, err := apissh.NewClientConn(ctx, conn, addr, sshClientConfig)
	if err != nil {
		return nil, fmt.Errorf("push: teleport SSH handshake with %s: %w", addr, err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}
