package firewall_test

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/firewall"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

type planStream struct {
	grpc.ServerStreamingServer[pluginv1.PlanEvent]
	events []*pluginv1.PlanEvent
}

func (s *planStream) Send(e *pluginv1.PlanEvent) error { s.events = append(s.events, e); return nil }
func (s *planStream) Context() context.Context         { return context.Background() }
func (s *planStream) last() *pluginv1.PlanEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}

// assertNoMutatingFirewallCalls фейлит, если runner получил allow/deny/delete/add/remove/reload.
func assertNoMutatingFirewallCalls(t *testing.T, r *internaltest.Runner) {
	t.Helper()
	for _, c := range r.Calls {
		for _, bad := range []string{
			"ufw allow", "ufw deny", "ufw delete", "ufw enable", "ufw default",
			"firewall-cmd --permanent", "firewall-cmd --reload", "firewall-cmd --add", "firewall-cmd --remove",
		} {
			if strings.Contains(c, bad) {
				t.Fatalf("Plan вызвал мутирующую команду %q (должен быть pure-read)", c)
			}
		}
	}
}

func planOn(t *testing.T, r *internaltest.Runner, state string, params map[string]any) *planStream {
	t.Helper()
	m := &firewall.Module{Runner: r}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{State: state, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return stream
}

// TestPlan_UFW_Present_AlreadyPresent_Clean — 80/tcp уже в ufw status → clean.
func TestPlan_UFW_Present_AlreadyPresent_Clean(t *testing.T) {
	r := ufwRunner()
	stream := planOn(t, r, "present", map[string]any{"port": 80, "proto": "tcp"})
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertNoMutatingFirewallCalls(t, r)
}

// TestPlan_UFW_Present_Missing_Drift — 9999/tcp нет в status → drift.
func TestPlan_UFW_Present_Missing_Drift(t *testing.T) {
	r := ufwRunner()
	stream := planOn(t, r, "present", map[string]any{"port": 9999, "proto": "tcp"})
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertNoMutatingFirewallCalls(t, r)
}

// TestPlan_UFW_Absent_Present_Drift — 80/tcp в status → drift (Apply удалил бы).
func TestPlan_UFW_Absent_Present_Drift(t *testing.T) {
	r := ufwRunner()
	stream := planOn(t, r, "absent", map[string]any{"port": 80, "proto": "tcp"})
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertNoMutatingFirewallCalls(t, r)
}

// TestPlan_UFW_Absent_Missing_Clean — 9999/tcp нет → clean.
func TestPlan_UFW_Absent_Missing_Clean(t *testing.T) {
	r := ufwRunner()
	stream := planOn(t, r, "absent", map[string]any{"port": 9999, "proto": "tcp"})
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertNoMutatingFirewallCalls(t, r)
}

// TestPlan_Firewalld_Present_Match_Clean — port 80/tcp в list-ports → clean.
func TestPlan_Firewalld_Present_Match_Clean(t *testing.T) {
	r := firewalldRunner()
	stream := planOn(t, r, "present", map[string]any{"port": 80, "proto": "tcp"})
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertNoMutatingFirewallCalls(t, r)
}

// TestPlan_Firewalld_Present_Missing_Drift — порт не в list-ports → drift.
func TestPlan_Firewalld_Present_Missing_Drift(t *testing.T) {
	r := firewalldRunner()
	stream := planOn(t, r, "present", map[string]any{"port": 9999, "proto": "tcp"})
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertNoMutatingFirewallCalls(t, r)
}
