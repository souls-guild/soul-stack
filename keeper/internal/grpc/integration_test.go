//go:build integration

// Integration tests for the gRPC Bootstrap RPC via testcontainers
// (postgres:16-alpine + hashicorp/vault:1.18 with PKI). End-to-end:
//
//  1. Bring up PG, run migrations.
//  2. Bring up Vault, provision PKI (mount + root + role).
//  3. Seed operator + soul + bootstrap_token.
//  4. Bring up BootstrapServer on an ephemeral port.
//  5. A gRPC client (TLS, InsecureSkipVerify against the server's
//     self-signed cert) calls Bootstrap; we check: the PEM cert is valid,
//     the fingerprint is recorded, soul → connected, token used_at != NULL.

package grpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	integrationVaultToken = "root"
	integrationVaultImage = "hashicorp/vault:1.18"
	integrationPGImage    = "postgres:16-alpine"
	pkiMount              = "pki"
	pkiRole               = "soul-seed"
)

var (
	integrationPool     *pgxpool.Pool
	integrationVault    *keepervault.Client
	integrationVaultAPI *vaultapi.Client
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pgCtr, err := tcpostgres.Run(ctx,
		integrationPGImage,
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("grpc integration: PG setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("grpc integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		tctx, tc := context.WithTimeout(context.Background(), 30*time.Second)
		defer tc()
		_ = pgCtr.Terminate(tctx)
	}()
	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("PG ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	vCtr, err := tcvault.Run(ctx, integrationVaultImage, tcvault.WithToken(integrationVaultToken))
	if err != nil {
		log.Printf("Vault Run: %v", err)
		return 1
	}
	defer func() {
		tctx, tc := context.WithTimeout(context.Background(), 30*time.Second)
		defer tc()
		_ = vCtr.Terminate(tctx)
	}()
	vAddr, err := vCtr.HttpHostAddress(ctx)
	if err != nil {
		log.Printf("Vault HttpHostAddress: %v", err)
		return 1
	}

	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = vAddr
	api, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		log.Printf("vaultapi.NewClient: %v", err)
		return 1
	}
	api.SetToken(integrationVaultToken)
	integrationVaultAPI = api

	if err := provisionPKI(ctx, api); err != nil {
		log.Printf("provisionPKI: %v", err)
		return 1
	}

	cl, err := keepervault.NewClient(ctx, config.KeeperVault{
		Addr: vAddr, Token: integrationVaultToken, KVMount: "secret",
	})
	if err != nil {
		log.Printf("vault NewClient: %v", err)
		return 1
	}
	integrationVault = cl

	return m.Run()
}

func provisionPKI(ctx context.Context, api *vaultapi.Client) error {
	if err := api.Sys().Mount(pkiMount, &vaultapi.MountInput{
		Type:   "pki",
		Config: vaultapi.MountConfigInput{MaxLeaseTTL: "87600h"},
	}); err != nil {
		return fmt.Errorf("mount pki: %w", err)
	}
	if _, err := api.Logical().WriteWithContext(ctx, pkiMount+"/root/generate/internal", map[string]any{
		"common_name": "soul-stack-test",
		"ttl":         "87600h",
	}); err != nil {
		return fmt.Errorf("generate root: %w", err)
	}
	if _, err := api.Logical().WriteWithContext(ctx, pkiMount+"/roles/"+pkiRole, map[string]any{
		"allowed_domains":  "example.com,test,localhost",
		"allow_subdomains": true,
		"allow_localhost":  true,
		"max_ttl":          "720h",
	}); err != nil {
		return fmt.Errorf("create role: %w", err)
	}
	return nil
}

func resetAll(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

// seedOnboardingFixtures — operator + pending soul + bootstrap_token.
// Returns the plain token (to present over gRPC) and the SID.
func seedOnboardingFixtures(t *testing.T) (plain, sid string) {
	t.Helper()
	ctx := context.Background()
	aid := "archon-alice"
	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT,
	}); err != nil {
		t.Fatalf("operator.Insert: %v", err)
	}
	sid = "host.example.com"
	creator := aid
	if err := soul.Insert(ctx, integrationPool, &soul.Soul{
		SID:          sid,
		Transport:    soul.TransportAgent,
		Status:       soul.StatusPending,
		CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("soul.Insert: %v", err)
	}
	tok, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatalf("Generate token: %v", err)
	}
	plain = tok.Reveal()
	if _, err := bootstraptoken.Insert(ctx, integrationPool, sid, tok.Hash(), time.Hour, &creator); err != nil {
		t.Fatalf("token Insert: %v", err)
	}
	return plain, sid
}

// startTestServer — brings up BootstrapServer on 127.0.0.1:0 with a
// self-signed server cert, returns the actual addr and a cleanup function.
// testSigilPubKeyPEM — a Sigil trust-anchor fixture for bootstrap
// integration. Matches the first element of the set (primary first): the
// legacy single-anchor reply is now derived from the live set rather than
// a separate field (R3-S7, architect af7d).
const testSigilPubKeyPEM = "-----BEGIN PUBLIC KEY-----\nTEST-SIGIL-PUBKEY\n-----END PUBLIC KEY-----\n"

// testSigilPubKeyPEMSet — a multi-anchor set (R3-S6) for bootstrap
// integration: primary (same as the single one) + a second anchor. Checks
// that the reply carries the full set (set>single on the Soul side).
var testSigilPubKeyPEMSet = []string{
	testSigilPubKeyPEM,
	"-----BEGIN PUBLIC KEY-----\nTEST-SIGIL-PUBKEY-2\n-----END PUBLIC KEY-----\n",
}

func startTestServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	cp, kp := mustSelfSignedIT(t, dir)
	deps := BootstrapDeps{
		Pool:        integrationPool,
		VaultClient: integrationVault,
		AuditWriter: auditpg.NewWriter(integrationPool),
		KID:         "kid-test",
		PKIMount:    pkiMount,
		PKIRole:     pkiRole,
		// Sigil trust anchors (ADR-026(h), R3-S7): a live source for the set.
		// The reply reads it on every onboarding; the legacy single anchor is
		// derived from the first element. The content isn't a valid PEM, just
		// a "passed through as-is" marker; the real form (SPKI) is checked in
		// the soul-side persistence layer.
		SigilAnchorSource: &fakeTrustAnchorSource{pems: testSigilPubKeyPEMSet},
	}
	srv, err := NewBootstrapServer(config.KeeperListenGRPCBootstrap{
		Addr: "127.0.0.1:0",
		TLS:  config.KeeperListenGRPCBootstrapTLS{Cert: cp, Key: kp},
	}, deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewBootstrapServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Start(ctx) }()
	// Wait for bind.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (srv.Addr() == "" || srv.Addr() == "127.0.0.1:0") {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Addr() == "127.0.0.1:0" {
		cancel()
		<-done
		t.Fatal("server did not bind")
	}
	return srv.Addr(), func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop within 5s")
		}
	}
}

func dialClient(t *testing.T, addr string) (keeperv1.KeeperClient, func()) {
	t.Helper()
	conn, err := grpclib.NewClient(addr,
		grpclib.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS13,
		})),
	)
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	return keeperv1.NewKeeperClient(conn), func() { _ = conn.Close() }
}

func TestIntegration_Bootstrap_HappyPath(t *testing.T) {
	resetAll(t)
	plain, sid := seedOnboardingFixtures(t)
	addr, stop := startTestServer(t)
	defer stop()

	client, closeClient := dialClient(t, addr)
	defer closeClient()

	csrPEM := mustMakeCSRIT(t, sid)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reply, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid:            sid,
		BootstrapToken: plain,
		CsrPem:         []byte(csrPEM),
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !strings.Contains(string(reply.GetCertificatePem()), "BEGIN CERTIFICATE") {
		t.Fatalf("cert not PEM: %q", reply.GetCertificatePem())
	}
	if !strings.Contains(string(reply.GetCaChainPem()), "BEGIN CERTIFICATE") {
		t.Fatalf("ca_chain not PEM: %q", reply.GetCaChainPem())
	}
	if reply.GetKid() != "kid-test" {
		t.Errorf("kid = %q, want kid-test", reply.GetKid())
	}
	if reply.GetNotAfter() == nil || !reply.GetNotAfter().AsTime().After(time.Now()) {
		t.Errorf("not_after = %v, want future", reply.GetNotAfter())
	}
	// Sigil trust anchor (ADR-026, S2b) delivered in the reply as-is.
	if reply.GetSigilPubkeyPem() != testSigilPubKeyPEM {
		t.Errorf("sigil_pubkey_pem = %q, want %q", reply.GetSigilPubkeyPem(), testSigilPubKeyPEM)
	}
	// Multi-anchor set (ADR-026(h), R3-S6) delivered in full (set>single on
	// the Soul side).
	gotSet := reply.GetSigilPubkeyPemSet()
	if len(gotSet) != len(testSigilPubKeyPEMSet) {
		t.Fatalf("sigil_pubkey_pem_set = %v, want %v", gotSet, testSigilPubKeyPEMSet)
	}
	for i := range testSigilPubKeyPEMSet {
		if gotSet[i] != testSigilPubKeyPEMSet[i] {
			t.Errorf("sigil_pubkey_pem_set[%d] = %q, want %q", i, gotSet[i], testSigilPubKeyPEMSet[i])
		}
	}

	// Check the DB: soul.connected, seed.active, token.used.
	s, err := soul.SelectBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if s.Status != soul.StatusConnected {
		t.Errorf("soul.status = %v, want connected", s.Status)
	}
	if s.LastSeenByKID == nil || *s.LastSeenByKID != "kid-test" {
		t.Errorf("last_seen_by_kid = %v, want kid-test", s.LastSeenByKID)
	}
	seed, err := soulseed.SelectActiveBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectActiveBySID: %v", err)
	}
	if seed.IssuedByKID == nil || *seed.IssuedByKID != "kid-test" {
		t.Errorf("seed.issued_by_kid = %v, want kid-test", seed.IssuedByKID)
	}
	if seed.SerialNumber == "" {
		t.Error("seed.serial_number empty")
	}

	// Audit: exactly two records (bootstrapped + seed-issued) sharing one
	// correlation_id.
	rows, err := integrationPool.Query(ctx, `SELECT event_type, correlation_id FROM audit_log ORDER BY event_type`)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	defer rows.Close()
	var types []string
	var corr0 string
	for rows.Next() {
		var typ, c string
		if err := rows.Scan(&typ, &c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		types = append(types, typ)
		if corr0 == "" {
			corr0 = c
		} else if c != corr0 {
			t.Errorf("correlation_id mismatch: %q vs %q", corr0, c)
		}
	}
	if len(types) != 2 {
		t.Errorf("audit rows = %v, want 2 (bootstrapped + seed-issued)", types)
	}
}

func TestIntegration_Bootstrap_InvalidToken(t *testing.T) {
	resetAll(t)
	_, sid := seedOnboardingFixtures(t)
	addr, stop := startTestServer(t)
	defer stop()

	client, closeClient := dialClient(t, addr)
	defer closeClient()

	csrPEM := mustMakeCSRIT(t, sid)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid:            sid,
		BootstrapToken: "wrong-token",
		CsrPem:         []byte(csrPEM),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", got)
	}
	// The soul stayed in pending; no seed was issued.
	s, err := soul.SelectBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if s.Status != soul.StatusPending {
		t.Errorf("soul.status = %v, want pending (unchanged)", s.Status)
	}
	if _, err := soulseed.SelectActiveBySID(ctx, integrationPool, sid); !errors.Is(err, soulseed.ErrSeedNotFound) {
		t.Errorf("expected no active seed, got err=%v", err)
	}
}

// TestIntegration_EventStream_HelloHandshake — e2e EventStream handshake:
//
//  1. Issue a SoulSeed cert via Bootstrap (server-only TLS) (RSA CSR with
//     CN=SID, signed by Vault PKI).
//  2. Bring up an EventStream listener (mTLS) with CA = Vault PKI root.
//  3. The client connects with the obtained cert+key, RootCAs = serverCert
//     (self-signed for the server side; a real Soul would trust the
//     Keeper cert via config).
//  4. Send Hello, wait for HelloReply: check kid + ULID format +
//     server_time.
func TestIntegration_EventStream_HelloHandshake(t *testing.T) {
	resetAll(t)
	plain, sid := seedOnboardingFixtures(t)

	// Start the Bootstrap server for onboarding.
	bsAddr, bsStop := startTestServer(t)
	defer bsStop()

	// 1) Bootstrap: SoulSeed-cert.
	csrPEM, clientKey := mustMakeCSRWithKeyIT(t, sid)
	bsClient, closeBS := dialClient(t, bsAddr)
	defer closeBS()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bsReply, err := bsClient.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid:            sid,
		BootstrapToken: plain,
		CsrPem:         []byte(csrPEM),
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// 2) EventStream-server: server-cert self-signed, CA = Vault PKI root.
	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	esAddr, esStop := startEventStreamServer(t, caRootPEM)
	defer esStop()

	// 3) Client: cert/key from the Bootstrap reply, RootCAs = server-cert.
	clientCert, err := tls.X509KeyPair(bsReply.GetCertificatePem(), clientKey)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	conn, err := grpclib.NewClient(esAddr, grpclib.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates:       []tls.Certificate{clientCert},
		InsecureSkipVerify: true, // server-cert self-signed; in prod this is replaced by RootCAs from the Soul's config.
		MinVersion:         tls.VersionTLS13,
	})))
	if err != nil {
		t.Fatalf("dial event_stream: %v", err)
	}
	defer conn.Close()
	esClient := keeperv1.NewKeeperClient(conn)

	// 4) Hello → HelloReply.
	stream, err := esClient.EventStream(ctx)
	if err != nil {
		t.Fatalf("EventStream: %v", err)
	}
	if err := stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_Hello{Hello: &keeperv1.Hello{
			SidEcho:     sid,
			SoulVersion: "test-0.0.1",
		}},
	}); err != nil {
		t.Fatalf("stream.Send Hello: %v", err)
	}
	got, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream.Recv HelloReply: %v", err)
	}
	reply := got.GetHelloReply()
	if reply == nil {
		t.Fatalf("got = %T, want *FromKeeper_HelloReply", got.GetPayload())
	}
	if reply.GetKid() != "kid-test" {
		t.Errorf("kid = %q, want kid-test", reply.GetKid())
	}
	if len(reply.GetSessionId()) != 26 {
		t.Errorf("session_id = %q (len %d), want 26-char ULID",
			reply.GetSessionId(), len(reply.GetSessionId()))
	}
	if reply.GetServerTime() == nil || reply.GetServerTime().AsTime().IsZero() {
		t.Errorf("server_time empty: %v", reply.GetServerTime())
	}
	_ = stream.CloseSend()
}

// TestIntegration_EventStream_RevokedSeedRejected — after Bootstrap, mark
// the seed as `revoked` in the DB and verify that a new EventStream
// connection is rejected at the application level (Unauthenticated). The
// mTLS handshake succeeds (the cert is signed by the same PKI, same CA),
// but the interceptor sees a non-active seed and closes the stream.
func TestIntegration_EventStream_RevokedSeedRejected(t *testing.T) {
	resetAll(t)
	plain, sid := seedOnboardingFixtures(t)

	bsAddr, bsStop := startTestServer(t)
	defer bsStop()

	csrPEM, clientKey := mustMakeCSRWithKeyIT(t, sid)
	bsClient, closeBS := dialClient(t, bsAddr)
	defer closeBS()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bsReply, err := bsClient.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Revoke the seed in the DB.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE soul_seeds SET status = 'revoked', revocation_reason = 'test' WHERE sid = $1`, sid,
	); err != nil {
		t.Fatalf("revoke seed: %v", err)
	}

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	esAddr, esStop := startEventStreamServer(t, caRootPEM)
	defer esStop()

	clientCert, err := tls.X509KeyPair(bsReply.GetCertificatePem(), clientKey)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	conn, err := grpclib.NewClient(esAddr, grpclib.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
	})))
	if err != nil {
		t.Fatalf("dial event_stream: %v", err)
	}
	defer conn.Close()
	esClient := keeperv1.NewKeeperClient(conn)
	stream, err := esClient.EventStream(ctx)
	if err != nil {
		// gRPC can already return an error at this stage (the interceptor
		// runs before the handler).
		if got := status.Code(err); got == codes.Unauthenticated {
			return
		}
		t.Fatalf("EventStream open: unexpected err: %v", err)
	}
	// Otherwise the error arrives on the first Send/Recv.
	_ = stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_Hello{Hello: &keeperv1.Hello{SidEcho: sid}},
	})
	_, err = stream.Recv()
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("Recv: code = %v, want Unauthenticated", got)
	}
}

// fetchVaultPKIRootCA — fetches the PEM root CA from the Vault PKI engine.
// Symmetric with `vault read pki/cert/ca`.
func fetchVaultPKIRootCA(t *testing.T, ctx context.Context) []byte {
	t.Helper()
	sec, err := integrationVaultAPI.Logical().ReadWithContext(ctx, pkiMount+"/cert/ca")
	if err != nil {
		t.Fatalf("vault read ca: %v", err)
	}
	if sec == nil || sec.Data == nil {
		t.Fatal("vault ca: nil response")
	}
	raw, _ := sec.Data["certificate"].(string)
	if raw == "" {
		t.Fatalf("vault ca: certificate missing, data=%v", sec.Data)
	}
	return []byte(raw)
}

// startEventStreamServer — brings up EventStreamServer on 127.0.0.1:0
// with a self-signed server cert and CA = the passed-in PKI root PEM.
func startEventStreamServer(t *testing.T, caPEM []byte) (addr string, cleanup func()) {
	return startEventStreamServerExt(t, caPEM, EventStreamDeps{
		SeedDB:      integrationPool,
		SoulDB:      integrationPool,
		AuditWriter: auditpg.NewWriter(integrationPool),
		KID:         "kid-test",
	})
}

// startEventStreamServerExt — an extended version for tests with custom
// deps (e.g., a different KID or a Redis client). depsTpl is the
// template; TLS/Addr/Logger are set by the helper.
func startEventStreamServerExt(t *testing.T, caPEM []byte, deps EventStreamDeps) (addr string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	cp, kp := mustSelfSignedIT(t, dir)
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	srv, err := NewEventStreamServer(config.KeeperListenGRPCEventStream{
		Addr: "127.0.0.1:0",
		TLS:  config.KeeperListenGRPCEventStreamTLS{Cert: cp, Key: kp, CA: caPath},
	}, deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewEventStreamServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Start(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (srv.Addr() == "" || srv.Addr() == "127.0.0.1:0") {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Addr() == "127.0.0.1:0" {
		cancel()
		<-done
		t.Fatal("event_stream server did not bind")
	}
	var once sync.Once
	return srv.Addr(), func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("event_stream server did not stop within 5s")
			}
		})
	}
}

// mustMakeCSRWithKeyIT — like mustMakeCSRIT, but also returns the
// PEM-encoded private key (RSA PKCS#1) for a subsequent tls.X509KeyPair
// with the signed cert.
func mustMakeCSRWithKeyIT(t *testing.T, cn string) (csrPEM string, keyPEM []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	var csr strings.Builder
	if err := pem.Encode(&writerAdapter{&csr}, &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}); err != nil {
		t.Fatalf("pem.Encode csr: %v", err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(priv)
	var keyBuf strings.Builder
	if err := pem.Encode(&writerAdapter{&keyBuf}, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("pem.Encode key: %v", err)
	}
	return csr.String(), []byte(keyBuf.String())
}

func TestIntegration_Bootstrap_TokenReuseRejected(t *testing.T) {
	resetAll(t)
	plain, sid := seedOnboardingFixtures(t)
	addr, stop := startTestServer(t)
	defer stop()

	client, closeClient := dialClient(t, addr)
	defer closeClient()

	csrPEM := mustMakeCSRIT(t, sid)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// First time — success.
	if _, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
	}); err != nil {
		t.Fatalf("Bootstrap #1: %v", err)
	}
	// Second time — the token is burned.
	_, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("Bootstrap #2: code = %v, want PermissionDenied", got)
	}
}

// mustMakeCSRIT — a copy of the helper from vault/integration_test.go
// (RSA CSR under the role's allowed_domains).
func mustMakeCSRIT(t *testing.T, cn string) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	var b strings.Builder
	if err := pem.Encode(&writerAdapter{&b}, &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}); err != nil {
		t.Fatalf("pem.Encode: %v", err)
	}
	return b.String()
}

type writerAdapter struct{ b *strings.Builder }

func (w *writerAdapter) Write(p []byte) (int, error) { return w.b.Write(p) }

// mustSelfSignedIT — a self-signed server cert for the TLS gRPC listener.
// ECDSA, CN=test.example.com, valid for an hour; that's enough for the
// integration test.
func mustSelfSignedIT(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.example.com"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"test.example.com", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if f, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err == nil {
		_ = pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der})
		_ = f.Close()
	} else {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	if f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err == nil {
		_ = pem.Encode(f, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
		_ = f.Close()
	} else {
		t.Fatal(err)
	}
	return certPath, keyPath
}
