package grpc

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// captureAudit — собирает audit-event-ы для проверки в тестах.
type captureAudit struct {
	mu     sync.Mutex
	events []*audit.Event
	failOn audit.EventType
}

func (c *captureAudit) Write(_ context.Context, ev *audit.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failOn != "" && ev.EventType == c.failOn {
		return errors.New("audit write forced failure")
	}
	c.events = append(c.events, ev)
	return nil
}

func (c *captureAudit) snapshot() []*audit.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*audit.Event, len(c.events))
	copy(out, c.events)
	return out
}

func TestNewOutbound_Validation(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	cases := []struct {
		name    string
		manager *StreamManager
		audit   audit.Writer
		logger  bool
		want    string
	}{
		{"nil manager", nil, nopAudit{}, true, "manager is required"},
		{"nil audit", m, nil, true, "auditWriter is required"},
		{"nil logger", m, nopAudit{}, false, "logger is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var lg = discardLogger(t)
			if !c.logger {
				lg = nil
			}
			_, err := NewOutbound(OutboundDeps{
				Manager:     c.manager,
				AuditWriter: c.audit,
				Logger:      lg,
			})
			if err == nil || !contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// newOutboundForTest — короткий helper для тестов, не требующих
// cluster-routing-а (Redis=nil → single-instance fallback).
func newOutboundForTest(t *testing.T, m *StreamManager, a audit.Writer) *Outbound {
	t.Helper()
	ob, err := NewOutbound(OutboundDeps{
		Manager:     m,
		AuditWriter: a,
		Logger:      discardLogger(t),
	})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	return ob
}

func TestOutbound_SendApply_NotConnected(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	ob := newOutboundForTest(t, m, nopAudit{})
	err := ob.SendApply(context.Background(), "host.example.com",
		&keeperv1.ApplyRequest{ApplyId: "01HABC"})
	if !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("err = %v, want ErrSoulNotConnected", err)
	}
}

func TestOutbound_SendApply_HappyPath(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("host.example.com")
	ca := &captureAudit{}
	ob := newOutboundForTest(t, m, ca)

	req := &keeperv1.ApplyRequest{
		ApplyId: "01HABC",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "t1", Module: "core.pkg.installed"},
			{Name: "t2", Module: "core.file.present"},
		},
	}
	if err := ob.SendApply(context.Background(), "host.example.com", req); err != nil {
		t.Fatalf("SendApply: %v", err)
	}

	msg, ok := <-out
	if !ok {
		t.Fatal("channel closed unexpectedly")
	}
	got := msg.GetApplyRequest()
	if got == nil {
		t.Fatalf("payload = %T, want ApplyRequest", msg.GetPayload())
	}
	if got.GetApplyId() != "01HABC" || len(got.GetTasks()) != 2 {
		t.Errorf("apply_id = %q, tasks = %d", got.GetApplyId(), len(got.GetTasks()))
	}

	evs := ca.snapshot()
	if len(evs) != 1 {
		t.Fatalf("audit count = %d, want 1", len(evs))
	}
	if evs[0].EventType != audit.EventApplyDispatched {
		t.Errorf("event_type = %q, want apply.dispatched", evs[0].EventType)
	}
	if evs[0].CorrelationID != "01HABC" {
		t.Errorf("correlation_id = %q", evs[0].CorrelationID)
	}
	if evs[0].Payload["tasks_count"].(int) != 2 {
		t.Errorf("tasks_count = %v", evs[0].Payload["tasks_count"])
	}
}

func TestOutbound_SendApply_QueueFull(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	_ = m.Register("sid")
	ob := newOutboundForTest(t, m, nopAudit{})

	for i := 0; i < outboundBufferSize; i++ {
		if err := ob.SendApply(context.Background(), "sid", &keeperv1.ApplyRequest{ApplyId: "x"}); err != nil {
			t.Fatalf("SendApply #%d: %v", i, err)
		}
	}
	err := ob.SendApply(context.Background(), "sid", &keeperv1.ApplyRequest{ApplyId: "x"})
	if !errors.Is(err, ErrOutboundQueueFull) {
		t.Fatalf("err = %v, want ErrOutboundQueueFull", err)
	}
}

func TestOutbound_SendCancel_HappyPath(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("sid")
	ca := &captureAudit{}
	ob := newOutboundForTest(t, m, ca)

	if err := ob.SendCancel(context.Background(), "sid", "01HABC", "test cancel"); err != nil {
		t.Fatalf("SendCancel: %v", err)
	}
	msg := <-out
	cancel := msg.GetCancelApply()
	if cancel == nil {
		t.Fatalf("payload = %T, want CancelApply", msg.GetPayload())
	}
	if cancel.GetApplyId() != "01HABC" || cancel.GetReason() != "test cancel" {
		t.Errorf("CancelApply = %+v", cancel)
	}
	evs := ca.snapshot()
	if len(evs) != 1 || evs[0].EventType != audit.EventApplyCancelled {
		t.Errorf("audit = %+v, want apply.cancelled", evs)
	}
}

func TestOutbound_SendCancel_EmptyApplyID(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	ob := newOutboundForTest(t, m, nopAudit{})
	err := ob.SendCancel(context.Background(), "sid", "", "x")
	if err == nil || !contains(err.Error(), "applyID is empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestOutbound_SendSeedRotationReply_NoAuditFromOutbound(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("sid")
	ca := &captureAudit{}
	ob := newOutboundForTest(t, m, ca)

	reply := &keeperv1.SeedRotationReply{CertificatePem: []byte("cert"), CaChainPem: []byte("ca")}
	if err := ob.SendSeedRotationReply(context.Background(), "sid", reply); err != nil {
		t.Fatalf("SendSeedRotationReply: %v", err)
	}
	msg := <-out
	if got := msg.GetSeedRotationReply(); got == nil {
		t.Fatalf("payload = %T, want SeedRotationReply", msg.GetPayload())
	}
	// Outbound сам audit-event для rotation НЕ пишет (это делает handler).
	if evs := ca.snapshot(); len(evs) != 0 {
		t.Errorf("audit = %+v, want empty", evs)
	}
}

func TestOutbound_SendSigilSnapshot_HappyPath(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("sid")
	ca := &captureAudit{}
	ob := newOutboundForTest(t, m, ca)

	set := []*keeperv1.PluginSigil{
		{
			Namespace:    "core",
			Name:         "template",
			Ref:          "v1.0.0",
			BinarySha256: "abc123",
			Signature:    []byte("sig"),
			Manifest:     []byte("raw-manifest-bytes"),
		},
		{Namespace: "cloud", Name: "hetzner", Ref: "v2", BinarySha256: "def456"},
	}
	if err := ob.SendSigilSnapshot(context.Background(), "sid", set); err != nil {
		t.Fatalf("SendSigilSnapshot: %v", err)
	}
	msg := <-out
	snap := msg.GetSigilSnapshot()
	if snap == nil {
		t.Fatalf("payload = %T, want SigilSnapshot", msg.GetPayload())
	}
	if len(snap.GetSigils()) != 2 {
		t.Fatalf("snapshot sigils = %d, want 2", len(snap.GetSigils()))
	}
	if snap.GetSigils()[0].GetName() != "template" || snap.GetSigils()[1].GetName() != "hetzner" {
		t.Errorf("snapshot order/identity = %+v", snap.GetSigils())
	}
	// Outbound — чистая «трубопроводная» функция: audit для раздачи snapshot-а
	// не пишется (он зафиксирован на plugin.allow/plugin.revoke).
	if evs := ca.snapshot(); len(evs) != 0 {
		t.Errorf("audit = %+v, want empty", evs)
	}
}

// TestOutbound_SendSigilSnapshot_Empty — пустой/nil snapshot валиден и шлётся как
// факт «ни один плагин не допущен» (ReplaceAll на Soul-е стирает старый набор).
func TestOutbound_SendSigilSnapshot_Empty(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("sid")
	ob := newOutboundForTest(t, m, nopAudit{})

	if err := ob.SendSigilSnapshot(context.Background(), "sid", nil); err != nil {
		t.Fatalf("SendSigilSnapshot(nil): %v", err)
	}
	msg := <-out
	snap := msg.GetSigilSnapshot()
	if snap == nil {
		t.Fatalf("payload = %T, want SigilSnapshot", msg.GetPayload())
	}
	if len(snap.GetSigils()) != 0 {
		t.Fatalf("empty snapshot sigils = %d, want 0", len(snap.GetSigils()))
	}
}

func TestOutbound_SendSigilSnapshot_NotConnected(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	ob := newOutboundForTest(t, m, nopAudit{})
	err := ob.SendSigilSnapshot(context.Background(), "sid",
		[]*keeperv1.PluginSigil{{Namespace: "core", Name: "template"}})
	if !errors.Is(err, ErrSoulNotConnected) {
		t.Fatalf("err = %v, want ErrSoulNotConnected", err)
	}
}
