package push

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Session is the narrow abstraction of a single SSH session that the
// dispatcher needs: run `soul apply`, feed stdin (protojson ApplyRequest),
// and return a stdout reader (NDJSON stream) plus waiting for completion with
// an exit code.
//
// It's an interface (rather than a concrete *sshSession) so [SshDispatcher]
// can be tested without a live sshd: unit tests substitute a mock Session,
// verifying stdin-feed → NDJSON-parse → RunResult. Live-sshd integration is a
// follow-up (docker was busy with another run, see the report).
type Session interface {
	// Run executes the command on the host, feeding stdinData to the
	// process's stdin, and blocks until it finishes. Returns the full stdout
	// and an error:
	//   - nil — the process exited 0;
	//   - *ssh.ExitError — non-zero exit (stdout is still returned: it may
	//     hold NDJSON with a FAILED RunResult).
	// stdout is returned as a string, not a reader: a oneshot `soul apply`
	// writes a bounded amount (an NDJSON stream without long-running
	// progress, ADR-012); streaming parse as data arrives is an S3
	// optimization.
	Run(ctx context.Context, cmd string, stdinData []byte) (stdout string, err error)
	// Close closes the SSH connection. Idempotent.
	Close() error
}

// HostKeyAuthority is the public key of the CA that signed target hosts'
// host certificates (the Vault SSH CA host key). Host-cert verification goes
// against it: a host presenting a cert signed by this CA is trusted; a
// foreign/self-signed one is rejected. Replaces TOFU/known_hosts with
// CA-trust (PM decision S0).
//
// S7-3 introduced multi-CA via [NamedHostKeyAuthority] / [LoadHostCAs]: the
// primary surface is now [DialConfig.HostAuthorities]. `HostKeyAuthority`
// remains as a typed value object (used in the [LoadHostCA] singular helper
// for the backward-compat path) and for `ProxyHostAuthority` (a separate
// proxy CA, without the multi-CA extension).
type HostKeyAuthority struct {
	// CAPublicKey is the public part of the host CA.
	CAPublicKey ssh.PublicKey
}

// DialConfig holds the parameters for opening a single SSH session.
type DialConfig struct {
	// Host is the FQDN/IP of the target host (= the push host's SID).
	Host string
	// Port is the sshd TCP port.
	Port int
	// User is the SSH user to log in as.
	User string
	// Auth holds Keeper's authentication methods on the host (from
	// SignReply: cert+key or an ephemeral keypair). Prepared by the
	// dispatcher from the Sign result.
	//
	// Canonical Teleport flow: the same Auth set (a signed user cert on an
	// ephemeral keypair) is used on BOTH hops — proxy and target. The cert
	// from the Teleport/Vault SSH CA authorizes the user on both sides.
	Auth []ssh.AuthMethod
	// HostAuthorities is the multi-CA set for verifying the target host's
	// host cert (S7-3, ADR-032 amendment 2026-05-26). Must be non-empty; the
	// handshake does an OR check across all elements via
	// ssh.CertChecker.IsHostAuthority. When [ProxyJump] is non-empty and
	// [ProxyHostAuthority] is nil, this same set is also used to verify the
	// proxy hop's host cert.
	HostAuthorities []NamedHostKeyAuthority
	// OnHostCAMatch is an optional callback fired on a host-CA match (for
	// observability: debug-log + the
	// `keeper_push_host_ca_used_total{ca_name=...}` metric). nil means the
	// callback isn't called. Caller is [SshDispatcher]; the live `Dial` flow
	// wires it up itself, and tests may leave it nil.
	OnHostCAMatch func(caName string)
	// ProxyJump is a bastion/proxy in `[user@]host:port` form, through which
	// the SSH tunnel to target runs. An empty string means a direct connect
	// (the S0 flow). Sourced from [pluginv1.SignReply.proxy_jump] from the
	// SshProvider (Teleport).
	//
	// Semantics: the dispatcher opens an SSH client to the proxy_jump host,
	// requests a direct-tcpip channel to [Host]:[Port] on it, and performs a
	// second SSH handshake with target over that channel. This is equivalent
	// to `ssh -J <proxy> <host>`. Authentication uses the same [Auth] on
	// both hops (Teleport flow: one user cert passes through the proxy and
	// authenticates on target).
	ProxyJump string
	// ProxyHostAuthority is a separate CA for host-cert verification on the
	// proxy hop. nil means [HostAuthorities] is used for the proxy too (the
	// typical case: one host CA signs host certs for both proxy and
	// target). Filled in when the operator explicitly separates the
	// proxy CA from the target CA via plugin params.
	ProxyHostAuthority *HostKeyAuthority
	// Timeout is the TCP+handshake phase timeout for the connection. With
	// proxy_jump, it applies to each hop separately (proxy and target).
	Timeout time.Duration
}

// hostCertCallback builds an ssh.HostKeyCallback that trusts ONLY hosts that
// present a host certificate signed by any CA in the set (S7-3 multi-CA OR
// check). A non-cert host key (a bare ed25519/rsa key without a CA
// signature) or a cert from a foreign CA is rejected.
//
// Implemented via ssh.CertChecker:
//   - IsHostAuthority(auth, addr) == true iff auth matches one of the CAs in
//     the set (by marshaled form). CertChecker itself verifies that the
//     presented cert has CertType=HostCert, is signed by that authority, is
//     valid by time, and its principals cover addr.
//   - HostKeyFallback isn't set → if a non-cert key is presented, the check
//     fails (there's no trusted path for a bare host key) — this is the
//     "no TOFU" stance.
//   - `onMatch` (may be nil) is a callback with the matched CA's name, for
//     observability (`keeper_push_host_ca_used_total{ca_name=...}` +
//     debug-log).
//
// Note on hot-path performance: the operator pins the CA set in keeper.yml
// (a closed set of a handful), so a linear bytes.Equal over the marshaled
// form inside the handshake callback doesn't need an index — the handshake
// already does far more system work (crypto/network), and the key
// comparison stays O(n) in the number of CAs in the set.
func hostCertCallback(cas []NamedHostKeyAuthority, onMatch func(caName string)) ssh.HostKeyCallback {
	marshaled := make([][]byte, len(cas))
	names := make([]string, len(cas))
	for i, ca := range cas {
		marshaled[i] = ca.CAPubKey.Marshal()
		names[i] = ca.Name
	}
	checker := &ssh.CertChecker{
		IsHostAuthority: func(auth ssh.PublicKey, _ string) bool {
			authMarshaled := auth.Marshal()
			for i, m := range marshaled {
				if bytesEqual(authMarshaled, m) {
					if onMatch != nil {
						onMatch(names[i])
					}
					return true
				}
			}
			return false
		},
	}
	return checker.CheckHostKey
}

// bytesEqual compares marshaled keys without caring about constant-time (this
// is public data, so there's no timing channel). Split out for the
// callback's readability.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sshSession is the production implementation of [Session] over
// golang.org/x/crypto/ssh.
//
// proxy is an optionally-held client to the bastion (when target is opened
// via a direct-tcpip channel on the proxy). Closed AFTER target, so as not
// to cut off the active channel.
type sshSession struct {
	client *ssh.Client
	proxy  *ssh.Client
}

// Dial opens an SSH connection per cfg with CA-signed host-cert
// verification. Returns a [Session] or a connect/handshake error (including
// a host-cert verification failure — the host presented a cert not from our
// CA, or a bare key).
//
// If cfg.ProxyJump is non-empty, an SSH client to the proxy hop is opened
// first; a direct-tcpip channel to target is requested on it, and a second
// SSH handshake runs over that channel (equivalent to `ssh -J <proxy>
// <host>`). Authentication uses the same cfg.Auth on both hops (Teleport
// flow: one signed user cert).
func Dial(ctx context.Context, cfg DialConfig) (Session, error) {
	if len(cfg.HostAuthorities) == 0 {
		// fail-closed: without a CA there's no trusted path to verify the
		// host key. InsecureIgnoreHostKey is NOT allowed in push (PM
		// decision S0).
		return nil, errors.New("push: HostAuthorities is empty (CA-signed host-cert verification)")
	}
	if cfg.ProxyJump == "" {
		return dialDirect(ctx, cfg)
	}
	return dialViaProxy(ctx, cfg)
}

// dialDirect is a direct connect (proxy_jump empty).
func dialDirect(ctx context.Context, cfg DialConfig) (Session, error) {
	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            cfg.Auth,
		HostKeyCallback: hostCertCallback(cfg.HostAuthorities, cfg.OnHostCAMatch),
		Timeout:         cfg.Timeout,
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("push: TCP-соединение с %s: %w", addr, err)
	}
	client, err := newSSHClient(conn, addr, clientCfg)
	if err != nil {
		_ = conn.Close()
		// A host-cert verification failure lands here too (handshake fail) —
		// this is the normal path for rejecting a foreign/self-signed host
		// key.
		return nil, fmt.Errorf("push: SSH-handshake с %s: %w", addr, err)
	}
	return &sshSession{client: client}, nil
}

// dialViaProxy is the Teleport flow: an SSH client to proxy, a direct-tcpip
// channel to target, a second handshake over that channel. Both hops go
// through CA-signed host-cert verification: proxy via
// cfg.ProxyHostAuthority (or cfg.HostAuthorities when nil); target via
// cfg.HostAuthorities. Authentication uses the same cfg.Auth on both hops.
// Cleanup on a fail-closed error: target is torn down before proxy,
// otherwise the direct-tcpip channel would die before its own ssh channel.
func dialViaProxy(ctx context.Context, cfg DialConfig) (Session, error) {
	proxyUser, proxyAddr, err := parseProxyJump(cfg.ProxyJump, cfg.User)
	if err != nil {
		return nil, fmt.Errorf("push: parse proxy_jump %q: %w", cfg.ProxyJump, err)
	}
	proxyCAs := cfg.HostAuthorities
	if cfg.ProxyHostAuthority != nil {
		if cfg.ProxyHostAuthority.CAPublicKey == nil {
			return nil, errors.New("push: ProxyHostAuthority задан, но CAPublicKey пуст")
		}
		// A separate proxy CA is a singleton set; multi-CA for the proxy hop
		// isn't introduced in MVP (a separate CA bag here covers the "proxy
		// and target have different CA owners" case, not "several proxy
		// CAs").
		proxyCAs = []NamedHostKeyAuthority{{
			Name:     "proxy",
			CAPubKey: cfg.ProxyHostAuthority.CAPublicKey,
		}}
	}

	// Hop 1: TCP+SSH handshake to the proxy_jump host.
	proxyCfg := &ssh.ClientConfig{
		User:            proxyUser,
		Auth:            cfg.Auth,
		HostKeyCallback: hostCertCallback(proxyCAs, cfg.OnHostCAMatch),
		Timeout:         cfg.Timeout,
	}
	d := net.Dialer{Timeout: cfg.Timeout}
	tcpConn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("push: TCP-соединение с proxy %s: %w", proxyAddr, err)
	}
	proxyClient, err := newSSHClient(tcpConn, proxyAddr, proxyCfg)
	if err != nil {
		_ = tcpConn.Close()
		return nil, fmt.Errorf("push: SSH-handshake с proxy %s: %w", proxyAddr, err)
	}

	// Hop 2: direct-tcpip channel proxy → target + second handshake.
	targetAddr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	tunnelConn, err := proxyClient.Dial("tcp", targetAddr)
	if err != nil {
		_ = proxyClient.Close()
		return nil, fmt.Errorf("push: direct-tcpip через proxy %s до %s: %w", proxyAddr, targetAddr, err)
	}
	targetCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            cfg.Auth,
		HostKeyCallback: hostCertCallback(cfg.HostAuthorities, cfg.OnHostCAMatch),
		Timeout:         cfg.Timeout,
	}
	targetClient, err := newSSHClient(tunnelConn, targetAddr, targetCfg)
	if err != nil {
		_ = tunnelConn.Close()
		_ = proxyClient.Close()
		return nil, fmt.Errorf("push: SSH-handshake с target %s через proxy %s: %w", targetAddr, proxyAddr, err)
	}
	return &sshSession{client: targetClient, proxy: proxyClient}, nil
}

// newSSHClient wraps ssh.NewClientConn+ssh.NewClient: a single contract for
// both a direct dial and a tunneled net.Conn (a direct-tcpip channel over
// proxy).
func newSSHClient(conn net.Conn, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// parseProxyJump parses SignReply.proxy_jump as `[user@]host:port`. user is
// optional: when absent, it's inherited from defaultUser (the same one used
// for target — the Teleport canonical flow, one user for both hops).
func parseProxyJump(raw, defaultUser string) (user, addr string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("пустая строка")
	}
	user = defaultUser
	hostPort := raw
	if at := strings.Index(raw, "@"); at >= 0 {
		user = raw[:at]
		hostPort = raw[at+1:]
		if user == "" {
			return "", "", errors.New("user пуст до '@'")
		}
	}
	host, port, splitErr := net.SplitHostPort(hostPort)
	if splitErr != nil {
		return "", "", fmt.Errorf("ожидался host:port: %w", splitErr)
	}
	if host == "" || port == "" {
		return "", "", errors.New("host или port пуст")
	}
	return user, net.JoinHostPort(host, port), nil
}

// Run opens a channel session, feeds stdin, and collects the command's
// stdout.
func (s *sshSession) Run(ctx context.Context, cmd string, stdinData []byte) (string, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("push: открытие SSH-сессии: %w", err)
	}
	defer sess.Close()

	stdinPipe, err := sess.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("push: stdin pipe: %w", err)
	}
	var stdout strings.Builder
	sess.Stdout = &stdout

	if err := sess.Start(cmd); err != nil {
		return "", fmt.Errorf("push: запуск %q: %w", cmd, err)
	}

	// We feed stdin (ApplyRequest protojson) and close it — `soul apply`
	// reads stdin to EOF, otherwise the process won't start the run. For
	// commands without stdin (delivery/cleanup helpers), we close the pipe
	// right away; an EOF/already-closed-channel from Close() in that case is
	// normal, not an error.
	var writeErr error
	if len(stdinData) == 0 {
		_ = stdinPipe.Close()
	} else {
		writeErr = writeAllAndClose(stdinPipe, stdinData)
	}

	// ctx cancellation: we abort the session without waiting for Wait. The
	// soul-side guard and Keeper's barrier treat the drop as a failure
	// (ParseStream will return ErrNoRunResult).
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		_ = sess.Close()
		<-done // wait for the goroutine to finish, don't leak it
		return stdout.String(), fmt.Errorf("push: сессия прервана: %w", ctx.Err())
	case waitErr := <-done:
		if writeErr != nil && waitErr == nil {
			// stdin wasn't delivered, but the process reported 0 — a
			// contradiction; return the write error as primary.
			return stdout.String(), fmt.Errorf("push: подача stdin: %w", writeErr)
		}
		return stdout.String(), waitErr
	}
}

// Close closes target first, then proxy. The reverse order would cut off the
// direct-tcpip channel before its SSH channel on the target side.
// Idempotent: a second call returns nil after the first successful close.
func (s *sshSession) Close() error {
	var firstErr error
	if s.client != nil {
		if err := s.client.Close(); err != nil {
			firstErr = err
		}
		s.client = nil
	}
	if s.proxy != nil {
		if err := s.proxy.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.proxy = nil
	}
	return firstErr
}

// writeAllAndClose writes data to the stdin pipe and closes it (EOF for soul
// apply). Close is always called, even on a write error (otherwise the
// process would hang reading stdin).
func writeAllAndClose(w interface {
	Write([]byte) (int, error)
	Close() error
}, data []byte) error {
	_, werr := w.Write(data)
	cerr := w.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
