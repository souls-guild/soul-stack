package group_test

import (
	"context"
	osuser "os/user"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/group"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// planStream — fake stream для Plan, захватывает события.
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

// TestPlan_Present_Exists_Clean — Plan(present) для существующей группы:
// changed=false, без мутаций.
func TestPlan_Present_Exists_Clean(t *testing.T) {
	r := internaltest.NewRunner()
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return &osuser.Group{Name: name, Gid: "200"}, nil
		},
	}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	if len(r.Calls) > 0 {
		t.Fatalf("Plan вызвал runner-команды: %v (должен быть pure-read)", r.Calls)
	}
}

// TestPlan_Present_Missing_Drift — Plan(present) для отсутствующей группы:
// changed=true (Apply создал бы), без мутаций.
func TestPlan_Present_Missing_Drift(t *testing.T) {
	r := internaltest.NewRunner()
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return nil, osuser.UnknownGroupError(name)
		},
	}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift: missing)")
	}
	if len(r.Calls) > 0 {
		t.Fatalf("Plan вызвал runner-команды: %v (должен быть pure-read)", r.Calls)
	}
}

// TestPlan_Absent_Exists_Drift — Plan(absent) для существующей группы:
// changed=true (Apply удалил бы), без мутаций.
func TestPlan_Absent_Exists_Drift(t *testing.T) {
	r := internaltest.NewRunner()
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return &osuser.Group{Name: name, Gid: "200"}, nil
		},
	}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift: exists)")
	}
	if len(r.Calls) > 0 {
		t.Fatalf("Plan вызвал runner-команды: %v (должен быть pure-read)", r.Calls)
	}
}

// TestPlan_Absent_Missing_Clean — Plan(absent) для отсутствующей группы:
// changed=false, без мутаций.
func TestPlan_Absent_Missing_Clean(t *testing.T) {
	r := internaltest.NewRunner()
	m := &group.Module{
		Runner: r,
		LookupGroup: func(name string) (*osuser.Group, error) {
			return nil, osuser.UnknownGroupError(name)
		},
	}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	if len(r.Calls) > 0 {
		t.Fatalf("Plan вызвал runner-команды: %v (должен быть pure-read)", r.Calls)
	}
}
