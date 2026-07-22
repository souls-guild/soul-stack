package directory_test

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/directory"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// statUIDGID — uid/gid of a directory/file via syscall.Stat_t.
func statUIDGID(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() is not *syscall.Stat_t on this platform")
	}
	return sys.Uid, sys.Gid
}

func lookupGID(gid uint32) func(string) (*user.Group, error) {
	return func(string) (*user.Group, error) {
		return &user.Group{Gid: strconv.Itoa(int(gid))}, nil
	}
}

func lookupUID(uid uint32) func(string) (*user.User, error) {
	return func(string) (*user.User, error) {
		return &user.User{Uid: strconv.Itoa(int(uid))}, nil
	}
}

// foreignGID looks for a process supplementary group other than the
// directory's gid, into which chgrp will succeed without root.
func foreignGID(t *testing.T, ownGID uint32) (uint32, bool) {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("Getgroups: %v", err)
	}
	for _, g := range groups {
		if uint32(g) != ownGID {
			return uint32(g), true
		}
	}
	return 0, false
}

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

type dirState struct {
	mode  os.FileMode
	isDir bool
}

func dirSnapshot(t *testing.T, path string) dirState {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("snapshot stat %s: %v", path, err)
	}
	return dirState{mode: info.Mode().Perm(), isDir: info.IsDir()}
}

func assertDirUnchanged(t *testing.T, path string, before dirState) {
	t.Helper()
	now := dirSnapshot(t, path)
	if now != before {
		t.Fatalf("Plan modified the directory: %+v != before %+v (pure-read)", now, before)
	}
}

// ---------------------------------------------------------------------------
// present (ported 1:1 from the former core.file.directory state)
// ---------------------------------------------------------------------------

// Guard 1: idempotency — directory exists with the wanted attributes → changed=false.
func TestApply_Present_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	uid, gid := statUIDGID(t, path)

	m := directory.New()
	m.LookupUser = lookupUID(uid)
	m.LookupGroup = lookupGID(gid)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":  path,
			"mode":  "0755",
			"owner": "me",
			"group": "grp",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ev := stream.Last(); ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want both false", ev.Changed, ev.Failed)
	}
}

// Guard 2: creation — directory missing → changed=true, created with owner/group/mode.
func TestApply_Present_Creates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	gid := func() uint32 { _, g := statUIDGID(t, dir); return g }()

	m := directory.New()
	m.LookupUser = lookupUID(func() uint32 { u, _ := statUIDGID(t, dir); return u }())
	m.LookupGroup = lookupGID(gid)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":  path,
			"mode":  "0750",
			"owner": "me",
			"group": "grp",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want changed=true", ev.Changed, ev.Failed)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("created is not a directory")
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("mode=%v want 0750", info.Mode().Perm())
	}
	if !ev.Output.Fields["created"].GetBoolValue() {
		t.Fatal("output.created != true")
	}
}

// Guard 3: owner drift → changed=true, owner fixed (via group, chgrp works without root).
func TestApply_Present_DriftOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, ownGID := statUIDGID(t, path)
	targetGID, ok := foreignGID(t, ownGID)
	if !ok {
		t.Skip("no supplementary group different from the directory gid - chgrp without root cannot be proven")
	}

	m := directory.New()
	m.LookupGroup = lookupGID(targetGID)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":  path,
			"group": "target",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	if !ev.Changed {
		t.Fatal("changed=false on owner/group drift")
	}
	if _, gid := statUIDGID(t, path); gid != targetGID {
		t.Fatalf("gid=%d not fixed to %d", gid, targetGID)
	}
}

// Guard 4: mode drift → changed=true, chmod applied.
func TestApply_Present_DriftMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path": path,
			"mode": "0700",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want changed=true", ev.Changed, ev.Failed)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode=%v want 0700", info.Mode().Perm())
	}
}

// Guard 5: type conflict — path exists but is a file → Failed, no overwrite.
func TestApply_Present_TypeConflict_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("i am a file"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false on type conflict (file)")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "i am a file" {
		t.Fatalf("file was modified: %q", string(got))
	}
}

// Guard 6a: parents:true creates intermediate directories.
func TestApply_Present_Parents_CreatesIntermediate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c")

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":    path,
			"parents": true,
			"mode":    "0755",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ev := stream.Last(); !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want changed=true", ev.Changed, ev.Failed)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory not created: err=%v", err)
	}
}

// Guard 6b: parents:false on a missing parent → error.
func TestApply_Present_NoParents_MissingParent_Fails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "d")

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":    path,
			"parents": false,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false with a missing parent and no parents")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("directory created despite the error: err=%v", err)
	}
}

// Guard 7: Plan(Scry) parity for present — same changed as Apply, no mutation.
func TestPlan_Present_Parity(t *testing.T) {
	t.Run("match clean", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "d")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		before := dirSnapshot(t, path)

		m := directory.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"path": path, "mode": "0755"}),
		}, stream); err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got := stream.last(); got == nil || got.GetChanged() {
			t.Fatalf("changed=%v want false", got.GetChanged())
		}
		assertDirUnchanged(t, path, before)
	})

	t.Run("missing drift no mutation", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "d")

		m := directory.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"path": path, "mode": "0755"}),
		}, stream); err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got := stream.last(); got == nil || !got.GetChanged() {
			t.Fatalf("changed=false want true (missing dir)")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("Plan created directory %s (should be pure-read)", path)
		}
	})

	t.Run("mode drift no mutation", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "d")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		before := dirSnapshot(t, path)

		m := directory.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"path": path, "mode": "0700"}),
		}, stream); err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got := stream.last(); got == nil || !got.GetChanged() {
			t.Fatalf("changed=false want true (mode drift)")
		}
		assertDirUnchanged(t, path, before)
	})

	t.Run("type conflict errors", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := directory.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"path": path}),
		}, stream); err == nil {
			t.Fatal("Plan returned nil error on type conflict (expected an error)")
		}
	})
}

// ---------------------------------------------------------------------------
// absent (new)
// ---------------------------------------------------------------------------

// Absent 1: missing path → changed=false (idempotent).
func TestApply_Absent_Missing_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone")

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want both false", ev.Changed, ev.Failed)
	}
	if ev.Output.Fields["removed"].GetBoolValue() {
		t.Fatal("output.removed != false")
	}
}

// Absent 2: empty directory → removed (no recursive needed).
func TestApply_Absent_Empty_Removes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want changed=true", ev.Changed, ev.Failed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("directory not removed: err=%v", err)
	}
}

// Absent 3: non-empty directory without recursive → Failed, nothing removed.
func TestApply_Absent_NonEmpty_NoRecursive_Fails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	child := filepath.Join(path, "f")
	if err := os.WriteFile(child, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false on a non-empty directory without recursive")
	}
	if _, err := os.Stat(child); err != nil {
		t.Fatalf("contents removed despite the error: %v", err)
	}
}

// Absent 4: non-empty directory with recursive:true → removed.
func TestApply_Absent_NonEmpty_Recursive_Removes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.MkdirAll(filepath.Join(path, "sub"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "sub", "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path, "recursive": true}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want changed=true", ev.Changed, ev.Failed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("tree not removed: err=%v", err)
	}
}

// Absent 5: root-guard — a protected system path is refused.
func TestApply_Absent_ProtectedRoot_Fails(t *testing.T) {
	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": "/etc", "recursive": true}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false on a protected system path /etc")
	}
	// /etc must still exist.
	if _, err := os.Stat("/etc"); err != nil {
		t.Fatalf("/etc was touched: %v", err)
	}
}

// Absent 6: type conflict — path is a file → Failed, file intact.
func TestApply_Absent_TypeConflict_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false on a file at path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file removed despite type conflict: %v", err)
	}
}

// Absent 7: symlink at path → Failed, never traversed; the target survives.
func TestApply_Absent_Symlink_Fails_TargetIntact(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed target child: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	m := directory.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": link, "recursive": true}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false on a symlink at path")
	}
	// The link's target and its contents must be untouched.
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Fatalf("symlink target was removed: %v", err)
	}
}

// Absent Plan parity.
func TestPlan_Absent_Parity(t *testing.T) {
	t.Run("missing clean", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "gone")
		m := directory.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "absent",
			Params: mustStruct(t, map[string]any{"path": path}),
		}, stream); err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got := stream.last(); got == nil || got.GetChanged() {
			t.Fatalf("changed=%v want false (missing)", got.GetChanged())
		}
	})

	t.Run("empty drift no mutation", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "d")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		m := directory.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "absent",
			Params: mustStruct(t, map[string]any{"path": path}),
		}, stream); err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got := stream.last(); got == nil || !got.GetChanged() {
			t.Fatalf("changed=false want true (empty removable)")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("Plan removed directory (should be pure-read): %v", err)
		}
	})

	t.Run("non-empty without recursive errors", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "d")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := directory.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "absent",
			Params: mustStruct(t, map[string]any{"path": path}),
		}, stream); err == nil {
			t.Fatal("Plan returned nil error on a non-empty dir without recursive (expected directory_not_empty)")
		}
		if stream.last() != nil && !stream.last().GetChanged() {
			t.Fatal("Plan sent changed=false instead of PlanFailed (false-clean)")
		}
	})

	t.Run("non-empty with recursive drift", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "d")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := directory.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "absent",
			Params: mustStruct(t, map[string]any{"path": path, "recursive": true}),
		}, stream); err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got := stream.last(); got == nil || !got.GetChanged() {
			t.Fatalf("changed=false want true (non-empty recursive)")
		}
	})

	t.Run("symlink errors", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		m := directory.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "absent",
			Params: mustStruct(t, map[string]any{"path": link}),
		}, stream); err == nil {
			t.Fatal("Plan returned nil error on a symlink at path (expected an error)")
		}
	})
}

// Validate rejects an unknown state and a missing path (delegated to manifest).
func TestValidate(t *testing.T) {
	m := directory.New()
	if reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"path": "/etc/x"}),
	}); reply.Ok {
		t.Fatal("Validate ok=true for an unknown state")
	}
	if reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{}),
	}); reply.Ok {
		t.Fatal("Validate ok=true with a missing required path")
	}
	if reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": "/etc/x"}),
	}); !reply.Ok {
		t.Fatal("Validate ok=false for a valid absent request")
	}
}
