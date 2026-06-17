package archive_test

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/archive"

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

// dirSnap — снимок entries каталога для assertUnchangedDir.
func dirSnap(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func assertEntries(t *testing.T, dir string, want []string) {
	t.Helper()
	got := dirSnap(t, dir)
	if len(got) != len(want) {
		t.Fatalf("dir entries=%v, want %v", got, want)
	}
	for i, n := range want {
		if got[i] != n {
			t.Fatalf("dir entries=%v, want %v", got, want)
		}
	}
}

// TestPlan_Extracted_NoMarker_Drift — dest пустой, marker отсутствует → drift,
// архив не распакован, dest остался пуст.
func TestPlan_Extracted_NoMarker_Drift(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "a.tar", makeTar(t, []tarEntry{
		{name: "hello.txt", typeflag: tar.TypeReg, mode: 0o644, body: "hi"},
	}))
	dest := filepath.Join(dir, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	beforeEntries := dirSnap(t, dest)

	m := archive.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "extracted",
		Params: mustStruct(t, map[string]any{"path": src, "dest": dest}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (no marker)")
	}
	assertEntries(t, dest, beforeEntries)
}

// TestPlan_Extracted_MarkerMatches_Clean — marker есть и его sha == sha(src) →
// drift=false, без мутаций.
func TestPlan_Extracted_MarkerMatches_Clean(t *testing.T) {
	dir := t.TempDir()
	srcBytes := makeTar(t, []tarEntry{
		{name: "hello.txt", typeflag: tar.TypeReg, mode: 0o644, body: "hi"},
	})
	src := writeArchive(t, dir, "a.tar", srcBytes)
	dest := filepath.Join(dir, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dest, archive.MarkerFile)
	if err := os.WriteFile(markerPath, []byte(sha(srcBytes)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeEntries := dirSnap(t, dest)
	beforeMarker, _ := os.ReadFile(markerPath)

	m := archive.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "extracted",
		Params: mustStruct(t, map[string]any{"path": src, "dest": dest}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertEntries(t, dest, beforeEntries)
	if afterMarker, _ := os.ReadFile(markerPath); string(afterMarker) != string(beforeMarker) {
		t.Fatalf("Plan изменил marker")
	}
}

// TestPlan_Extracted_MarkerMismatch_Drift — marker с другим sha → drift,
// архив не распакован.
func TestPlan_Extracted_MarkerMismatch_Drift(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "a.tar", makeTar(t, []tarEntry{
		{name: "hello.txt", typeflag: tar.TypeReg, mode: 0o644, body: "hi"},
	}))
	dest := filepath.Join(dir, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dest, archive.MarkerFile)
	if err := os.WriteFile(markerPath, []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeEntries := dirSnap(t, dest)

	m := archive.New()
	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "extracted",
		Params: mustStruct(t, map[string]any{"path": src, "dest": dest}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (marker mismatch)")
	}
	assertEntries(t, dest, beforeEntries)
}
