// Package bootstrap implements Soul-side onboarding per [ADR-012(b)].
//
// `soul init` (entrypoint in cmd/soul), in order:
//
//  1. Determines SID (explicit --sid or os.Hostname).
//  2. Takes the RSA key of an unfinished earlier attempt, or generates one,
//     and builds a PKCS#10 CSR (CN = SID) for it — see [PendingKeyFile].
//  3. Connects to one of the Keeper Bootstrap endpoints
//     (server-only TLS, `keeper.tls.ca` from soul.yml).
//  4. Calls the unary Bootstrap RPC with (sid, plain-token, csr_pem).
//  5. Writes (cert.pem, key.pem, ca.pem) to `paths.seed` via seed.Write, then
//     drops the pending key.
//
// The private key never leaves the host (ADR-012(b)); the CSR carries only
// the public key, and the token is hashed server-side.
//
// [ADR-012(b)]: docs/adr/0012-keeper-soul-grpc.md
package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/tlsx"
	"github.com/souls-guild/soul-stack/soul/internal/seed"
)

// RSA key size for the Soul side. 2048 is the industry minimum, compatible
// with most Vault PKI roles; a single point to change if policy tightens.
const rsaKeySize = 2048

// sidRe is the canonical SID form (= FQDN), kept in sync with
// `keeper/internal/soul.SIDPattern`. Duplicated here so the Soul side
// doesn't pull in Keeper's internal package.
var sidRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,253}$`)

// ValidSID is a surface-level SID check on the Soul side before the
// round-trip. The server side still validates via its own regex + PG CHECK.
func ValidSID(sid string) bool { return sidRe.MatchString(sid) }

// Config is the input to bootstrap.Run.
type Config struct {
	// SID is the explicit SID; empty → os.Hostname lower-cased.
	SID string
	// Token is the one-time bootstrap token (plain). Never log it.
	Token string
	// SeedDir is `paths.seed` from soul.yml. Must not be empty.
	SeedDir string
	// KeeperCA is the path to Keeper's CA bundle (`keeper.tls.ca` from
	// soul.yml). Used to verify the server certificate during the
	// server-only TLS handshake.
	KeeperCA string
	// Endpoints is an ordered (by priority) list of Keeper Bootstrap
	// addresses (`host:bootstrap_port`), extracted from `keeper.endpoints[]`
	// via SoulKeeperEndpoint.BootstrapAddr(). Tried in order until the first
	// success; failback doesn't apply to bootstrap (one-shot).
	Endpoints []string
	// HandshakeTimeout is the window for one RPC. Default 10s.
	HandshakeTimeout time.Duration
	// SoulVersion is the soul binary version, for onboarding audit.
	// An empty string is fine.
	SoulVersion string
}

// Result is the outcome of a successful onboarding.
type Result struct {
	SID      string
	Endpoint string
	KID      string
	NotAfter time.Time
	SeedDir  string
	// ReusedKey reports that this run picked up the key an earlier attempt left
	// behind (see [PendingKeyFile]).
	//
	// It says the earlier attempt did not finish — NOT that it reached the
	// Keeper. A key is left behind by a refused token and by a Keeper that
	// never answered alike, and only the server can tell those apart. Do not
	// render it as "the token was already burned".
	ReusedKey bool

	// PendingKeyLeft is non-nil when the scratch key could not be removed after
	// a SUCCESSFUL onboarding (read-only `paths.seed`, an LSM denying unlink).
	// Reported rather than returned as an error, because the seed is written
	// and the token is spent by then: this is a file to clean up, not a
	// bootstrap to retry.
	PendingKeyLeft error
}

// Run executes the full bootstrap cycle.
//
// Retrying is safe and is the supported recovery. The Keeper burns the token
// when it signs the certificate, so a reply lost in flight leaves the token
// spent — but a repeat presentation carrying the SAME key (which Run does,
// via [PendingKeyFile]) is recognized as the same onboarding and completes it.
// A repeat carrying a different key, or one made after the host has connected
// even once, is still PermissionDenied: what may not be repeated is binding
// the SID to a key. See docs/adr/0090-bootstrap-reply-loss-recovery.md.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("bootstrap: token is empty")
	}
	if cfg.SeedDir == "" {
		return nil, errors.New("bootstrap: seed_dir is empty (set paths.seed in soul.yml)")
	}
	if cfg.KeeperCA == "" {
		return nil, errors.New("bootstrap: keeper.tls.ca is empty in soul.yml")
	}
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("bootstrap: keeper.endpoints is empty in soul.yml")
	}

	sid := cfg.SID
	if sid == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("bootstrap: detect hostname: %w", err)
		}
		sid = strings.ToLower(strings.TrimSpace(host))
	}
	if !ValidSID(sid) {
		return nil, fmt.Errorf("bootstrap: invalid sid %q (must match %s)", sid, sidRe.String())
	}

	// Key+CSR before opening the network. The CSR carries the public key +
	// CN=SID; the private key goes to `paths.seed/pending-key.pem` first and
	// into the seed version only on success.
	//
	// It is persisted, and a retry REUSES it, because the Keeper burns the
	// bootstrap token when it signs — not when the reply lands. A reply lost to
	// this client's deadline leaves a token that only the key that burn bound
	// can redeem, so an attempt carrying a fresh key would be indistinguishable
	// from a second host claiming the same SID and is refused as one. See
	// [PendingKeyFile] for why a certificate-less key on disk is an acceptable
	// price (NIM-865).
	key, csrPEM, reusedKey, err := loadOrCreatePendingKey(cfg.SeedDir, sid)
	if err != nil {
		return nil, err
	}

	timeout := cfg.HandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	tlsCfg, err := tlsx.LoadClientTLS(tlsx.ClientConfig{
		CAPath: cfg.KeeperCA,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: load client TLS: %w", err)
	}

	var (
		reply       *keeperv1.BootstrapReply
		successAddr string
		dialErrs    []string
	)
	for _, addr := range cfg.Endpoints {
		// ServerName is set per-endpoint: the gRPC target is host:port —
		// only the host is needed for SNI / hostname-verify.
		cfgForAddr := tlsCfg.Clone()
		if h, ok := hostFromAddr(addr); ok {
			cfgForAddr.ServerName = h
		}
		r, err := dialAndBootstrap(ctx, bootstrapAttempt{
			addr:        addr,
			tlsCfg:      cfgForAddr,
			sid:         sid,
			token:       cfg.Token,
			csrPEM:      csrPEM,
			soulVersion: cfg.SoulVersion,
			timeout:     timeout,
		})
		if err == nil {
			reply = r
			successAddr = addr
			break
		}
		dialErrs = append(dialErrs, fmt.Sprintf("%s: %v", addr, err))
	}
	if reply == nil {
		return nil, fmt.Errorf("bootstrap: all endpoints failed:\n  - %s",
			strings.Join(dialErrs, "\n  - "))
	}

	// caChainPem from BootstrapReply is the PKI CA chain Soul will use to
	// verify the server certificate on subsequent mTLS. We save this one,
	// not `keeper.tls.ca` (one chain covers the whole cluster, refreshed
	// through rotation via the same bootstrap).
	//
	// The sigil_pubkey trust anchor for plugin permission signing (ADR-026,
	// S2b/R3) is optional. Priority set > single (ADR-026(h)): a non-empty
	// sigil_pubkey_pem_set (field 6, multi-anchor for gapless rotation)
	// outranks the single sigil_pubkey_pem (field 5, legacy). Both empty =
	// Sigil not configured on Keeper → SigilPubKeyPEM stays nil, no file is
	// written, plugin verify is off. Persisted in the same seed version as
	// cert/key/ca — survives a restart (pull-mode verify in S6 without
	// bootstrap).
	material := &seed.Material{
		CertPEM: reply.GetCertificatePem(),
		KeyPEM:  encodeRSAPrivateKeyPEM(key),
		CAPEM:   reply.GetCaChainPem(),
	}
	// Empty trust anchor (Sigil off on Keeper) → nil, not []byte{} —
	// consistent with seed.Load (nil = "off"), so S6 verify doesn't need to
	// distinguish nil from an empty anchor set.
	if anchors := sigilAnchorsPEM(reply); len(anchors) > 0 {
		material.SigilPubKeyPEM = anchors
	}
	if err := seed.Write(cfg.SeedDir, material); err != nil {
		return nil, err
	}
	// Only now: the seed is on disk and the retry this key existed for can no
	// longer be needed. Removing it before the write would reopen the window.
	//
	// Not fatal. The certificate is written and the token is spent, so failing
	// here would report a failed onboarding for one that succeeded — and the
	// operator's natural response, re-running `soul init`, would be a second
	// presentation. Surfaced on the Result instead, for whoever prints it.
	pendingKeyErr := clearPendingKey(cfg.SeedDir)

	res := &Result{
		SID:            sid,
		Endpoint:       successAddr,
		KID:            reply.GetKid(),
		SeedDir:        cfg.SeedDir,
		ReusedKey:      reusedKey,
		PendingKeyLeft: pendingKeyErr,
	}
	if reply.GetNotAfter() != nil {
		res.NotAfter = reply.GetNotAfter().AsTime()
	}
	return res, nil
}

// bootstrapAttempt carries the inputs of a single dial. The fields are keyed
// rather than positional because sid and token are both strings and sat next
// to each other in the old signature: swapping them compiled, and the result
// would have put the bootstrap token in the field Keeper echoes into its logs
// while the SID went to the field compared as a secret.
type bootstrapAttempt struct {
	addr        string
	tlsCfg      *tls.Config
	sid         string
	token       string
	csrPEM      []byte
	soulVersion string
	timeout     time.Duration
}

// dialAndBootstrap is one attempt: gRPC dial + Bootstrap RPC + close.
func dialAndBootstrap(ctx context.Context, at bootstrapAttempt) (*keeperv1.BootstrapReply, error) {
	creds := credentials.NewTLS(at.tlsCfg)
	conn, err := grpc.NewClient(at.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	rpcCtx, cancel := context.WithTimeout(ctx, at.timeout)
	defer cancel()

	client := keeperv1.NewKeeperClient(conn)
	reply, err := client.Bootstrap(rpcCtx, &keeperv1.BootstrapRequest{
		Sid:            at.sid,
		BootstrapToken: at.token,
		CsrPem:         at.csrPEM,
		SoulVersion:    at.soulVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("Bootstrap RPC: %w", err)
	}
	if len(reply.GetCertificatePem()) == 0 || len(reply.GetCaChainPem()) == 0 {
		return nil, errors.New("Bootstrap reply incomplete (missing certificate_pem or ca_chain_pem)")
	}
	return reply, nil
}

// generateKeyAndCSR creates an RSA key and PKCS#10 CSR with CN=sid.
func generateKeyAndCSR(sid string) (*rsa.PrivateKey, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaKeySize)
	if err != nil {
		return nil, nil, fmt.Errorf("bootstrap: generate rsa key: %w", err)
	}
	csrPEM, err := createCSR(key, sid)
	if err != nil {
		return nil, nil, err
	}
	return key, csrPEM, nil
}

// createCSR builds the PKCS#10 CSR (CN=sid) for a key that already exists —
// the retry path, where reusing the previous attempt's key is the whole point
// (see [PendingKeyFile]). The CSR is rebuilt rather than stored beside the key:
// it is derived, and one stored copy that could disagree with the key is a
// worse failure than the few milliseconds it costs to re-derive.
func createCSR(key *rsa.PrivateKey, sid string) ([]byte, error) {
	tmpl := x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: sid},
		DNSNames: []string{sid},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: create CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// sigilAnchorsPEM extracts Sigil trust anchors from BootstrapReply by
// priority set > single (ADR-026(h)): a non-empty sigil_pubkey_pem_set
// (field 6) is authoritative, and the single sigil_pubkey_pem (field 5) is
// ignored in that case. The set is assembled by concatenating PEM blocks
// (the same way the sigil_pubkey.pem seed file stores them and
// seed.ParseSigilPubKeys parses them). Both sources empty → nil (Sigil off).
//
// Each set element is normalized with a trailing newline so the
// concatenation is valid multi-PEM (pem.Decode requires block boundaries).
func sigilAnchorsPEM(reply *keeperv1.BootstrapReply) []byte {
	if set := reply.GetSigilPubkeyPemSet(); len(set) > 0 {
		var buf []byte
		for _, p := range set {
			if p == "" {
				continue
			}
			buf = append(buf, p...)
			if p[len(p)-1] != '\n' {
				buf = append(buf, '\n')
			}
		}
		return buf
	}
	if single := reply.GetSigilPubkeyPem(); single != "" {
		return []byte(single)
	}
	return nil
}

// encodeRSAPrivateKeyPEM returns the PKCS#1 PEM form of the key.
func encodeRSAPrivateKeyPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// hostFromAddr extracts the host from `host:port` (IPv4 / FQDN). IPv6
// without brackets and formless hosts return (s, false). Sufficient for
// ServerName.
func hostFromAddr(s string) (string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", false
	}
	host := s[:i]
	if strings.Contains(host, ":") {
		// IPv6 without brackets — TLS ServerName would be useless anyway.
		return "", false
	}
	return host, true
}
