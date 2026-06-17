package file_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/file"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// planStream — fake grpc.ServerStreamingServer[PlanEvent] для тестов Plan.
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

// TestPlan_Present_Match_Clean — Plan(present) при совпадающем content:
// changed=false и файл НЕ изменён на диске (pure-read, ADR-031 Scry).
func TestPlan_Present_Match_Clean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := file.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": path, "content": "same\n"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Present_ContentMismatch_Drift — Plan(present) при расхождении
// content: changed=true, файл НЕ переписан.
func TestPlan_Present_ContentMismatch_Drift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := file.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": path, "content": "new\n"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (content drift)")
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Present_Missing_Drift — Plan(present) для отсутствующего файла:
// changed=true (Apply создал бы) и файл НЕ создан.
func TestPlan_Present_Missing_Drift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")

	m := file.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": path, "content": "x\n"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (missing file drift)")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Plan создал файл %s (должен быть pure-read)", path)
	}
}

// TestPlan_Absent_Present_Drift — Plan(absent) при существующем файле:
// changed=true (Apply удалил бы) и файл НЕ удалён.
func TestPlan_Absent_Present_Drift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := file.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (file exists drift)")
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Absent_Missing_Clean — Plan(absent) для отсутствующего файла:
// changed=false (нечего удалять).
func TestPlan_Absent_Missing_Clean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.txt")

	m := file.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
}

// fileState — снимок (содержимое, mode) файла для сверки pure-read в assertUnchanged.
type fileState struct {
	content string
	mode    os.FileMode
}

func snapshot(t *testing.T, path string) fileState {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("snapshot read %s: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("snapshot stat %s: %v", path, err)
	}
	return fileState{content: string(b), mode: info.Mode().Perm()}
}

func assertUnchanged(t *testing.T, path string, before fileState) {
	t.Helper()
	now := snapshot(t, path)
	if now != before {
		t.Fatalf("Plan изменил файл: %+v != before %+v (должен быть pure-read)", now, before)
	}
}
