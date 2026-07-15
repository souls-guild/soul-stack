package sysctl_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/sysctl"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

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

// assertNoMutatingSysctlCalls fails if the runner received `-w` or mkdir/write.
func assertNoMutatingSysctlCalls(t *testing.T, r *internaltest.Runner) {
	t.Helper()
	for _, c := range r.Calls {
		if strings.Contains(c, "sysctl -w") {
			t.Fatalf("Plan вызвал мутирующую команду %q", c)
		}
	}
}

// TestPlan_RuntimeAndPersistMatch_Clean — runtime value matches + the persist
// file contains the same line → drift=false, no mutations.
func TestPlan_RuntimeAndPersistMatch_Clean(t *testing.T) {
	dir := t.TempDir()
	name := "net.ipv4.ip_forward"
	fname := "net-ipv4-ip_forward.conf"
	path := filepath.Join(dir, fname)
	if err := os.WriteFile(path, []byte(name+" = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforePersist, _ := os.ReadFile(path)

	r := internaltest.NewRunner()
	r.On("sysctl -n "+name, util.Result{ExitCode: 0, Stdout: "1\n"})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  name,
			"value": "1",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertNoMutatingSysctlCalls(t, r)
	if afterPersist, _ := os.ReadFile(path); string(afterPersist) != string(beforePersist) {
		t.Fatalf("Plan изменил persist-файл")
	}
}

// TestPlan_RuntimeDrift — runtime value differs → drift=true, no mutations.
func TestPlan_RuntimeDrift(t *testing.T) {
	dir := t.TempDir()
	name := "net.ipv4.ip_forward"
	r := internaltest.NewRunner()
	r.On("sysctl -n "+name, util.Result{ExitCode: 0, Stdout: "0\n"})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  name,
			"value": "1",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (runtime drift)")
	}
	assertNoMutatingSysctlCalls(t, r)
}

// TestPlan_PersistMissing_Drift — runtime matches, no persist file → drift.
func TestPlan_PersistMissing_Drift(t *testing.T) {
	dir := t.TempDir()
	name := "net.ipv4.ip_forward"
	r := internaltest.NewRunner()
	r.On("sysctl -n "+name, util.Result{ExitCode: 0, Stdout: "1\n"})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  name,
			"value": "1",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (persist missing)")
	}
	// Plan must not create the persist file.
	fname := "net-ipv4-ip_forward.conf"
	if _, err := os.Stat(filepath.Join(dir, fname)); !os.IsNotExist(err) {
		t.Fatalf("Plan создал persist-файл")
	}
	assertNoMutatingSysctlCalls(t, r)
}

// TestPlan_PersistContentDrift — runtime matches, file exists but the line differs → drift.
func TestPlan_PersistContentDrift(t *testing.T) {
	dir := t.TempDir()
	name := "net.ipv4.ip_forward"
	fname := "net-ipv4-ip_forward.conf"
	path := filepath.Join(dir, fname)
	if err := os.WriteFile(path, []byte(name+" = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforePersist, _ := os.ReadFile(path)

	r := internaltest.NewRunner()
	r.On("sysctl -n "+name, util.Result{ExitCode: 0, Stdout: "1\n"})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  name,
			"value": "1",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (persist content drift)")
	}
	if afterPersist, _ := os.ReadFile(path); string(afterPersist) != string(beforePersist) {
		t.Fatalf("Plan изменил persist-файл")
	}
	assertNoMutatingSysctlCalls(t, r)
}
