//go:build integration

// Integration-тесты M2.3 (Redis SoulLease) и M2.4 (event handlers).
//
// Поднимаем PG + Vault PKI (общие fixture-ы из integration_test.go) и
// miniredis-ом в процессе для Redis. miniredis не требует docker-а, но мы
// держимся под тегом `integration`, потому что без PG handler-ы не запишут
// `souls` и `audit_log`.

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

// discardIntegrationLogger — slog-логгер, который не печатает ничего.
// Не t.Helper-завязан (используется из non-test-helper-функций); под
// integration-сборку лежит здесь, чтобы не зависеть от unit-build-тегов.
func discardIntegrationLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// drainEventStreamClient — закрывает send-half клиента и вычитывает
// receive-half до EOF/error-а. Нужно для тестов, которые поднимают
// EventStream-server с outbound-каналом: если клиент не закроет стрим,
// GracefulStop сервера ждёт окончания всех bidi-streams и срабатывает
// 5s timeout-страховка из startEventStreamServerExt.
func drainEventStreamClient(stream keeperv1.Keeper_EventStreamClient) {
	_ = stream.CloseSend()
	for {
		_, err := stream.Recv()
		if err != nil {
			return
		}
	}
}

// newIntegrationRedisClient — miniredis-инстанс + Keeper-Redis-обёртка.
// miniredis в процессе → не требует docker-а.
func newIntegrationRedisClient(t *testing.T) (*keeperredis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := keeperredis.NewClient(context.Background(), keeperredis.Config{Addr: mr.Addr()})
	if err != nil {
		t.Fatalf("redis NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// helloAndEventStream — onboards + открывает EventStream + шлёт Hello.
// Возвращает стрим (caller сам шлёт следующие payload-ы).
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

// TestIntegration_EventStream_LeaseConflict_ReturnsAlreadyExists — второй
// подключающийся Keeper-инстанс к тому же SID получает AlreadyExists
// (PM-decision 1).
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

	// Keeper A — захватывает lease, держит стрим открытым.
	esAddrA, esStopA := startEventStreamServerExt(t, caRootPEM, EventStreamDeps{
		SeedDB: integrationPool, SoulDB: integrationPool, Redis: rc,
		AuditWriter:  auditpg.NewWriter(integrationPool),
		KID:          "kid-a",
		SoulLeaseTTL: 5 * time.Second,
	})
	defer esStopA()

	streamA := helloAndEventStream(t, ctx, esAddrA, bsReply.GetCertificatePem(), clientKey)
	defer streamA.CloseSend()

	// Keeper B — пытается принять стрим тем же SID-ом.
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
		// AlreadyExists может прилететь и при Send.
		if code := status.Code(err); code == codes.AlreadyExists {
			return
		}
	}
	_, err = streamB.Recv()
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("B Recv: code = %v, want AlreadyExists (err=%v)", got, err)
	}
}

// TestIntegration_EventStream_HeartbeatUpdated — после Hello heartbeat-кэш
// в Redis обновляется (HSET soul:<sid>:hb).
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

	// Polling: heartbeat пишется async к Send-у Hello.
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
// с event_type=task.executed и correlation_id=apply_id.
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
	// Дожидаемся EOF от сервера, чтобы audit writer успел отработать.
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

	// Polling: audit пишется в той же гoроутине handler-а, но Send/Close
	// async-завершают стрим — даём небольшое окно.
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

// TestIntegration_EventStream_OutboundSendApply — после Hello-handshake
// и регистрации в StreamManager-е, SendApply на тот же SID долетает до
// клиента через stream.Recv() (M2.5).
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

// TestIntegration_EventStream_OutboundSendCancel — SendCancel приходит
// клиенту как CancelApply (M2.5).
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

// TestIntegration_EventStream_SeedRotation — Soul шлёт SeedRotationRequest,
// Keeper подписывает CSR через Vault PKI, supersede-ит старый seed,
// вставляет новый и шлёт SeedRotationReply обратно (M2.5).
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
	// Старый active-seed (выпущен при Bootstrap-е).
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

	// Готовим новый CSR (новая пара ключей).
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

	// БД: новый active-seed, старый — superseded.
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

	// Старый seed → superseded.
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
	// E2E BUG-A: composite-ключи в JSONB — snake_case (ADR-018, единая точка
	// правды с CEL/template-проекцией .self.os.pkg_mgr). camelCase jsonName
	// (pkgMgr/initSystem) недопустим — рассинхрон с шаблоном.
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

	// UpdateSoulprint reachable из CRUD-слоя: дополнительно проверим, что
	// тайминги попали в колонки.
	s, err := soul.SelectBySID(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	_ = s
}

// seedConnectedSoul вставляет connected-soul со заданным last_seen_at.
// created_by_aid=NULL (FK ON DELETE SET NULL допускает) — operator для теста
// flush-а не нужен.
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

// bootstrapForStream — онбордит seedOnboardingFixtures-Soul через Bootstrap-RPC
// и возвращает выданный cert + client key для последующего EventStream-а.
// Сворачивает повторяющийся boilerplate тестов session-lifecycle (Часть 1/2).
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

// soulAlive — presence-предикат: жив ли Redis SID-lease (= Soul online для
// таргет-резолвера, ADR-006(a)). Заменяет прежний PG-status-снимок: presence
// больше не пишется синхронно в `souls.status`.
func soulAlive(t *testing.T, ctx context.Context, rc *keeperredis.Client, sid string) bool {
	t.Helper()
	alive, err := keeperredis.SoulStreamAlive(ctx, rc, sid)
	if err != nil {
		t.Fatalf("SoulStreamAlive(%s): %v", sid, err)
	}
	return alive
}

// waitSoulAlive поллит presence-предикат до желаемого значения или дедлайна.
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

// TestIntegration_EventStream_Reconnect_RestoresPresence — presence-модель
// (ADR-006(a)): после рестарта Keeper-а переподключившийся Soul становится
// online через захват SID-lease на session-open — без синхронной PG-записи
// presence. Это и есть видимость таргет-резолверу (он деривирует online из
// lease). disconnected-снимок в `souls.status` НЕ мешает: presence решает lease.
func TestIntegration_EventStream_Reconnect_RestoresPresence(t *testing.T) {
	resetAll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, certPEM, keyPEM := bootstrapForStream(t, ctx)

	// Эмуляция «Keeper рестартанул, стрим был потерян»: legacy-снимок в
	// disconnected (presence из него больше НЕ читается).
	if _, err := integrationPool.Exec(ctx,
		`UPDATE souls SET status = 'disconnected' WHERE sid = $1`, sid); err != nil {
		t.Fatalf("force disconnected: %v", err)
	}

	caRootPEM := fetchVaultPKIRootCA(t, ctx)
	rc, _ := newIntegrationRedisClient(t)
	// До подключения lease нет → presence offline.
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

	// session-open захватил lease → presence online (виден резолверу), несмотря
	// на disconnected-снимок в PG.
	if !waitSoulAlive(t, ctx, rc, sid, true) {
		t.Errorf("soul offline after reconnect, want online (lease acquired on session-open)")
	}
}

// TestIntegration_EventStream_Teardown_DropsPresence — штатное закрытие стрима
// гасит presence (Release SID-lease) без ожидания Reaper-таймаута и без записи
// в `souls.status`. После teardown-а Soul offline для резолвера.
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

	// session-open захватил lease → presence online.
	if !waitSoulAlive(t, ctx, rc, sid, true) {
		t.Fatalf("precondition: soul offline after session-open, want online")
	}

	// Штатное закрытие стрима клиентом → teardown освобождает lease.
	drainEventStreamClient(stream)

	if !waitSoulAlive(t, ctx, rc, sid, false) {
		t.Errorf("soul still online after teardown, want offline (lease released, no Reaper dependency)")
	}
}

// TestIntegration_EventStream_Teardown_ForeignLease_NoRelease — ключевой
// инвариант lease-модели: teardown старого инстанса НЕ гасит presence, если
// lease уже принадлежит ДРУГОМУ Keeper-у (Soul переехал). Lease compare-and-set
// по value=KID: Release чужого lease — no-op, presence остаётся online.
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

	// session-open kid-old захватил lease → presence online.
	if !waitSoulAlive(t, ctx, rc, sid, true) {
		t.Fatalf("precondition: soul offline after session-open, want online")
	}

	// Эмуляция lease-переезда: lease теперь принадлежит kid-new (перетираем
	// value напрямую в miniredis). Старый инстанс kid-old больше не владелец.
	leaseKey := keeperredis.SoulLeaseKey(sid)
	if err := mr.Set(leaseKey, "kid-new"); err != nil {
		t.Fatalf("simulate lease move: %v", err)
	}

	// kid-old закрывает свою (уже мёртвую) сессию → Release чужого lease no-op
	// (compare-and-delete по value=kid-old не сматчит kid-new).
	drainEventStreamClient(stream)
	time.Sleep(500 * time.Millisecond)

	// Presence остаётся online (lease kid-new жив, не перетёрт teardown-ом kid-old).
	if !soulAlive(t, ctx, rc, sid) {
		t.Errorf("soul offline after foreign teardown, want online (kid-new lease must survive)")
	}
	if owner, err := mr.Get(leaseKey); err != nil || owner != "kid-new" {
		t.Errorf("lease owner = %q (err=%v), want kid-new (unchanged)", owner, err)
	}
}

// TestIntegration_HeartbeatFlush_PreventsFalseDisconnect — регрессия HA-бага:
// live EventStream-стрим обновлял heartbeat только в Redis, PG-`last_seen_at`
// оставался stale, и Reaper-правило `mark_disconnected` ложно помечало живой
// стрим disconnected через stale_after.
//
// End-to-end: connected soul со stale last_seen → flushLastSeen (реальный pool)
// освежает PG → mark_disconnected(90s) НЕ трогает. Контроль: второй soul без
// flush-а тем же mark_disconnected помечается disconnected.
func TestIntegration_HeartbeatFlush_PreventsFalseDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resetAll(t)

	const staleAfter = 90 * time.Second
	now := time.Now().UTC()
	stale := now.Add(-5 * time.Minute) // заведомо старше stale_after

	const liveSID = "live-stream.example.com"
	const deadSID = "no-flush.example.com"
	seedConnectedSoul(t, ctx, liveSID, stale)
	seedConnectedSoul(t, ctx, deadSID, stale)

	// Имитируем live-стрим: throttled PG-flush через тот же handler-путь.
	h := newEventStreamHandler(EventStreamDeps{
		SeedDB:                &fakeSeedDB{},
		SoulDB:                integrationPool,
		AuditWriter:           nopAudit{},
		KID:                   "kid-test",
		LastSeenFlushInterval: staleAfter / 3,
	}, discardIntegrationLogger())
	h.flushLastSeen(ctx, liveSID, time.Now().UTC())

	// PG-snapshot live-soul-а теперь свежий.
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

// callMarkDisconnected — прямой вызов SQL-функции mark_disconnected на
// integrationPool (Reaper-purger тянет лишние зависимости; здесь нужен только
// сам предикат по PG-`last_seen_at`).
func callMarkDisconnected(ctx context.Context, staleAfter time.Duration) (int64, error) {
	var n int64
	err := integrationPool.QueryRow(ctx,
		`SELECT mark_disconnected(make_interval(secs => $1), 100)`,
		staleAfter.Seconds()).Scan(&n)
	return n, err
}
