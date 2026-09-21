package cron_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/cron"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"

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

func TestValidate(t *testing.T) {
	m := cron.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "backup"}),
	})
	if reply.Ok {
		t.Fatal("Validate without schedule/command: ok unexpectedly")
	}
	reply, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     "backup",
			"schedule": "0 * * * *",
			"command":  "/usr/local/bin/backup.sh",
		}),
	})
	if !reply.Ok {
		t.Fatalf("Validate: %+v", reply)
	}
}

func TestApply_Present_CreatesJobFile(t *testing.T) {
	dir := t.TempDir()
	m := &cron.Module{Dir: dir}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     "backup",
			"schedule": "0 3 * * *",
			"command":  "/usr/local/bin/backup.sh",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false when creating a cron-job")
	}
	got, err := os.ReadFile(filepath.Join(dir, "backup"))
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	want := "0 3 * * * root /usr/local/bin/backup.sh\n"
	if string(got) != want {
		t.Fatalf("content=%q want %q", string(got), want)
	}
}

func TestApply_Present_CustomUser(t *testing.T) {
	dir := t.TempDir()
	m := &cron.Module{Dir: dir}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     "appjob",
			"schedule": "*/5 * * * *",
			"command":  "/opt/app/poll.sh",
			"user":     "appuser",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "appjob"))
	want := "*/5 * * * * appuser /opt/app/poll.sh\n"
	if string(got) != want {
		t.Fatalf("content=%q want %q", string(got), want)
	}
}

func TestApply_Present_IdempotentWhenIdentical(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup")
	if err := os.WriteFile(path, []byte("0 3 * * * root /usr/local/bin/backup.sh\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := &cron.Module{Dir: dir}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     "backup",
			"schedule": "0 3 * * *",
			"command":  "/usr/local/bin/backup.sh",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true for an identical cron-job")
	}
}

func TestApply_Present_ChangesOnScheduleDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup")
	if err := os.WriteFile(path, []byte("0 3 * * * root /usr/local/bin/backup.sh\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := &cron.Module{Dir: dir}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     "backup",
			"schedule": "0 4 * * *",
			"command":  "/usr/local/bin/backup.sh",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false with a different schedule")
	}
}

func TestApply_Absent_RemovesIfExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doomed")
	if err := os.WriteFile(path, []byte("0 0 * * * root /bin/true\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := &cron.Module{Dir: dir}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "doomed"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false when removing an existing one")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file was not removed: err=%v", err)
	}
}

func TestApply_Absent_MissingIsNoOp(t *testing.T) {
	dir := t.TempDir()
	m := &cron.Module{Dir: dir}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "ghost"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true for an already-absent entry")
	}
}

func TestApply_RejectsInvalidName(t *testing.T) {
	dir := t.TempDir()
	m := &cron.Module{Dir: dir}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     "../escape",
			"schedule": "0 * * * *",
			"command":  "/bin/true",
		}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("Failed=false with an invalid job name")
	}
}
