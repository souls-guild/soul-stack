package line_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/line"

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
	if snapshot(t, path) != before {
		t.Fatalf("Plan изменил файл")
	}
}

// TestPlan_Present_LineExists_Clean — строка уже есть → drift=false.
func TestPlan_Present_LineExists_Clean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\nbar\nbaz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := line.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": path, "line": "bar"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Present_LineMissing_Drift — строки нет → drift=true.
func TestPlan_Present_LineMissing_Drift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\nbaz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := line.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": path, "line": "bar"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (line missing)")
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Present_FileMissing_Drift — файла нет → drift=true (Apply create-or-fail).
func TestPlan_Present_FileMissing_Drift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.txt")

	m := line.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": path, "line": "bar", "create": true}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (file missing)")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Plan создал файл")
	}
}

// TestPlan_Present_Regexp_NoMatch_Drift — regexp без совпадений → Apply
// добавил бы строку → drift=true.
func TestPlan_Present_Regexp_NoMatch_Drift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := line.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": path, "line": "bar=1", "regexp": "^bar="}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true")
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Absent_LineExists_Drift — строка есть → drift=true.
func TestPlan_Absent_LineExists_Drift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\nbar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := line.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path, "line": "bar"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true")
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Absent_LineMissing_Clean — строки нет → drift=false.
func TestPlan_Absent_LineMissing_Clean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	m := line.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path, "line": "bar"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertUnchanged(t, path, before)
}

// TestPlan_Absent_FileMissing_Clean — файла нет → нечего удалять → clean.
func TestPlan_Absent_FileMissing_Clean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.txt")

	m := line.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path, "line": "bar"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
}
