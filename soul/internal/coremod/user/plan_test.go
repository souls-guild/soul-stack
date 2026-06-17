package user_test

import (
	"context"
	osuser "os/user"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/user"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// planStream — fake stream для Plan.
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

// TestPlan_Present_Exists_Clean — Plan(present) для существующего пользователя:
// changed=false, без мутаций.
func TestPlan_Present_Exists_Clean(t *testing.T) {
	r := internaltest.NewRunner()
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return &osuser.User{Username: name, Uid: "1001", Gid: "1001"}, nil
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

// TestPlan_Present_Missing_Drift — Plan(present) для отсутствующего пользователя:
// changed=true (Apply создал бы), без мутаций.
func TestPlan_Present_Missing_Drift(t *testing.T) {
	r := internaltest.NewRunner()
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
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

// TestPlan_Absent_Exists_Drift — Plan(absent) для существующего пользователя:
// changed=true (Apply удалил бы), без мутаций.
func TestPlan_Absent_Exists_Drift(t *testing.T) {
	r := internaltest.NewRunner()
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return &osuser.User{Username: name, Uid: "1001", Gid: "1001"}, nil
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

// TestPlan_Absent_Missing_Clean — Plan(absent) для отсутствующего пользователя:
// changed=false, без мутаций.
func TestPlan_Absent_Missing_Clean(t *testing.T) {
	r := internaltest.NewRunner()
	m := &user.Module{
		Runner: r,
		LookupUser: func(name string) (*osuser.User, error) {
			return nil, osuser.UnknownUserError(name)
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
