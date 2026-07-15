package push

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// orderingDeliverer and orderingSession capture ordering: Deliver must happen
// AFTER Dial and BEFORE the `soul apply` exec. Otherwise delivery either
// targets an unconnected session, or a stale binary gets to run first.
type orderingDeliverer struct {
	mu       sync.Mutex
	deliverN int
	err      error
	gotSpec  SoulSpec
}

func (d *orderingDeliverer) Deliver(_ context.Context, _ Session, spec SoulSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deliverN++
	d.gotSpec = spec
	return d.err
}

type orderingCleaner struct {
	mu       sync.Mutex
	cleanupN int
	err      error
	sessions []Session
}

func (c *orderingCleaner) Cleanup(_ context.Context, sess Session) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanupN++
	c.sessions = append(c.sessions, sess)
	return c.err
}

// orderingSession records the timing of the Run call relative to Deliver via
// a shared trace list.
type orderingSession struct {
	stdout string
	trace  *[]string
	mu     *sync.Mutex
}

func (s *orderingSession) Run(_ context.Context, _ string, _ []byte) (string, error) {
	s.mu.Lock()
	*s.trace = append(*s.trace, "session.Run")
	s.mu.Unlock()
	return s.stdout, nil
}
func (s *orderingSession) Close() error { return nil }

// tracingDeliverer writes to a shared trace to verify the order
// Dial → Deliver → session.Run.
type tracingDeliverer struct {
	trace *[]string
	mu    *sync.Mutex
}

func (d *tracingDeliverer) Deliver(_ context.Context, _ Session, _ SoulSpec) error {
	d.mu.Lock()
	*d.trace = append(*d.trace, "deliver")
	d.mu.Unlock()
	return nil
}

func TestSendApply_DeliverHappensBeforeExec(t *testing.T) {
	var trace []string
	var mu sync.Mutex
	sess := &orderingSession{stdout: successStdout(t, "ap-deliv-1"), trace: &trace, mu: &mu}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Deliverer: &tracingDeliverer{trace: &trace, mu: &mu},
		SoulSpec:  SoulSpec{SoulBinaryPath: "/local/soul"},
		Dial: func(_ context.Context, _ DialConfig) (Session, error) {
			mu.Lock()
			trace = append(trace, "dial")
			mu.Unlock()
			return sess, nil
		},
	})

	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-deliv-1"})
	if err != nil {
		t.Fatalf("SendApply: %v", err)
	}
	want := []string{"dial", "deliver", "session.Run"}
	if len(trace) != len(want) {
		t.Fatalf("trace=%v, want %v", trace, want)
	}
	for i := range want {
		if trace[i] != want[i] {
			t.Errorf("trace[%d]=%q, want %q (full: %v)", i, trace[i], want[i], trace)
		}
	}
}

func TestSendApply_DeliverFailAbortsExec(t *testing.T) {
	sess := &mockSession{stdout: successStdout(t, "ap-deliv-2")}
	deliv := &orderingDeliverer{err: errors.New("upload failed")}

	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Deliverer: deliv,
		SoulSpec:  SoulSpec{SoulBinaryPath: "/local/soul"},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return sess, nil },
	})

	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-deliv-2"})
	if err == nil {
		t.Fatal("ждали fail-closed на ошибке доставки")
	}
	if len(sess.gotStdin) != 0 {
		t.Errorf("exec не должен был случиться (stdin пуст)")
	}
}

func TestSendApply_DelivererNilSkipsDelivery_S0BC(t *testing.T) {
	// S0-flow: Deliverer is not configured, delivery should be skipped, the
	// run still works via the pre-installed soul binary.
	sess := &mockSession{stdout: successStdout(t, "ap-deliv-3")}
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return sess, nil },
	})
	if _, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-deliv-3"}); err != nil {
		t.Fatalf("S0-flow без Deliverer должен работать: %v", err)
	}
}

func TestSendApply_PropagatesSoulSpecToDeliverer(t *testing.T) {
	sess := &mockSession{stdout: successStdout(t, "ap-deliv-4")}
	deliv := &orderingDeliverer{}
	spec := SoulSpec{
		SoulBinaryPath: "/local/soul-v2",
		Modules:        []ModuleSpec{{Name: "soul-mod-pkg", Path: "/local/mod"}},
	}
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Deliverer: deliv,
		SoulSpec:  spec,
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return sess, nil },
	})
	if _, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-deliv-4"}); err != nil {
		t.Fatalf("SendApply: %v", err)
	}
	if deliv.deliverN != 1 {
		t.Fatalf("Deliver вызван %d раз, want 1", deliv.deliverN)
	}
	if deliv.gotSpec.SoulBinaryPath != spec.SoulBinaryPath {
		t.Errorf("SoulBinaryPath=%q, want %q", deliv.gotSpec.SoulBinaryPath, spec.SoulBinaryPath)
	}
	if len(deliv.gotSpec.Modules) != 1 || deliv.gotSpec.Modules[0].Name != "soul-mod-pkg" {
		t.Errorf("Modules не доехали: %+v", deliv.gotSpec.Modules)
	}
}

func TestDispatcher_Cleanup_UsesCleaner(t *testing.T) {
	cleaner := &orderingCleaner{}
	sess := &mockSession{}
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Cleaner:   cleaner,
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return sess, nil },
	})
	if err := disp.Cleanup(context.Background(), "host-1.example.com", testProviderName); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if cleaner.cleanupN != 1 {
		t.Errorf("Cleaner вызван %d раз, want 1", cleaner.cleanupN)
	}
	if !sess.closed {
		t.Error("сессия не закрыта после Cleanup")
	}
}

func TestDispatcher_Cleanup_NoCleanerConfigured(t *testing.T) {
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return &mockSession{}, nil },
	})
	err := disp.Cleanup(context.Background(), "host-1.example.com", testProviderName)
	if err == nil {
		t.Fatal("без Cleaner cleanup должен возвращать ошибку")
	}
}

func TestDispatcher_Cleanup_RejectsNonSSHTransport(t *testing.T) {
	agentSoul := &soul.Soul{SID: "host-1.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected}
	dialed := false
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: agentSoul},
		Cleaner:   &orderingCleaner{},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { dialed = true; return &mockSession{}, nil },
	})
	if err := disp.Cleanup(context.Background(), "host-1.example.com", testProviderName); err == nil {
		t.Fatal("ждали ошибку для transport=agent")
	}
	if dialed {
		t.Error("connect не должен происходить для non-ssh при Cleanup")
	}
}

func TestDispatcher_Cleanup_AuthorizeDeny(t *testing.T) {
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: false, authReason: "deny", signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Cleaner:   &orderingCleaner{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return &mockSession{}, nil },
	})
	if err := disp.Cleanup(context.Background(), "host-1.example.com", testProviderName); err == nil {
		t.Fatal("ждали отказ Authorize → ошибка Cleanup")
	}
}
