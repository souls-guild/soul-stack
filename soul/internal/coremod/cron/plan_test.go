package cron_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/cron"

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

type fileSnap struct {
	content string
	exists  bool
}

func snapshot(t *testing.T, path string) fileSnap {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fileSnap{exists: false}
	}
	if err != nil {
		t.Fatalf("snapshot %s: %v", path, err)
	}
	return fileSnap{content: string(b), exists: true}
}

func assertUnchanged(t *testing.T, path string, before fileSnap) {
	t.Helper()
	now := snapshot(t, path)
	if now != before {
		t.Fatalf("Plan изменил файл: %+v != before %+v", now, before)
	}
}

// TestPlan_Present_Match_Clean: Plan(present), file already matches → clean.
func TestPlan_Present_Match_Clean(t *testing.T) {
	dir := t.TempDir()
	name := "backup"
	path := filepath.Join(dir, name)
	want := "0 3 * * * root /usr/local/bin/backup\n"
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := &cron.Module{Dir: dir}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     name,
			"schedule": "0 3 * * *",
			"command":  "/usr/local/bin/backup",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Present_Missing_Drift: Plan(present), file missing → drift.
func TestPlan_Present_Missing_Drift(t *testing.T) {
	dir := t.TempDir()
	name := "backup"
	path := filepath.Join(dir, name)

	m := &cron.Module{Dir: dir}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     name,
			"schedule": "0 3 * * *",
			"command":  "/usr/local/bin/backup",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (missing)")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Plan создал файл %s", path)
	}
}

// TestPlan_Present_ContentDrift: content differs → drift, file untouched.
func TestPlan_Present_ContentDrift(t *testing.T) {
	dir := t.TempDir()
	name := "backup"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := &cron.Module{Dir: dir}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     name,
			"schedule": "0 3 * * *",
			"command":  "/usr/local/bin/backup",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (content drift)")
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Absent_Exists_Drift: Plan(absent), file exists → drift.
func TestPlan_Absent_Exists_Drift(t *testing.T) {
	dir := t.TempDir()
	name := "backup"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("0 3 * * * root /backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := &cron.Module{Dir: dir}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": name}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Absent_Missing_Clean: Plan(absent), file missing → clean.
func TestPlan_Absent_Missing_Clean(t *testing.T) {
	dir := t.TempDir()
	m := &cron.Module{Dir: dir}
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "missing"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
}
