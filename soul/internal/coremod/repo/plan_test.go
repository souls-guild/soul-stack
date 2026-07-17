package repo_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

// snapshotRoot captures all files under root for verify-no-mutation checks.
func snapshotRoot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		out[p] = string(b)
		return nil
	})
	return out
}

func assertRootUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	now := snapshotRoot(t, root)
	if len(now) != len(before) {
		t.Fatalf("files added/removed by Plan: before=%d after=%d", len(before), len(now))
	}
	for p, b := range before {
		if now[p] != b {
			t.Fatalf("Plan changed %s", p)
		}
	}
}

// TestPlan_Apt_Missing_Drift — no .list file → drift, no mutations.
func TestPlan_Apt_Missing_Drift(t *testing.T) {
	m, root := newModule(t, util.PkgMgrApt)
	before := snapshotRoot(t, root)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name": "myrepo",
			"uri":  "https://example.com/apt",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (missing)")
	}
	assertRootUnchanged(t, root, before)
}

// TestPlan_Apt_Match_Clean — .list matches, no gpg_key → clean.
func TestPlan_Apt_Match_Clean(t *testing.T) {
	m, root := newModule(t, util.PkgMgrApt)
	if err := os.MkdirAll(m.AptSourcesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.AptSourcesDir, "myrepo.list")
	if err := os.WriteFile(path, []byte("deb https://example.com/apt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotRoot(t, root)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name": "myrepo",
			"uri":  "https://example.com/apt",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertRootUnchanged(t, root, before)
}

// TestPlan_Apt_KeyMissing_Drift — .list matches, but gpg_key is set and the file is missing → drift.
func TestPlan_Apt_KeyMissing_Drift(t *testing.T) {
	m, root := newModule(t, util.PkgMgrApt)
	if err := os.MkdirAll(m.AptSourcesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(m.AptKeyringsDir, "myrepo.gpg")
	listContent := "deb [signed-by=" + keyPath + "] https://example.com/apt\n"
	if err := os.WriteFile(filepath.Join(m.AptSourcesDir, "myrepo.list"), []byte(listContent), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotRoot(t, root)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":    "myrepo",
			"uri":     "https://example.com/apt",
			"gpg_key": "PUB KEY",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (key missing)")
	}
	assertRootUnchanged(t, root, before)
}

// TestPlan_Yum_Match_Clean — .repo matches → clean.
func TestPlan_Yum_Match_Clean(t *testing.T) {
	m, root := newModule(t, util.PkgMgrYum)
	if err := os.MkdirAll(m.YumReposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "[myrepo]\nname=myrepo\nbaseurl=https://example.com/yum\nenabled=1\ngpgcheck=1\n"
	path := filepath.Join(m.YumReposDir, "myrepo.repo")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotRoot(t, root)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name": "myrepo",
			"uri":  "https://example.com/yum",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertRootUnchanged(t, root, before)
}

// TestPlan_Apk_LineMatch_Clean — the line in repositories matches exactly → clean.
func TestPlan_Apk_LineMatch_Clean(t *testing.T) {
	m, root := newModule(t, util.PkgMgrApk)
	if err := os.MkdirAll(filepath.Dir(m.ApkReposFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.ApkReposFile, []byte("https://example.com/apk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotRoot(t, root)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name": "myrepo",
			"uri":  "https://example.com/apk",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertRootUnchanged(t, root, before)
}

// TestPlan_Absent_Apt_Exists_Drift — .list exists → drift.
func TestPlan_Absent_Apt_Exists_Drift(t *testing.T) {
	m, root := newModule(t, util.PkgMgrApt)
	if err := os.MkdirAll(m.AptSourcesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.AptSourcesDir, "myrepo.list")
	if err := os.WriteFile(path, []byte("deb x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotRoot(t, root)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "myrepo"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true")
	}
	assertRootUnchanged(t, root, before)
}

// TestPlan_Absent_Apt_Missing_Clean — no .list → clean.
func TestPlan_Absent_Apt_Missing_Clean(t *testing.T) {
	m, root := newModule(t, util.PkgMgrApt)
	before := snapshotRoot(t, root)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "myrepo"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertRootUnchanged(t, root, before)
}
