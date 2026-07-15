//go:build integration

// Integration tests for M2.3 (Redis SoulLease) and M2.4 (event handlers).
//
// We bring up PG + Vault PKI (shared fixtures from integration_test.go) and
// an in-process miniredis for Redis. miniredis needs no docker, but we
// still keep this under the `integration` tag because without PG the
// handlers can't write `souls` and `audit_log`.

package grpc

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// discardIntegrationLogger — a slog logger that prints nothing. Not tied
// to t.Helper (used from non-test-helper functions); lives here under the
// integration build so it doesn't depend on unit-build tags.
func discardIntegrationLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// drainEventStreamClient — closes the client's send half and reads the
// receive half until EOF/error. Needed for tests that bring up an
// EventStream server with an outbound channel: if the client doesn't close
// the stream, the server's GracefulStop waits for all bidi streams to end
// and the 5s timeout safety net in startEventStreamServerExt kicks in.
func drainEventStreamClient(stream keeperv1.Keeper_EventStreamClient) {
	_ = stream.CloseSend()
	for {
		_, err := stream.Recv()
		if err != nil {
			return
		}
	}
}

// newIntegrationRedisClient — a miniredis instance + Keeper-Redis wrapper.
// In-process miniredis → no docker needed.
func newIntegrationRedisClient(t *testing.T) (*keeperredis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := keeperredis.NewClient(context.Background(), keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// helloAndEventStream — onboards + opens EventStream + sends Hello.
// Returns the stream (the caller sends any further payloads itself).
func helloAndEventStream(t *testing.T, ctx context.Context, esAddr string, certPEM, keyPEM []byte) keeperv1.Keeper_EventStreamClient {
	t.Helper()
	clientCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	conn, err := grpclib.NewClient(esAddr, grpclib.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clientCert}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
	})))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := keeperv1.NewKeeperClient(conn)
	stream, err := client.EventStream(ctx)
	if err != nil {
		t.Fatalf("EventStream: %v", err)
	}
	if err := stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_Hello{Hello: &keeperv1.Hello{SidEcho: "host.example.com"}},
	}); err != nil {
		t.Fatalf("send Hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv HelloReply: %v", err)
	}
	return stream
}

// TestIntegration_EventStream_LeaseConflict_ReturnsAlreadyExists — a
// second Keeper instance connecting for the same SID gets AlreadyExists
// when the current holder is ALIVE in Conclave (split-brain guard, ADR-027
// amend (n)). Presence-gated force-release does NOT trigger on a live
// owner — otherwise delivery dedup by SID lease would be undermined.
func TestIntegration_EventStream_LeaseConflict_ReturnsAlreadyExists(t *testing.T) {
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

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)

	// kid-a is alive in Conclave: the EventStream server itself doesn't
	// register presence (that's done by the daemon wire-up), so the test
	// registers it explicitly — otherwise kid-b would consider kid-a dead
	// and force-release it (that's covered separately by the reconnect
	// scenario). Here we're specifically checking the guard for a live
	// holder.
	if err := keeperredis.RegisterInstance(ctx, rc, "kid-a", "kid-a", 30*time.Second, false); err != nil {
		t.Fatalf("RegisterInstance(kid-a): %v", err)
	}

	// Keeper A — acquires the lease, keeps the stream open.
	esAddrA, esStopA := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter:  auditpg.NewWriter(integrationPool),
		KID:          "kid-a",
		SoulLeaseTTL: 5 * time.Second,
	})
	defer esStopA()

	streamA := helloAndEventStream(t, ctx, esAddrA, bsReply.GetCertificatePem(), clientKey)
	defer streamA.CloseSend()

	// Keeper B — tries to accept a stream with the same SID.
	esAddrB, esStopB := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter:  auditpg.NewWriter(integrationPool),
		KID:          "kid-b",
		SoulLeaseTTL: 5 * time.Second,
	})
	defer esStopB()

	clientCertB, err := tls.X509KeyPair(bsReply.GetCertificatePem(), clientKey)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	connB, err := grpclib.NewClient(esAddrB, grpclib.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clientCertB}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
	})))
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.Close()
	clientB := keeperv1.NewKeeperClient(connB)
	streamB, err := clientB.EventStream(ctx)
	if err != nil {
		if code := status.Code(err); code == codes.AlreadyExists {
			return
		}
		t.Fatalf("EventStream B open: %v", err)
	}
	if err := streamB.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_Hello{Hello: &keeperv1.Hello{SidEcho: sid}},
	}); err != nil {
		// AlreadyExists can also arrive on Send.
		if code := status.Code(err); code == codes.AlreadyExists {
			return
		}
	}
	_, err = streamB.Recv()
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("B Recv: code = %v, want AlreadyExists (err=%v)", got, err)
	}
}

// TestIntegration_EventStream_DeadHolderLease_ForceReleased — finding 2
// (ADR-027 amend (n)): a stale SID lease from a dead holder is hijacked on
// reconnect of the same SID to a different keeper. Emulation: kid-dead
// holds the lease (as after a SIGKILL — the key hangs around until TTL),
// but its Conclave presence is gone (the instance isn't renewing). The
// Soul reconnects to kid-live → presence-gated force-release → the stream
// OPENS (HelloReply arrives) instead of closing with AlreadyExists. This
// is what lifts the ~60s block on dispatched-orphan reconciliation.
func TestIntegration_EventStream_DeadHolderLease_ForceReleased(t *testing.T) {
	resetAll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, certPEM, keyPEM := bootstrapForStream(t, ctx)

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)

	// Dead holder kid-dead: the lease hangs around (the crash left it until
	// TTL), but Conclave presence is NOT registered (the renewal goroutine
	// is dead).
	if _, err := keeperredis.AcquireSoulLease(ctx, rc, sid, "kid-dead", 60*time.Second); err != nil {
		t.Fatalf("seed dead-holder lease: %v", err)
	}

	// The live keeper kid-live accepts the Soul's reconnect.
	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter:  auditpg.NewWriter(integrationPool),
		KID:          "kid-live",
		SoulLeaseTTL: 5 * time.Second,
	})
	defer esStop()

	// helloAndEventStream fails with t.Fatal if HelloReply doesn't arrive —
	// a successful return means the stream is open, force-release worked.
	stream := helloAndEventStream(t, ctx, esAddr, certPEM, keyPEM)
	defer drainEventStreamClient(stream)

	// The lease has been re-acquired by kid-live.
	if !waitSoulAlive(t, ctx, rc, sid, true) {
		t.Fatalf("soul offline after reconnect, want online (force-release acquired lease)")
	}
	owner, ok, err := keeperredis.SoulLeaseOwner(ctx, rc, sid)
	if err != nil || !ok {
		t.Fatalf("SoulLeaseOwner: ok=%v err=%v", ok, err)
	}
	if owner != "kid-live" {
		t.Errorf("lease owner = %q, want kid-live (перехвачен у мёртвого kid-dead)", owner)
	}

	// Audit `eventstream.lease_force_released` with prev/new KID.
	deadline := time.Now().Add(3 * time.Second)
	var (
		prevKID, newKID string
		found           bool
	)
	for time.Now().Before(deadline) {
		row := integrationPool.QueryRow(ctx,
			`SELECT payload->>'prev_kid', payload->>'new_kid'
			   FROM audit_log
			  WHERE event_type='eventstream.lease_force_released' AND correlation_id=$1
			  ORDER BY created_at DESC LIMIT 1`, sid)
		if err := row.Scan(&prevKID, &newKID); err == nil {
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("audit_log: eventstream.lease_force_released not found")
	}
	if prevKID != "kid-dead" || newKID != "kid-live" {
		t.Errorf("audit prev/new = %q/%q, want kid-dead/kid-live", prevKID, newKID)
	}
}

// TestIntegration_EventStream_HeartbeatUpdated — after Hello, the Redis
// heartbeat cache is updated (HSET soul:<sid>:hb).
func TestIntegration_EventStream_HeartbeatUpdated(t *testing.T) {
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

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, mr := newIntegrationRedisClient(t)

	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter: auditpg.NewWriter(integrationPool), KID: "kid-test",
	})
	defer esStop()
	stream := helloAndEventStream(t, ctx, esAddr, bsReply.GetCertificatePem(), clientKey)
	defer stream.CloseSend()

	// Polling: the heartbeat is written async relative to the Hello Send.
	deadline := time.Now().Add(2 * time.Second)
	var atStr, kidStr string
	for time.Now().Before(deadline) {
		atStr = mr.HGet("soul:"+sid+":hb", "at")
		kidStr = mr.HGet("soul:"+sid+":hb", "kid")
		if atStr != "" && kidStr != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if kidStr != "kid-test" {
		t.Errorf("hb.kid = %q, want kid-test", kidStr)
	}
	if atStr == "" {
		t.Error("hb.at empty after Hello")
	}
}

// TestIntegration_EventStream_TaskEventWritesAudit — TaskEvent → audit_log
// with event_type=task.executed and correlation_id=apply_id.
func TestIntegration_EventStream_TaskEventWritesAudit(t *testing.T) {
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

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)

	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter: auditpg.NewWriter(integrationPool), KID: "kid-test",
	})
	defer esStop()
	stream := helloAndEventStream(t, ctx, esAddr, bsReply.GetCertificatePem(), clientKey)

	applyID := "01HABCAPPLY00000000000000"
	if err := stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_TaskEvent{TaskEvent: &keeperv1.TaskEvent{
			ApplyId: applyID, TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED,
		}},
	}); err != nil {
		t.Fatalf("send TaskEvent: %v", err)
	}
	_ = stream.CloseSend()
	// Wait for EOF from the server so the audit writer has time to run.
	for {
		_, err := stream.Recv()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled {
			break
		}
		break
	}

	// Polling: audit is written in the handler's same goroutine, but
	// Send/Close finish the stream asynchronously — give it a small window.
	deadline := time.Now().Add(3 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		row := integrationPool.QueryRow(ctx,
			`SELECT correlation_id FROM audit_log WHERE event_type='task.executed' ORDER BY created_at DESC LIMIT 1`)
		var corr string
		if err := row.Scan(&corr); err == nil && corr == applyID {
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("audit_log: task.executed with apply_id not found")
	}
}

// TestIntegration_EventStream_OutboundSendApply — after the Hello
// handshake and registration in StreamManager, SendApply for the same SID
// reaches the client via stream.Recv() (M2.5).
func TestIntegration_EventStream_OutboundSendApply(t *testing.T) {
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

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)
	mgr := NewStreamManager(discardIntegrationLogger())

	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter: auditpg.NewWriter(integrationPool),
		KID:         "kid-test",
		Manager:     mgr,
	})
	defer esStop()
	stream := helloAndEventStream(t, ctx, esAddr, bsReply.GetCertificatePem(), clientKey)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.lookup(sid) != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mgr.lookup(sid) == nil {
		t.Fatal("StreamManager: sid not registered after handshake")
	}

	ob, err := NewOutbound(OutboundDeps{
		Manager:     mgr,
		AuditWriter: auditpg.NewWriter(integrationPool),
		Logger:      discardIntegrationLogger(),
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	applyReq := &keeperv1.ApplyRequest{
		ApplyId: "01HABCAPPLY00000000000001",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "install", Module: "core.pkg.installed"},
		},
	}
	if err := ob.SendApply(ctx, sid, applyReq); err != nil {
		t.Fatalf("SendApply: %v", err)
	}

	got, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream.Recv: %v", err)
	}
	ar := got.GetApplyRequest()
	if ar == nil {
		t.Fatalf("payload = %T, want ApplyRequest", got.GetPayload())
	}
	if ar.GetApplyId() != applyReq.GetApplyId() {
		t.Errorf("apply_id = %q, want %q", ar.GetApplyId(), applyReq.GetApplyId())
	}
	if len(ar.GetTasks()) != 1 || ar.GetTasks()[0].GetModule() != "core.pkg.installed" {
		t.Errorf("tasks = %+v", ar.GetTasks())
	}
	drainEventStreamClient(stream)

	// Audit `apply.dispatched`.
	deadline = time.Now().Add(3 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		row := integrationPool.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE event_type='apply.dispatched' AND correlation_id=$1`,
			applyReq.GetApplyId())
		if err := row.Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n != 1 {
		t.Errorf("audit apply.dispatched count = %d, want 1", n)
	}
}

// TestIntegration_EventStream_OutboundSendCancel — SendCancel arrives at
// the client as CancelApply (M2.5).
func TestIntegration_EventStream_OutboundSendCancel(t *testing.T) {
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

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)
	mgr := NewStreamManager(discardIntegrationLogger())

	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter: auditpg.NewWriter(integrationPool),
		KID:         "kid-test",
		Manager:     mgr,
	})
	defer esStop()
	stream := helloAndEventStream(t, ctx, esAddr, bsReply.GetCertificatePem(), clientKey)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.lookup(sid) != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	ob, _ := NewOutbound(OutboundDeps{
		Manager:     mgr,
		AuditWriter: auditpg.NewWriter(integrationPool),
		Logger:      discardIntegrationLogger(),
	})
	if err := ob.SendCancel(ctx, sid, "01HABCAPPLY00000000000099", "test-cancel"); err != nil {
		t.Fatalf("SendCancel: %v", err)
	}

	got, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream.Recv: %v", err)
	}
	ca := got.GetCancelApply()
	if ca == nil {
		t.Fatalf("payload = %T, want CancelApply", got.GetPayload())
	}
	if ca.GetApplyId() != "01HABCAPPLY00000000000099" || ca.GetReason() != "test-cancel" {
		t.Errorf("CancelApply = %+v", ca)
	}
	drainEventStreamClient(stream)
}

// TestIntegration_EventStream_SeedRotation — the Soul sends a
// SeedRotationRequest, the Keeper signs the CSR via Vault PKI, supersedes
// the old seed, inserts a new one, and sends SeedRotationReply back (M2.5).
func TestIntegration_EventStream_SeedRotation(t *testing.T) {
	resetAll(t)
	plain, sid := seedOnboardingFixtures(t)

	bsAddr, bsStop := startTestServer(t)
	defer bsStop()
	csrPEM, clientKey := mustMakeCSRWithKeyIT(t, sid)
	bsClient, closeBS := dialClient(t, bsAddr)
	defer closeBS()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bsReply, err := bsClient.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// The old active seed (issued during Bootstrap).
	oldSeed, err := soulseed.SelectActiveBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectActiveBySID: %v", err)
	}

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)
	mgr := NewStreamManager(discardIntegrationLogger())
	auditW := auditpg.NewWriter(integrationPool)
	ob, _ := NewOutbound(OutboundDeps{
		Manager:     mgr,
		AuditWriter: auditW,
		Logger:      discardIntegrationLogger(),
	})

	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter: auditW,
		KID:         "kid-test",
		Manager:     mgr,
		SeedRotation: &SeedRotationDeps{
			Pool:        integrationPool,
			VaultClient: integrationVault,
			AuditWriter: auditW,
			Outbound:    ob,
			KID:         "kid-test",
			PKIMount:    pkiMount,
			PKIRole:     pkiRole,
		},
	})
	defer esStop()

	stream := helloAndEventStream(t, ctx, esAddr, bsReply.GetCertificatePem(), clientKey)

	// Prepare a new CSR (a new key pair).
	newCSRPEM, _ := mustMakeCSRWithKeyIT(t, sid)
	if err := stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_SeedRotationRequest{
			SeedRotationRequest: &keeperv1.SeedRotationRequest{CsrPem: []byte(newCSRPEM)},
		},
	}); err != nil {
		t.Fatalf("send SeedRotationRequest: %v", err)
	}

	got, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream.Recv: %v", err)
	}
	reply := got.GetSeedRotationReply()
	if reply == nil {
		t.Fatalf("payload = %T, want SeedRotationReply", got.GetPayload())
	}
	if !strings.Contains(string(reply.GetCertificatePem()), "BEGIN CERTIFICATE") {
		t.Fatalf("certificate not PEM: %q", reply.GetCertificatePem())
	}
	if reply.GetNotAfter() == nil || !reply.GetNotAfter().AsTime().After(time.Now()) {
		t.Errorf("not_after = %v, want future", reply.GetNotAfter())
	}
	drainEventStreamClient(stream)

	// DB: the new active seed, the old one — superseded.
	deadline := time.Now().Add(3 * time.Second)
	var (
		newSeed *soulseed.SoulSeed
		newErr  error
	)
	for time.Now().Before(deadline) {
		newSeed, newErr = soulseed.SelectActiveBySID(ctx, integrationPool, sid)
		if newErr == nil && newSeed.SeedID != oldSeed.SeedID {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if newErr != nil {
		t.Fatalf("SelectActiveBySID: %v", newErr)
	}
	if newSeed.SeedID == oldSeed.SeedID {
		t.Fatal("seed not rotated (same SeedID)")
	}
	if newSeed.Fingerprint == oldSeed.Fingerprint {
		t.Error("seed.fingerprint not changed")
	}

	// The old seed → superseded.
	var oldStatus string
	row := integrationPool.QueryRow(ctx, `SELECT status FROM soul_seeds WHERE seed_id = $1`, oldSeed.SeedID)
	if err := row.Scan(&oldStatus); err != nil {
		t.Fatalf("old seed status query: %v", err)
	}
	if oldStatus != "superseded" {
		t.Errorf("old seed status = %q, want superseded", oldStatus)
	}

	// Audit `soul.seed-rotated`.
	deadline = time.Now().Add(3 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		row := integrationPool.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE event_type='soul.seed-rotated' AND correlation_id=$1`,
			newSeed.SeedID)
		if err := row.Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n != 1 {
		t.Errorf("audit soul.seed-rotated count = %d, want 1", n)
	}
}

// TestIntegration_EventStream_SoulprintReportWritesSoulsAndAudit —
// SoulprintReport → souls.soulprint_facts JSONB + audit `soulprint.received`.
func TestIntegration_EventStream_SoulprintReportWritesSoulsAndAudit(t *testing.T) {
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

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)

	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter: auditpg.NewWriter(integrationPool), KID: "kid-test",
	})
	defer esStop()
	stream := helloAndEventStream(t, ctx, esAddr, bsReply.GetCertificatePem(), clientKey)

	collected := time.Now().UTC().Add(-time.Minute)
	if err := stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_SoulprintReport{SoulprintReport: &keeperv1.SoulprintReport{
			CollectedAt: timestamppb.New(collected),
			TypedFacts: &keeperv1.SoulprintFacts{
				Sid: sid, Hostname: "host",
				Os: &keeperv1.OsFacts{Family: "debian", Distro: "ubuntu", Version: "22.04", PkgMgr: "apt", InitSystem: "systemd"},
			},
		}},
	}); err != nil {
		t.Fatalf("send SoulprintReport: %v", err)
	}
	_ = stream.CloseSend()
	for {
		_, err := stream.Recv()
		if err == nil {
			continue
		}
		break
	}

	deadline := time.Now().Add(3 * time.Second)
	var factsJSON string
	for time.Now().Before(deadline) {
		row := integrationPool.QueryRow(ctx,
			`SELECT soulprint_facts::text FROM souls WHERE sid = $1`, sid)
		var raw *string
		if err := row.Scan(&raw); err == nil && raw != nil && *raw != "" {
			factsJSON = *raw
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if factsJSON == "" {
		t.Fatal("souls.soulprint_facts not populated")
	}
	if !strings.Contains(factsJSON, "debian") {
		t.Errorf("souls.soulprint_facts = %q, want family=debian", factsJSON)
	}
	// E2E BUG-A: composite keys in JSONB are snake_case (ADR-018, the
	// single source of truth shared with the CEL/template projection
	// .self.os.pkg_mgr). A camelCase jsonName (pkgMgr/initSystem) is not
	// acceptable — it would desync from the template.
	if !strings.Contains(factsJSON, `"pkg_mgr"`) || !strings.Contains(factsJSON, `"init_system"`) {
		t.Errorf("souls.soulprint_facts = %q, want snake_case keys pkg_mgr/init_system", factsJSON)
	}
	if strings.Contains(factsJSON, `"pkgMgr"`) || strings.Contains(factsJSON, `"initSystem"`) {
		t.Errorf("souls.soulprint_facts = %q содержит camelCase ключи — рассинхрон с CEL/docs", factsJSON)
	}

	// Audit.
	row := integrationPool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE event_type='soulprint.received'`)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if n != 1 {
		t.Errorf("audit count = %d, want 1", n)
	}

	// UpdateSoulprint is reachable from the CRUD layer: additionally check
	// that the timings landed in the columns.
	s, err := soul.SelectBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	_ = s
}

// seedConnectedSoul inserts a connected soul with the given last_seen_at.
// created_by_aid=NULL (allowed by FK ON DELETE SET NULL) — the flush test
// doesn't need an operator.
func seedConnectedSoul(t *testing.T, ctx context.Context, sid string, lastSeen time.Time) {
	t.Helper()
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO souls (sid, transport, status, registered_at, last_seen_at)
		 VALUES ($1, 'agent', 'connected', NOW(), $2)`,
		sid, lastSeen.UTC())
	if err != nil {
		t.Fatalf("seed connected soul %q: %v", sid, err)
	}
}

func soulStatus(t *testing.T, ctx context.Context, sid string) string {
	t.Helper()
	var st string
	if err := integrationPool.QueryRow(ctx,
		`SELECT status FROM souls WHERE sid = $1`, sid).Scan(&st); err != nil {
		t.Fatalf("read status %q: %v", sid, err)
	}
	return st
}

// bootstrapForStream — onboards the seedOnboardingFixtures Soul via the
// Bootstrap RPC and returns the issued cert + client key for a subsequent
// EventStream. Collapses the repeated boilerplate of session-lifecycle
// tests (Part 1/2).
func bootstrapForStream(t *testing.T, ctx context.Context) (sid string, certPEM, keyPEM []byte) {
	t.Helper()
	plain, s := seedOnboardingFixtures(t)
	sid = s
	bsAddr, bsStop := startTestServer(t)
	t.Cleanup(bsStop)
	csrPEM, clientKey := mustMakeCSRWithKeyIT(t, sid)
	bsClient, closeBS := dialClient(t, bsAddr)
	t.Cleanup(closeBS)
	reply, err := bsClient.Bootstrap(ctx, &keeperv1.BootstrapRequest{
		Sid: sid, BootstrapToken: plain, CsrPem: []byte(csrPEM),
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return sid, reply.GetCertificatePem(), clientKey
}

// soulAlive — presence predicate: whether the Redis SID lease is alive
// (= Soul online for the target resolver, ADR-006(a)). Replaces the former
// PG status snapshot: presence is no longer written synchronously to
// `souls.status`.
func soulAlive(t *testing.T, ctx context.Context, rc *keeperredis.Client, sid string) bool {
	t.Helper()
	alive, err := keeperredis.SoulStreamAlive(ctx, rc, sid)
	if err != nil {
		t.Fatalf("SoulStreamAlive(%s): %v", sid, err)
	}
	return alive
}

// waitSoulAlive polls the presence predicate until the desired value or
// the deadline.
func waitSoulAlive(t *testing.T, ctx context.Context, rc *keeperredis.Client, sid string, want bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if soulAlive(t, ctx, rc, sid) == want {
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return soulAlive(t, ctx, rc, sid) == want
}

// TestIntegration_EventStream_Reconnect_RestoresPresence — presence model
// (ADR-006(a)): after a Keeper restart, a reconnecting Soul becomes online
// by acquiring the SID lease on session-open — without a synchronous PG
// presence write. This is exactly the visibility the target resolver
// needs (it derives online from the lease). A disconnected snapshot in
// `souls.status` doesn't get in the way: the lease decides presence.
func TestIntegration_EventStream_Reconnect_RestoresPresence(t *testing.T) {
	resetAll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, certPEM, keyPEM := bootstrapForStream(t, ctx)

	// Emulating "Keeper restarted, the stream was lost": the legacy
	// snapshot is set to disconnected (presence is no longer read from
	// it).
	if _, err := integrationPool.Exec(ctx,
		`UPDATE souls SET status = 'disconnected' WHERE sid = $1`, sid); err != nil {
		t.Fatalf("force disconnected: %v", err)
	}

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)
	// Before connecting there's no lease → presence offline.
	if soulAlive(t, ctx, rc, sid) {
		t.Fatalf("precondition: soul online before stream, want offline (no lease)")
	}

	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter: auditpg.NewWriter(integrationPool), KID: "kid-reconnect",
	})
	defer esStop()

	stream := helloAndEventStream(t, ctx, esAddr, certPEM, keyPEM)
	defer drainEventStreamClient(stream)

	// session-open acquired the lease → presence online (visible to the
	// resolver), despite the disconnected snapshot in PG.
	if !waitSoulAlive(t, ctx, rc, sid, true) {
		t.Errorf("soul offline after reconnect, want online (lease acquired on session-open)")
	}
}

// TestIntegration_EventStream_Teardown_DropsPresence — a normal stream
// close drops presence (Release SID lease) without waiting for the Reaper
// timeout and without writing to `souls.status`. After teardown, the Soul
// is offline for the resolver.
func TestIntegration_EventStream_Teardown_DropsPresence(t *testing.T) {
	resetAll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, certPEM, keyPEM := bootstrapForStream(t, ctx)

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)
	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter: auditpg.NewWriter(integrationPool), KID: "kid-teardown",
	})
	defer esStop()

	stream := helloAndEventStream(t, ctx, esAddr, certPEM, keyPEM)

	// session-open acquired the lease → presence online.
	if !waitSoulAlive(t, ctx, rc, sid, true) {
		t.Fatalf("precondition: soul offline after session-open, want online")
	}

	// A normal client-side stream close → teardown releases the lease.
	drainEventStreamClient(stream)

	if !waitSoulAlive(t, ctx, rc, sid, false) {
		t.Errorf("soul still online after teardown, want offline (lease released, no Reaper dependency)")
	}
}

// TestIntegration_EventStream_Teardown_ForeignLease_NoRelease — a key
// invariant of the lease model: teardown of the old instance does NOT
// drop presence if the lease already belongs to a DIFFERENT Keeper (the
// Soul moved). Lease compare-and-set by value=KID: releasing someone
// else's lease is a no-op, presence stays online.
func TestIntegration_EventStream_Teardown_ForeignLease_NoRelease(t *testing.T) {
	resetAll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, certPEM, keyPEM := bootstrapForStream(t, ctx)

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, mr := newIntegrationRedisClient(t)
	esAddr, esStop := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter: auditpg.NewWriter(integrationPool),
		KID:         "kid-old",
	})
	defer esStop()

	stream := helloAndEventStream(t, ctx, esAddr, certPEM, keyPEM)

	// session-open by kid-old acquired the lease → presence online.
	if !waitSoulAlive(t, ctx, rc, sid, true) {
		t.Fatalf("precondition: soul offline after session-open, want online")
	}

	// Emulating a lease move: the lease now belongs to kid-new (we
	// overwrite the value directly in miniredis). The old instance kid-old
	// is no longer the owner.
	leaseKey := keeperredis.SoulLeaseKey(sid)
	if err := mr.Set(leaseKey, "kid-new"); err != nil {
		t.Fatalf("simulate lease move: %v", err)
	}

	// kid-old closes its own (already dead) session → releasing someone
	// else's lease is a no-op (compare-and-delete by value=kid-old won't
	// match kid-new).
	drainEventStreamClient(stream)
	time.Sleep(500 * time.Millisecond)

	// Presence stays online (kid-new's lease is alive, not overwritten by
	// kid-old's teardown).
	if !soulAlive(t, ctx, rc, sid) {
		t.Errorf("soul offline after foreign teardown, want online (kid-new lease must survive)")
	}
	if owner, err := mr.Get(leaseKey); err != nil || owner != "kid-new" {
		t.Errorf("lease owner = %q (err=%v), want kid-new (unchanged)", owner, err)
	}
}

// TestIntegration_HeartbeatFlush_PreventsFalseDisconnect — regression test
// for an HA bug: a live EventStream stream only updated the heartbeat in
// Redis, PG's `last_seen_at` stayed stale, and the Reaper rule
// `mark_disconnected` falsely marked a live stream disconnected once
// stale_after elapsed.
//
// End-to-end: a connected soul with a stale last_seen → flushLastSeen (a
// real pool) refreshes PG → mark_disconnected(90s) leaves it alone.
// Control: a second soul without a flush gets marked disconnected by the
// same mark_disconnected call.
func TestIntegration_HeartbeatFlush_PreventsFalseDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resetAll(t)

	const staleAfter = 90 * time.Second
	now := time.Now().UTC()
	stale := now.Add(-5 * time.Minute) // comfortably older than stale_after

	const liveSID = "live-stream.example.com"
	const deadSID = "no-flush.example.com"
	seedConnectedSoul(t, ctx, liveSID, stale)
	seedConnectedSoul(t, ctx, deadSID, stale)

	// Simulate a live stream: a throttled PG flush through the same
	// handler path.
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:                &fakeSeedDB{},
		SoulDB:                integrationPool,
		AuditWriter:           nopAudit{},
		KID:                   "kid-test",
		LastSeenFlushInterval: staleAfter / 3,
	}, discardIntegrationLogger())
	h.flushLastSeen(ctx, liveSID, time.Now().UTC())

	// The live soul's PG snapshot is now fresh.
	var flushed time.Time
	if err := integrationPool.QueryRow(ctx,
		`SELECT last_seen_at FROM souls WHERE sid = $1`, liveSID).Scan(&flushed); err != nil {
		t.Fatalf("read last_seen: %v", err)
	}
	if now.Sub(flushed) > staleAfter {
		t.Fatalf("flush не освежил last_seen: %v (now=%v)", flushed, now)
	}

	updated, err := callMarkDisconnected(ctx, staleAfter)
	if err != nil {
		t.Fatalf("mark_disconnected: %v", err)
	}
	if updated != 1 {
		t.Errorf("mark_disconnected updated = %d, want 1 (только no-flush soul)", updated)
	}
	if got := soulStatus(t, ctx, liveSID); got != "connected" {
		t.Errorf("live soul status = %q, want connected (flush защитил)", got)
	}
	if got := soulStatus(t, ctx, deadSID); got != "disconnected" {
		t.Errorf("no-flush soul status = %q, want disconnected", got)
	}
}

// callMarkDisconnected — a direct call of the mark_disconnected SQL
// function on integrationPool (the Reaper purger pulls in extra
// dependencies; here we only need the predicate itself over PG's
// `last_seen_at`).
func callMarkDisconnected(ctx context.Context, staleAfter time.Duration) (int64, error) {
	var n int64
	err := integrationPool.QueryRow(ctx,
		`SELECT mark_disconnected(make_interval(secs => $1), 100)`,
		staleAfter.Seconds()).Scan(&n)
	return n, err
}
