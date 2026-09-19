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
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
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
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	pgCtr, err := integrationenv.Start(ctx, "postgres", func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
		return tcpostgres.Run(ctx,
			integrationPGImage,
			tcpostgres.WithDatabase("keeper_test"),
			tcpostgres.WithUsername("keeper"),
			tcpostgres.WithPassword("keeper"),
			tcpostgres.BasicWaitStrategies(),
		)
	})
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

	vCtr, err := integrationenv.Start(ctx, "vault", func(ctx context.Context) (*tcvault.VaultContainer, error) {
		return tcvault.Run(ctx, integrationVaultImage, tcvault.WithToken(integrationVaultToken))
	})
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

	// Check the DB: the soul stays PENDING, seed.active, token.used.
	//
	// `connected` is defined as "stream alive, Keeper holds lease in Redis"
	// and no stream exists yet — signing a certificate is one network hop
	// short of the host holding one. Bootstrap used to write it here anyway,
	// and nothing ever corrected it for a host that never appeared, because
	// the Reaper's disconnect sweep cannot match a NULL last_seen_at
	// (migration 043). `connected` now comes from the EventStream handshake —
	// see TestIntegration_EventStream_HelloHandshake (NIM-865).
	s, err := soul.SelectBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if s.Status != soul.StatusPending {
		t.Errorf("soul.status = %v, want pending (no stream has existed yet)", s.Status)
	}
	if s.LastSeenByKID != nil {
		t.Errorf("last_seen_by_kid = %v, want nil — it records which Keeper held the STREAM", s.LastSeenByKID)
	}
	// The predicate the token-recovery path is gated on: a host that has never
	// held a stream. If bootstrap ever starts writing this, a burned token
	// becomes unrecoverable again and NIM-865 is back.
	if s.LastSeenAt != nil {
		t.Errorf("last_seen_at = %v, want nil after bootstrap", s.LastSeenAt)
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

	// The handshake is what makes `connected` true, and is now what writes it
	// (NIM-865). Bootstrap above left the row `pending`; the value is earned
	// here, by a peer that authenticated with an active seed and received a
	// message.
	//
	// Polled, not read once: the server writes AFTER stream.Send returns, so a
	// client that has already received HelloReply can legitimately win the race
	// to the database. A single read here is a flake, not a check.
	var s *soul.Soul
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err = soul.SelectBySID(ctx, integrationPool, sid)
		if err != nil {
			t.Fatalf("SelectBySID: %v", err)
		}
		if s.Status == soul.StatusConnected || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.Status != soul.StatusConnected {
		t.Errorf("soul.status = %v after the handshake, want connected", s.Status)
	}
	if s.LastSeenByKID == nil || *s.LastSeenByKID != "kid-test" {
		t.Errorf("last_seen_by_kid = %v, want kid-test", s.LastSeenByKID)
	}
	// `last_seen_at` is the predicate the burned-token recovery is gated on, so
	// a completed handshake must have closed it. Two writers uphold this today
	// — MarkConnected and the Hello-as-app-message flush — which is why removing
	// either one alone leaves this green; it guards the INVARIANT, not a
	// particular writer, and it is the thing that must never regress.
	if s.LastSeenAt == nil {
		t.Error("last_seen_at is nil after the handshake — the recovery gate is still open on a live host")
	}
	_ = stream.CloseSend()
}

// TestIntegration_MarkConnected_DoesNotResurrectALifecycleStatus — a
// reconnecting stream must not undo a lifecycle decision. `MarkConnected` is
// deliberately narrowed to `pending`/`disconnected`: an operator who revoked a
// host, or a cascade that destroyed one, outranks the fact that something is
// still dialling in, and a blanket `SET status='connected'` would reverse both.
//
// It calls the writer directly rather than opening a stream — a revoked host
// cannot complete the handshake anyway (the seed authenticator refuses it), so
// driving this through gRPC would assert the interceptor, not the guard. What
// is under test is the WHERE clause.
func TestIntegration_MarkConnected_DoesNotResurrectALifecycleStatus(t *testing.T) {
	resetAll(t)
	_, sid := seedOnboardingFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, st := range []soul.Status{soul.StatusRevoked, soul.StatusDestroyed, soul.StatusExpired} {
		if _, err := integrationPool.Exec(ctx,
			`UPDATE souls SET status = $2 WHERE sid = $1`, sid, string(st)); err != nil {
			t.Fatalf("set status %s: %v", st, err)
		}
		if err := soul.MarkConnected(ctx, integrationPool, sid, "kid-test", time.Now().UTC()); err != nil {
			t.Fatalf("MarkConnected(%s): %v", st, err)
		}
		s, err := soul.SelectBySID(ctx, integrationPool, sid)
		if err != nil {
			t.Fatalf("SelectBySID: %v", err)
		}
		if s.Status != st {
			t.Errorf("status = %v after MarkConnected over %v, want it untouched", s.Status, st)
		}
	}
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

// TestIntegration_Bootstrap_TokenReuseRejected — a burned token is refused for
// a SECOND BINDING, which is what one-time means (NIM-865). It used to refuse
// every repeat presentation, including the one that carried the same key; that
// is the defect, not the property — the Keeper burns when it SIGNS, and a reply
// lost between the commit and the host is enough to strand it forever.
//
// The three refusals below are the whole of what anti-replay defends, and each
// is checked against a token that is otherwise perfectly recoverable, so a
// carve-out that widened past its conditions fails here.
func TestIntegration_Bootstrap_TokenReuseRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// An attacker replaying a captured token wants a certificate for their OWN
	// key. That is the second binding, and it stays refused.
	t.Run("a different key", func(t *testing.T) {
		resetAll(t)
		plain, sid := seedOnboardingFixtures(t)
		addr, stop := startTestServer(t)
		defer stop()
		client, closeClient := dialClient(t, addr)
		defer closeClient()

		if _, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
			Sid: sid, BootstrapToken: plain, CsrPem: []byte(mustMakeCSRIT(t, sid)),
		}); err != nil {
			t.Fatalf("Bootstrap #1: %v", err)
		}
		_, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
			Sid: sid, BootstrapToken: plain, CsrPem: []byte(mustMakeCSRIT(t, sid)),
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("Bootstrap with a fresh key = %v, want PermissionDenied", got)
		}
	})

	// ★ Outranks the recovery itself. Once the host has appeared, the
	// credential provably arrived and the token is finished — otherwise a
	// plaintext still sitting in /etc/soul/token could be turned against a
	// working host at any point inside the TTL.
	t.Run("after the host has connected", func(t *testing.T) {
		resetAll(t)
		plain, sid := seedOnboardingFixtures(t)
		addr, stop := startTestServer(t)
		defer stop()
		client, closeClient := dialClient(t, addr)
		defer closeClient()

		csrPEM := mustMakeCSRIT(t, sid)
		if _, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
			Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
		}); err != nil {
			t.Fatalf("Bootstrap #1: %v", err)
		}
		// Exactly what the EventStream handshake writes — the real writer, not a
		// hand-rolled UPDATE that would keep passing if the two drifted apart.
		if err := soul.MarkConnected(ctx, integrationPool, sid, "kid-test", time.Now().UTC()); err != nil {
			t.Fatalf("MarkConnected: %v", err)
		}
		_, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
			Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("Bootstrap after first contact = %v, want PermissionDenied", got)
		}
	})

	// An operator who force-reissued meant it: that burn records an
	// invalidation, not a presentation, so there is no lost reply behind it.
	t.Run("a system-marked burn", func(t *testing.T) {
		resetAll(t)
		plain, sid := seedOnboardingFixtures(t)
		addr, stop := startTestServer(t)
		defer stop()
		client, closeClient := dialClient(t, addr)
		defer closeClient()

		csrPEM := mustMakeCSRIT(t, sid)
		if _, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
			Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
		}); err != nil {
			t.Fatalf("Bootstrap #1: %v", err)
		}
		if _, err := integrationPool.Exec(ctx,
			`UPDATE bootstrap_tokens SET used_by_kid = $2 WHERE sid = $1`,
			sid, bootstraptoken.SystemKIDForceReissue); err != nil {
			t.Fatalf("mark force-reissue: %v", err)
		}
		_, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
			Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("Bootstrap over a force-reissue burn = %v, want PermissionDenied", got)
		}
	})
}

// TestIntegration_Bootstrap_RecoversLostReply — the defect end to end: the
// Keeper signed and committed, the reply never reached the host, and the retry
// completes the onboarding instead of being stranded.
//
// The seed row must be re-stamped IN PLACE. `soul_seeds_fingerprint_idx` is
// globally unique, so a Supersede+Insert here would violate it — and would
// revoke the very identity the retry is delivering, since a stream is admitted
// only on `status='active'`.
func TestIntegration_Bootstrap_RecoversLostReply(t *testing.T) {
	resetAll(t)
	plain, sid := seedOnboardingFixtures(t)
	addr, stop := startTestServer(t)
	defer stop()

	client, closeClient := dialClient(t, addr)
	defer closeClient()

	csrPEM := mustMakeCSRIT(t, sid)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The attempt whose reply is lost. The Soul never sees this certificate;
	// everything the Keeper committed for it stays behind.
	lost, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
	})
	if err != nil {
		t.Fatalf("Bootstrap #1: %v", err)
	}
	before, err := soulseed.SelectActiveBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectActiveBySID before: %v", err)
	}

	// The retry `soul init` makes behind ADR-0063's seed-cert guard, carrying
	// the key the first attempt persisted.
	got, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
	})
	if err != nil {
		t.Fatalf("Bootstrap #2 (same key, never connected): %v, want success", err)
	}
	if !strings.Contains(string(got.GetCertificatePem()), "BEGIN CERTIFICATE") {
		t.Fatalf("recovery cert not PEM: %q", got.GetCertificatePem())
	}
	if string(got.GetCertificatePem()) == string(lost.GetCertificatePem()) {
		t.Error("recovery returned the SAME certificate — it must be freshly signed, nothing stores the lost PEM")
	}

	after, err := soulseed.SelectActiveBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectActiveBySID after: %v", err)
	}
	// The identity did not move: the fingerprint is the public key, and
	// re-signing one key does not change it.
	if after.Fingerprint != before.Fingerprint {
		t.Errorf("fingerprint moved: %q → %q", before.Fingerprint, after.Fingerprint)
	}
	if after.SeedID != before.SeedID {
		t.Errorf("seed_id = %q, want the row re-stamped in place (%q)", after.SeedID, before.SeedID)
	}
	if after.SerialNumber == before.SerialNumber {
		t.Errorf("serial_number = %q unchanged — a new certificate was not recorded", after.SerialNumber)
	}
	// Exactly one row, and it is active: a second one would mean the recovery
	// took the Supersede+Insert path.
	var seedRows int
	if err := integrationPool.QueryRow(ctx,
		`SELECT count(*) FROM soul_seeds WHERE sid = $1`, sid).Scan(&seedRows); err != nil {
		t.Fatalf("count seeds: %v", err)
	}
	if seedRows != 1 {
		t.Errorf("soul_seeds rows = %d, want 1", seedRows)
	}
	// And the registry still says what is true: no stream has ever existed.
	s, err := soul.SelectBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if s.Status != soul.StatusPending || s.LastSeenAt != nil {
		t.Errorf("soul = {%v, last_seen_at=%v}, want pending with no last_seen_at", s.Status, s.LastSeenAt)
	}
}

// TestIntegration_Bootstrap_FreshTokenWithTheKeptKey — the OTHER recovery, and
// the one that persisting the key nearly broke.
//
// An operator who does not want to rely on the re-presentation window simply
// issues a new token through `issue-token`. NOT via `core.bootstrap.issued`:
// that path converges over a host holding an active seed rather than re-arming
// it, because handing such a host a fresh token is a takeover primitive
// (NIM-865, see TestIntegration_IssuedBatch_PendingHostWithASeedIsNotReArmed).
// The host still has the key of the attempt whose reply was lost, so it
// presents a FIRST presentation of a fresh token carrying a key the registry
// has already recorded — and `soul_seeds_fingerprint_idx` is globally unique,
// so a Supersede+Insert here fails the index and burns the new token for
// nothing. The seed write therefore decides insert-vs-re-stamp from the
// registry, not from which token path it is on.
func TestIntegration_Bootstrap_FreshTokenWithTheKeptKey(t *testing.T) {
	resetAll(t)
	plain, sid := seedOnboardingFixtures(t)
	addr, stop := startTestServer(t)
	defer stop()

	client, closeClient := dialClient(t, addr)
	defer closeClient()

	csrPEM := mustMakeCSRIT(t, sid)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
	}); err != nil {
		t.Fatalf("Bootstrap #1: %v", err)
	}
	before, err := soulseed.SelectActiveBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectActiveBySID before: %v", err)
	}

	// The operator's fresh token for the same host.
	fresh, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, _, err := bootstraptoken.ExpireActiveBySID(ctx, integrationPool, sid,
		bootstraptoken.SystemKIDForceReissue); err != nil {
		t.Fatalf("ExpireActiveBySID: %v", err)
	}
	if _, err := bootstraptoken.Insert(ctx, integrationPool, sid, fresh.Hash(), 24*time.Hour, nil); err != nil {
		t.Fatalf("Insert fresh token: %v", err)
	}

	// `soul init` again, still carrying the key it persisted.
	if _, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: fresh.Reveal(), CsrPem: []byte(csrPEM),
	}); err != nil {
		t.Fatalf("Bootstrap with a fresh token and the kept key: %v, want success", err)
	}

	after, err := soulseed.SelectActiveBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectActiveBySID after: %v", err)
	}
	if after.SeedID != before.SeedID || after.Fingerprint != before.Fingerprint {
		t.Errorf("seed = {%q, %q}, want the same row re-stamped ({%q, %q})",
			after.SeedID, after.Fingerprint, before.SeedID, before.Fingerprint)
	}
	if after.SerialNumber == before.SerialNumber {
		t.Error("serial_number unchanged — no new certificate was recorded")
	}
	var seedRows int
	if err := integrationPool.QueryRow(ctx,
		`SELECT count(*) FROM soul_seeds WHERE sid = $1`, sid).Scan(&seedRows); err != nil {
		t.Fatalf("count seeds: %v", err)
	}
	if seedRows != 1 {
		t.Errorf("soul_seeds rows = %d, want 1", seedRows)
	}
}

// TestIntegration_Bootstrap_RefusesAKeyBoundToAnotherSID — a CSR carrying
// someone else's public key. The key is public, so presenting one is free; what
// it must not buy is a certificate, and the refusal is the ordinary
// PermissionDenied rather than the unique-index violation this used to be.
func TestIntegration_Bootstrap_RefusesAKeyBoundToAnotherSID(t *testing.T) {
	resetAll(t)
	victimPlain, victimSID := seedOnboardingFixtures(t)
	addr, stop := startTestServer(t)
	defer stop()

	client, closeClient := dialClient(t, addr)
	defer closeClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The victim onboards normally.
	victimCSR, victimKey := mustMakeCSRWithKeyIT(t, victimSID)
	if _, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: victimSID, BootstrapToken: victimPlain, CsrPem: []byte(victimCSR),
	}); err != nil {
		t.Fatalf("victim Bootstrap: %v", err)
	}
	victimSeed, err := soulseed.SelectActiveBySID(ctx, integrationPool, victimSID)
	if err != nil {
		t.Fatalf("victim seed: %v", err)
	}

	// A second host with a token of its own presents the victim's public key.
	// Building that CSR needs the victim's private half, which an attacker does
	// not have — so this is the strongest form of the attempt, and it still
	// must not be served.
	otherSID := "other-host.example.com"
	if err := soul.Insert(ctx, integrationPool, &soul.Soul{SID: otherSID, Status: soul.StatusPending}); err != nil {
		t.Fatalf("insert other soul: %v", err)
	}
	otherTok, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := bootstraptoken.Insert(ctx, integrationPool, otherSID, otherTok.Hash(), 24*time.Hour, nil); err != nil {
		t.Fatalf("insert other token: %v", err)
	}
	stolenCSR := mustMakeCSRForKeyIT(t, otherSID, victimKey)

	_, err = client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: otherSID, BootstrapToken: otherTok.Reveal(), CsrPem: []byte(stolenCSR),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("Bootstrap with another host's key = %v, want PermissionDenied", got)
	}
	// The victim's identity is untouched.
	still, err := soulseed.SelectActiveBySID(ctx, integrationPool, victimSID)
	if err != nil {
		t.Fatalf("victim seed after: %v", err)
	}
	if still.SeedID != victimSeed.SeedID || still.SerialNumber != victimSeed.SerialNumber {
		t.Errorf("victim seed moved: %+v → %+v", victimSeed, still)
	}
}

// mustMakeCSRForKeyIT builds a CSR with the given CN over an EXISTING key,
// which is what a retry does: the public key is what the Keeper recognizes, so
// a test about key identity has to be able to hold the key fixed and vary
// everything else.
func mustMakeCSRForKeyIT(t *testing.T, cn string, keyPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		t.Fatal("key is not PEM")
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKCS1PrivateKey: %v", err)
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

// TestIntegration_Bootstrap_FreshTokenWithAFreshKeyRotatesTheSeed — the third
// arm of writeSeed, and the operator remedy for a host that lost its key.
//
// A disk wipe leaves the registry holding an active seed the host can no longer
// use. `issue-token --force` mints a new token, the host generates a NEW keypair
// (no pending key survived), and that is a first presentation of a key the
// registry has never seen: supersede the old seed, insert the new one. Without
// this the seed-write path would be covered only where it re-stamps in place,
// and the arm that actually rotates an identity would be guarded by nothing.
func TestIntegration_Bootstrap_FreshTokenWithAFreshKeyRotatesTheSeed(t *testing.T) {
	resetAll(t)
	plain, sid := seedOnboardingFixtures(t)
	addr, stop := startTestServer(t)
	defer stop()

	client, closeClient := dialClient(t, addr)
	defer closeClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: plain, CsrPem: []byte(mustMakeCSRIT(t, sid)),
	}); err != nil {
		t.Fatalf("Bootstrap #1: %v", err)
	}
	before, err := soulseed.SelectActiveBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectActiveBySID before: %v", err)
	}

	// `issue-token --force`, then a host with nothing left on disk.
	fresh, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, _, err := bootstraptoken.ExpireActiveBySID(ctx, integrationPool, sid,
		bootstraptoken.SystemKIDForceReissue); err != nil {
		t.Fatalf("ExpireActiveBySID: %v", err)
	}
	if _, err := bootstraptoken.Insert(ctx, integrationPool, sid, fresh.Hash(), 24*time.Hour, nil); err != nil {
		t.Fatalf("Insert fresh token: %v", err)
	}

	if _, err := client.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: fresh.Reveal(), CsrPem: []byte(mustMakeCSRIT(t, sid)),
	}); err != nil {
		t.Fatalf("Bootstrap with a fresh token and a fresh key: %v, want success", err)
	}

	after, err := soulseed.SelectActiveBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectActiveBySID after: %v", err)
	}
	if after.Fingerprint == before.Fingerprint {
		t.Error("fingerprint unchanged — a new keypair did not produce a new identity")
	}
	if after.SeedID == before.SeedID {
		t.Error("the same row was re-stamped; a different key must get its own row")
	}
	// The old row survives as history, superseded — not deleted, and not left
	// active beside the new one (the partial unique index allows exactly one).
	var total, active int
	if err := integrationPool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE status = 'active') FROM soul_seeds WHERE sid = $1`,
		sid).Scan(&total, &active); err != nil {
		t.Fatalf("count seeds: %v", err)
	}
	if total != 2 || active != 1 {
		t.Errorf("soul_seeds total/active = %d/%d, want 2/1", total, active)
	}
}
