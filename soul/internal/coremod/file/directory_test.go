package file_test

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/file"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"os/user"
)

// statUIDGID — uid/gid of a directory/file via syscall.Stat_t. The Soul agent
// targets unix, Stat_t is guaranteed (see util.OwnershipDrift).
func statUIDGID(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() не *syscall.Stat_t на этой платформе")
	}
	return sys.Uid, sys.Gid
}

// lookupGID — a LookupGroup mock returning a fixed gid for any name.
func lookupGID(gid uint32) func(string) (*user.Group, error) {
	return func(string) (*user.Group, error) {
		return &user.Group{Gid: strconv.Itoa(int(gid))}, nil
	}
}

// lookupUID — a LookupUser mock returning a fixed uid for any name.
func lookupUID(uid uint32) func(string) (*user.User, error) {
	return func(string) (*user.User, error) {
		return &user.User{Uid: strconv.Itoa(int(uid))}, nil
	}
}

// foreignGID looks for a process supplementary group other than the
// directory's gid, into which chgrp will succeed without root. Returns
// (gid, true) if found.
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

// Guard 1: idempotency — directory exists with the wanted attributes → changed=false.
func TestApply_Directory_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	uid, gid := statUIDGID(t, path)

	m := file.New()
	m.LookupUser = lookupUID(uid)
	m.LookupGroup = lookupGID(gid)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "directory",
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
func TestApply_Directory_Creates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	gid := func() uint32 { _, g := statUIDGID(t, dir); return g }()

	m := file.New()
	m.LookupUser = lookupUID(func() uint32 { u, _ := statUIDGID(t, dir); return u }())
	m.LookupGroup = lookupGID(gid)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "directory",
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
		t.Fatal("создан не каталог")
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("mode=%v want 0750", info.Mode().Perm())
	}
	if !ev.Output.Fields["created"].GetBoolValue() {
		t.Fatal("output.created != true")
	}
}

// Guard 3: owner drift → changed=true, owner fixed. uid-chown requires root,
// so we prove the fix via group (chgrp into a process supplementary group
// works without root); under root we also check uid.
func TestApply_Directory_DriftOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, ownGID := statUIDGID(t, path)
	targetGID, ok := foreignGID(t, ownGID)
	if !ok {
		t.Skip("нет supplementary-группы, отличной от gid каталога — chgrp без root недоказуем")
	}

	m := file.New()
	m.LookupGroup = lookupGID(targetGID)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "directory",
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
		t.Fatal("changed=false на owner/group drift")
	}
	if _, gid := statUIDGID(t, path); gid != targetGID {
		t.Fatalf("gid=%d not fixed to %d", gid, targetGID)
	}
}

// Guard 4: mode drift → changed=true, chmod applied.
func TestApply_Directory_DriftMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := file.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "directory",
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
func TestApply_Directory_TypeConflict_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("i am a file"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := file.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "directory",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false на конфликте типа (файл)")
	}
	// The file must not be touched.
	got, _ := os.ReadFile(path)
	if string(got) != "i am a file" {
		t.Fatalf("файл изменён: %q", string(got))
	}
	info, _ := os.Stat(path)
	if info.IsDir() {
		t.Fatal("файл превращён в каталог")
	}
}

// Guard 6a: parents:true creates intermediate directories.
func TestApply_Directory_Parents_CreatesIntermediate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c")

	m := file.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "directory",
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
		t.Fatalf("каталог не создан: err=%v", err)
	}
}

// Guard 6b: parents:false on a missing parent → error.
func TestApply_Directory_NoParents_MissingParent_Fails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "d")

	m := file.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "directory",
		Params: mustStruct(t, map[string]any{
			"path":    path,
			"parents": false,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false при отсутствующем родителе без parents")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("каталог создан вопреки ошибке: err=%v", err)
	}
}

// Guard 7: Plan(Scry) parity — planDirectory reports the same changed as
// Apply, without mutating the host.
func TestPlan_Directory_Parity(t *testing.T) {
	// 7a: directory matches → changed=false, no mutation.
	t.Run("match clean", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "d")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		before := dirSnapshot(t, path)

		m := file.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "directory",
			Params: mustStruct(t, map[string]any{"path": path, "mode": "0755"}),
		}, stream); err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got := stream.last(); got == nil || got.GetChanged() {
			t.Fatalf("changed=%v want false", got.GetChanged())
		}
		assertDirUnchanged(t, path, before)
	})

	// 7b: directory missing → changed=true, no creation (parity with Apply.created).
	t.Run("missing drift no mutation", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "d")

		m := file.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "directory",
			Params: mustStruct(t, map[string]any{"path": path, "mode": "0755"}),
		}, stream); err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got := stream.last(); got == nil || !got.GetChanged() {
			t.Fatalf("changed=false want true (missing dir)")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("Plan создал каталог %s (должен быть pure-read)", path)
		}
	})

	// 7c: mode drift → changed=true, no chmod.
	t.Run("mode drift no mutation", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "d")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		before := dirSnapshot(t, path)

		m := file.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "directory",
			Params: mustStruct(t, map[string]any{"path": path, "mode": "0700"}),
		}, stream); err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if got := stream.last(); got == nil || !got.GetChanged() {
			t.Fatalf("changed=false want true (mode drift)")
		}
		assertDirUnchanged(t, path, before)
	})

	// 7d: type conflict (file) → Plan returns error (not false-clean).
	t.Run("type conflict errors", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := file.New()
		stream := &planStream{}
		if err := m.Plan(&pluginv1.PlanRequest{
			State:  "directory",
			Params: mustStruct(t, map[string]any{"path": path}),
		}, stream); err == nil {
			t.Fatal("Plan вернул nil error на конфликте типа (ожидался error)")
		}
	})
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
		t.Fatalf("Plan изменил каталог: %+v != before %+v (pure-read)", now, before)
	}
}
