package mount_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/mount"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
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

func newModule(t *testing.T, fstab string, r *internaltest.Runner) *mount.Module {
	t.Helper()
	return &mount.Module{Runner: r, FstabPath: fstab}
}

func TestValidate(t *testing.T) {
	m := mount.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": "/mnt/data"}),
	})
	if reply.Ok {
		t.Fatal("Validate without source/fstype: ok unexpectedly")
	}
	reply, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":   "/mnt/data",
			"source": "/dev/sdb1",
			"fstype": "ext4",
		}),
	})
	if !reply.Ok {
		t.Fatalf("Validate: %+v", reply)
	}
}

func TestApply_Present_AddsToFstabAndMounts(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	if err := os.WriteFile(fstab, []byte("# header\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target := filepath.Join(dir, "mnt-data")

	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("findmnt --target "+target, util.Result{ExitCode: 1})
	r.On("mount -t ext4 -o defaults -- /dev/sdb1 "+target, util.Result{ExitCode: 0})
	m := newModule(t, fstab, r)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "/dev/sdb1",
			"fstype": "ext4",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false when adding")
	}
	content, _ := os.ReadFile(fstab)
	want := "/dev/sdb1 " + target + " ext4 defaults 0 0"
	if !strings.Contains(string(content), want) {
		t.Fatalf("fstab=%q does not contain %q", string(content), want)
	}
}

func TestApply_Present_IdempotentWhenAlreadyMountedAndInFstab(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	if err := os.WriteFile(fstab, []byte("/dev/sdb1 "+target+" ext4 defaults 0 0\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("findmnt --target "+target, util.Result{ExitCode: 0, Stdout: "OK"})
	m := newModule(t, fstab, r)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "/dev/sdb1",
			"fstype": "ext4",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true for already-mounted + identical fstab")
	}
}

func TestApply_Present_ReplacesFstabOnOptsDiff(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	if err := os.WriteFile(fstab, []byte("/dev/sdb1 "+target+" ext4 defaults 0 0\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("findmnt --target "+target, util.Result{ExitCode: 0, Stdout: "OK"})
	m := newModule(t, fstab, r)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "/dev/sdb1",
			"fstype": "ext4",
			"opts":   "noatime,nodiratime",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false when opts change in fstab")
	}
	content, _ := os.ReadFile(fstab)
	if !strings.Contains(string(content), "noatime,nodiratime") {
		t.Fatalf("fstab was not updated: %q", string(content))
	}
}

func TestApply_Absent_RemovesFstabAndUnmounts(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	if err := os.WriteFile(fstab, []byte("# header\n/dev/sdb1 "+target+" ext4 defaults 0 0\n/dev/sdc1 /other ext4 defaults 0 0\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("findmnt --target "+target, util.Result{ExitCode: 0, Stdout: "OK"})
	r.On("umount -- "+target, util.Result{ExitCode: 0})
	m := newModule(t, fstab, r)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": target}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false when removing")
	}
	content, _ := os.ReadFile(fstab)
	if strings.Contains(string(content), target) {
		t.Fatalf("fstab still contains target: %q", string(content))
	}
	if !strings.Contains(string(content), "/other") {
		t.Fatalf("unexpectedly removed a different line: %q", string(content))
	}
}

func TestApply_Mounted_NoFstabWrite(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	if err := os.WriteFile(fstab, []byte(""), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target := filepath.Join(dir, "mnt")

	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("findmnt --target "+target, util.Result{ExitCode: 1})
	r.On("mount -t tmpfs -o defaults -- tmpfs "+target, util.Result{ExitCode: 0})
	m := newModule(t, fstab, r)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "mounted",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "tmpfs",
			"fstype": "tmpfs",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false when mounted")
	}
	content, _ := os.ReadFile(fstab)
	if strings.Contains(string(content), target) {
		t.Fatalf("fstab must not be touched for mounted; got %q", string(content))
	}
}

// TestApply_Present_PreservesFstabMode checks preserve-by-default (the
// core.line pilot pattern): editing fstab (options) doesn't reset its mode to
// default. fstab is often 0644, but the operator/distro may have set a
// non-standard mode — the module must preserve it.
func TestApply_Present_PreservesFstabMode(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	if err := os.WriteFile(fstab, []byte("/dev/sdb1 "+target+" ext4 defaults 0 0\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// WriteFile respects umask — set the exact mode explicitly.
	if err := os.Chmod(fstab, 0o600); err != nil {
		t.Fatalf("seed chmod: %v", err)
	}

	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("findmnt --target "+target, util.Result{ExitCode: 0, Stdout: "OK"})
	m := newModule(t, fstab, r)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "/dev/sdb1",
			"fstype": "ext4",
			"opts":   "noatime",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false when opts change")
	}
	info, err := os.Stat(fstab)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600 (preserve-by-default did not trigger)", info.Mode().Perm())
	}
}

// TestApply_Absent_PreservesFstabMode — same, for removing a line from fstab.
func TestApply_Absent_PreservesFstabMode(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	if err := os.WriteFile(fstab, []byte("/dev/sdb1 "+target+" ext4 defaults 0 0\n/dev/sdc1 /other ext4 defaults 0 0\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(fstab, 0o600); err != nil {
		t.Fatalf("seed chmod: %v", err)
	}

	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("findmnt --target "+target, util.Result{ExitCode: 0, Stdout: "OK"})
	r.On("umount -- "+target, util.Result{ExitCode: 0})
	m := newModule(t, fstab, r)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": target}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false when removing")
	}
	info, err := os.Stat(fstab)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600 (preserve on absent did not trigger)", info.Mode().Perm())
	}
}

// TestApply_Present_IdempotentRepeatKeepsFile — rerunning with the same
// content: changed=false, fstab's mode and mtime don't change (the no-op
// branch of upsertFstab must not write at all).
func TestApply_Present_IdempotentRepeatKeepsFile(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	if err := os.WriteFile(fstab, []byte("/dev/sdb1 "+target+" ext4 defaults 0 0\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(fstab, 0o600); err != nil {
		t.Fatalf("seed chmod: %v", err)
	}
	before, err := os.Stat(fstab)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("findmnt --target "+target, util.Result{ExitCode: 0, Stdout: "OK"})
	m := newModule(t, fstab, r)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":   target,
			"source": "/dev/sdb1",
			"fstype": "ext4",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true for identical fstab + mounted")
	}
	after, err := os.Stat(fstab)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if after.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600 (idempotent rerun changed mode)", after.Mode().Perm())
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("idempotent rerun rewrote fstab: mtime %v -> %v", before.ModTime(), after.ModTime())
	}
}

func TestApply_Unmounted_NoFstabChange(t *testing.T) {
	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	target := "/mnt/data"
	fstabContent := "/dev/sdb1 " + target + " ext4 defaults 0 0\n"
	if err := os.WriteFile(fstab, []byte(fstabContent), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("findmnt --target "+target, util.Result{ExitCode: 0})
	r.On("umount -- "+target, util.Result{ExitCode: 0})
	m := newModule(t, fstab, r)

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "unmounted",
		Params: mustStruct(t, map[string]any{"path": target}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false when unmounting")
	}
	content, _ := os.ReadFile(fstab)
	if string(content) != fstabContent {
		t.Fatalf("fstab changed while unmounted; want %q got %q", fstabContent, string(content))
	}
}
