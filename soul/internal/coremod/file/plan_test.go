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

// planStream is a fake grpc.ServerStreamingServer[PlanEvent] for Plan tests.
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

// TestPlan_Present_Match_Clean — Plan(present) with matching content:
// changed=false and the file is NOT modified on disk (pure-read, ADR-031 Scry).
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

// TestPlan_Present_ContentMismatch_Drift — Plan(present) with mismatched
// content: changed=true, the file is NOT rewritten.
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

// TestPlan_Present_Missing_Drift — Plan(present) for a missing file:
// changed=true (Apply would create it) and the file is NOT created.
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
		t.Fatalf("Plan created file %s (should be pure-read)", path)
	}
}

// TestPlan_Absent_Present_Drift — Plan(absent) with an existing file:
// changed=true (Apply would delete it) and the file is NOT deleted.
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

// TestPlan_Absent_Missing_Clean — Plan(absent) for a missing file:
// changed=false (nothing to delete).
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

// TestPlan_Present_Src_Missing_Drift — Plan(present, src) for a missing
// dest: changed=true (Apply would create it), src is NOT copied, dest is NOT
// created.
func TestPlan_Present_Src_Missing_Drift(t *testing.T) {
	src := seedSrc(t, "payload\n")
	dst := filepath.Join(t.TempDir(), "dest")

	m := file.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": dst, "src": src}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatal("changed=false, want true (dest missing)")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("Plan created dest %s (should be pure-read)", dst)
	}
}

// TestPlan_Present_Src_Match_Clean — Plan(present, src) when dest == src
// bytes: changed=false and dest is NOT rewritten.
func TestPlan_Present_Src_Match_Clean(t *testing.T) {
	src := seedSrc(t, "same\n")
	dst := filepath.Join(t.TempDir(), "dest")
	if err := os.WriteFile(dst, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dst)

	m := file.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": dst, "src": src}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertUnchanged(t, dst, before)
}

// TestPlan_Present_Src_Unreadable_PlanFailed — src is missing during Plan →
// Plan returns an error (PlanFailed), NOT false-clean (ADR-031).
func TestPlan_Present_Src_Unreadable_PlanFailed(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dest")

	m := file.New()
	stream := &planStream{}
	err := m.Plan(&pluginv1.PlanRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": dst, "src": filepath.Join(t.TempDir(), "nope")}),
	}, stream)
	if err == nil {
		t.Fatal("Plan returned nil, want PlanFailed for an unreadable src (not false-clean)")
	}
	if stream.last() != nil && !stream.last().GetChanged() {
		t.Fatal("Plan sent changed=false instead of PlanFailed (false-clean)")
	}
}

// fileState is a (content, mode) snapshot of a file to verify pure-read in assertUnchanged.
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
		t.Fatalf("Plan changed the file: %+v != before %+v (should be pure-read)", now, before)
	}
}
