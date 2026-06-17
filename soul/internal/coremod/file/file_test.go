package file_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/file"
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

func sha(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func TestValidate_RejectsUnknownState(t *testing.T) {
	m := file.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"path": "/etc/x"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для неизвестного state")
	}
}

func TestValidate_AcceptsRendered(t *testing.T) {
	m := file.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             "/etc/x",
			"template_content": "{{ .name }}",
		}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok=false для валидного rendered: %v", reply.Errors)
	}
}

func TestValidate_Rendered_RequiresTemplateContent(t *testing.T) {
	m := file.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "rendered",
		Params: mustStruct(t, map[string]any{"path": "/etc/x"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для rendered без template_content")
	}
}

func TestApply_Present_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.txt")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":    path,
			"content": "hello\n",
			"mode":    "0640",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello\n" {
		t.Fatalf("content=%q", string(got))
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v want 0640", info.Mode().Perm())
	}
	if ev.Output.Fields["sha256"].GetStringValue() != sha("hello\n") {
		t.Fatalf("sha256 mismatch")
	}
}

func TestApply_Present_NoChangeWhenIdentical(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.txt")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":    path,
			"content": "same\n",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true for identical content")
	}
}

func TestApply_Present_ChangeOnContentDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":    path,
			"content": "new\n",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false on content diff")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new\n" {
		t.Fatalf("content=%q", string(got))
	}
}

func TestApply_Present_ModeOnlyChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.txt")
	if err := os.WriteFile(path, []byte("data\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":    path,
			"content": "data\n",
			"mode":    "0600",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false on mode diff")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600", info.Mode().Perm())
	}
}

func TestApply_Present_InvalidMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.txt")
	m := file.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":    path,
			"content": "x",
			"mode":    "garbage",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false on invalid mode")
	}
}

func TestApply_Absent_FileExists_Removes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.txt")
	if err := os.WriteFile(path, []byte("doomed"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := file.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false on existing-file remove")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still exists: err=%v", err)
	}
}

func TestApply_Absent_FileMissing_NoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.txt")
	m := file.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true for missing-file absent")
	}
}

func TestApply_MissingPath_Fails(t *testing.T) {
	m := file.New()
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false for missing path")
	}
}
