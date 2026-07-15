package mount_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
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

// assertNoMutatingMountCalls fails if the runner received a mount/umount call.
func assertNoMutatingMountCalls(t *testing.T, r *internaltest.Runner) {
	t.Helper()
	for _, c := range r.Calls {
		for _, bad := range []string{"mount -", "mount /", "umount"} {
			if len(c) >= len(bad) && c[:len(bad)] == bad {
				t.Fatalf("Plan вызвал мутирующую команду %q (должен быть pure-read)", c)
			}
		}
	}
}

// TestPlan_Present_AlreadyMountedInFstab_Clean — fstab has a matching line and
// findmnt reports mounted → drift=false, no mutations.
func TestPlan_Present_AlreadyMountedInFstab_Clean(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	if err := os.WriteFile(fstab, []byte("/dev/sdb1 "+target+" ext4 defaults 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeFstab, _ := os.ReadFile(fstab)

	r := internaltest.NewRunner()
	r.On("findmnt --target "+target, util.Result{ExitCode: 0, Stdout: "OK"})
	m := newModule(t, fstab, r)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "/dev/sdb1",
			"fstype": "ext4",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	now, _ := os.ReadFile(fstab)
	if string(now) != string(beforeFstab) {
		t.Fatalf("Plan изменил fstab: %q != %q", string(now), string(beforeFstab))
	}
	assertNoMutatingMountCalls(t, r)
}

// TestPlan_Present_NotInFstab_Drift — empty fstab → drift, no mutations.
func TestPlan_Present_NotInFstab_Drift(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	if err := os.WriteFile(fstab, []byte("# header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeFstab, _ := os.ReadFile(fstab)

	r := internaltest.NewRunner()
	target := "/mnt/data"
	r.On("findmnt --target "+target, util.Result{ExitCode: 1})
	m := newModule(t, fstab, r)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "/dev/sdb1",
			"fstype": "ext4",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	now, _ := os.ReadFile(fstab)
	if string(now) != string(beforeFstab) {
		t.Fatalf("Plan изменил fstab")
	}
	assertNoMutatingMountCalls(t, r)
}

// TestPlan_Present_InFstabButNotMounted_Drift — fstab matches but findmnt
// reports not-mounted → drift (Apply would mount it).
func TestPlan_Present_InFstabButNotMounted_Drift(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	if err := os.WriteFile(fstab, []byte("/dev/sdb1 "+target+" ext4 defaults 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := internaltest.NewRunner()
	r.On("findmnt --target "+target, util.Result{ExitCode: 1})
	m := newModule(t, fstab, r)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "/dev/sdb1",
			"fstype": "ext4",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift: not mounted)")
	}
	assertNoMutatingMountCalls(t, r)
}

// TestPlan_Mounted_NotMounted_Drift — state mounted with findmnt exit != 0 → drift.
func TestPlan_Mounted_NotMounted_Drift(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	if err := os.WriteFile(fstab, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := internaltest.NewRunner()
	target := "/mnt/data"
	r.On("findmnt --target "+target, util.Result{ExitCode: 1})
	m := newModule(t, fstab, r)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State: "mounted",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "/dev/sdb1",
			"fstype": "ext4",
		}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertNoMutatingMountCalls(t, r)
}

// TestPlan_Unmounted_StillMounted_Drift — state unmounted with findmnt exit 0 → drift.
func TestPlan_Unmounted_StillMounted_Drift(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	if err := os.WriteFile(fstab, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := internaltest.NewRunner()
	target := "/mnt/data"
	r.On("findmnt --target "+target, util.Result{ExitCode: 0, Stdout: "OK"})
	m := newModule(t, fstab, r)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "unmounted",
		Params: mustStruct(t, map[string]any{"path": target}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertNoMutatingMountCalls(t, r)
}

// TestPlan_Absent_Mounted_Drift — Plan(absent), path is mounted → drift.
func TestPlan_Absent_Mounted_Drift(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	if err := os.WriteFile(fstab, []byte("/dev/sdb1 "+target+" ext4 defaults 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := internaltest.NewRunner()
	r.On("findmnt --target "+target, util.Result{ExitCode: 0, Stdout: "OK"})
	m := newModule(t, fstab, r)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": target}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertNoMutatingMountCalls(t, r)
}

// TestPlan_Absent_NotMountedNotInFstab_Clean — Plan(absent), nothing to remove → clean.
func TestPlan_Absent_NotMountedNotInFstab_Clean(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	if err := os.WriteFile(fstab, []byte("# only header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := internaltest.NewRunner()
	target := "/mnt/data"
	r.On("findmnt --target "+target, util.Result{ExitCode: 1})
	m := newModule(t, fstab, r)

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": target}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertNoMutatingMountCalls(t, r)
}
