package file_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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

// seedSrc creates a regular source file in t.TempDir() and returns its path.
func seedSrc(t *testing.T, content string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	return src
}

func applyPresent(t *testing.T, params map[string]any) *internaltest.ApplyStream {
	t.Helper()
	m := file.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{State: "present", Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return stream
}

func TestApply_Present_Src_CreatesFromSource(t *testing.T) {
	src := seedSrc(t, "binary payload\n")
	dst := filepath.Join(t.TempDir(), "dest.bin")

	ev := applyPresent(t, map[string]any{
		"path": dst,
		"src":  src,
		"mode": "0600",
	}).Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "binary payload\n" {
		t.Fatalf("content=%q", string(got))
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600", info.Mode().Perm())
	}
	if ev.Output.Fields["sha256"].GetStringValue() != sha("binary payload\n") {
		t.Fatal("sha256 mismatch (должен быть хэш содержимого src)")
	}
}

func TestApply_Present_Src_IdempotentSecondRun(t *testing.T) {
	src := seedSrc(t, "stable\n")
	dst := filepath.Join(t.TempDir(), "dest")

	if ev := applyPresent(t, map[string]any{"path": dst, "src": src}).Last(); !ev.Changed {
		t.Fatal("первый прогон: changed=false")
	}
	if ev := applyPresent(t, map[string]any{"path": dst, "src": src}).Last(); ev.Changed {
		t.Fatal("второй прогон того же src: changed=true (нет идемпотентности)")
	}
}

func TestApply_Present_Src_ContentUpgrade(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dest")
	if err := os.WriteFile(dst, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	src := seedSrc(t, "new\n")

	ev := applyPresent(t, map[string]any{"path": dst, "src": src}).Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want changed=true", ev.Changed, ev.Failed)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "new\n" {
		t.Fatalf("content=%q want new", string(got))
	}
}

func TestApply_Present_Src_ModeOnlyDrift(t *testing.T) {
	src := seedSrc(t, "same\n")
	dst := filepath.Join(t.TempDir(), "dest")
	if err := os.WriteFile(dst, []byte("same\n"), 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}

	ev := applyPresent(t, map[string]any{"path": dst, "src": src, "mode": "0600"}).Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want changed=true (mode drift)", ev.Changed, ev.Failed)
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600", info.Mode().Perm())
	}
}

// owner/group drift on the src branch: content matches but the group drifts.
// chgrp to a supplementary group of the process works without root (as in directory_test).
func TestApply_Present_Src_GroupDrift(t *testing.T) {
	src := seedSrc(t, "same\n")
	dst := filepath.Join(t.TempDir(), "dest")
	if err := os.WriteFile(dst, []byte("same\n"), 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	_, ownGID := statUIDGID(t, dst)
	targetGID, ok := foreignGID(t, ownGID)
	if !ok {
		t.Skip("нет supplementary-группы для chgrp без root")
	}

	m := file.New()
	m.LookupGroup = lookupGID(targetGID)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": dst, "src": src, "group": "grp"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want changed=true (group drift)", ev.Changed, ev.Failed)
	}
	if _, gid := statUIDGID(t, dst); gid != targetGID {
		t.Fatalf("gid=%d want %d (chgrp не выполнен)", gid, targetGID)
	}
}

func TestValidate_Present_ContentSrcMutuallyExclusive(t *testing.T) {
	m := file.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path":    "/etc/x",
			"content": "inline",
			"src":     "/opt/payload",
		}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для content+src (должен быть конфликт)")
	}
	if !hasErr(reply.Errors, "mutually exclusive") {
		t.Fatalf("errors=%v want mutually-exclusive", reply.Errors)
	}
}

func TestApply_Present_ContentSrcMutuallyExclusive(t *testing.T) {
	ev := applyPresent(t, map[string]any{
		"path":    filepath.Join(t.TempDir(), "x"),
		"content": "inline",
		"src":     "/opt/payload",
	}).Last()
	if !ev.Failed {
		t.Fatal("failed=false для content+src")
	}
}

// Subtlety: content:"" (empty string, key IS PRESENT) together with src still
// conflicts. Detected by key presence, not string emptiness.
func TestApply_Present_EmptyContentPlusSrc_StillConflict(t *testing.T) {
	ev := applyPresent(t, map[string]any{
		"path":    filepath.Join(t.TempDir(), "x"),
		"content": "",
		"src":     seedSrc(t, "payload"),
	}).Last()
	if !ev.Failed {
		t.Fatal("failed=false для content:\"\"+src (конфликт должен ловиться по ключу)")
	}
}

func TestApply_Present_Src_Missing_Fails(t *testing.T) {
	ev := applyPresent(t, map[string]any{
		"path": filepath.Join(t.TempDir(), "dest"),
		"src":  filepath.Join(t.TempDir(), "nope"),
	}).Last()
	if !ev.Failed {
		t.Fatal("failed=false для отсутствующего src")
	}
	if !hasErr([]string{ev.Message}, "no such file") {
		t.Fatalf("message=%q want no-such-file", ev.Message)
	}
}

func TestApply_Present_Src_Directory_Fails(t *testing.T) {
	srcDir := t.TempDir()
	ev := applyPresent(t, map[string]any{
		"path": filepath.Join(t.TempDir(), "dest"),
		"src":  srcDir,
	}).Last()
	if !ev.Failed {
		t.Fatal("failed=false для src-каталога")
	}
	if !hasErr([]string{ev.Message}, "not a regular file") {
		t.Fatalf("message=%q want not-a-regular-file", ev.Message)
	}
}

// A src symlink is rejected via Lstat (not followed) — protects against substitution.
func TestApply_Present_Src_Symlink_Fails(t *testing.T) {
	target := seedSrc(t, "real\n")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	ev := applyPresent(t, map[string]any{
		"path": filepath.Join(t.TempDir(), "dest"),
		"src":  link,
	}).Last()
	if !ev.Failed {
		t.Fatal("failed=false для src-симлинка (Lstat должен reject-ить)")
	}
	if !hasErr([]string{ev.Message}, "not a regular file") {
		t.Fatalf("message=%q want not-a-regular-file", ev.Message)
	}
}

func TestApply_Present_Src_Relative_Fails(t *testing.T) {
	ev := applyPresent(t, map[string]any{
		"path": filepath.Join(t.TempDir(), "dest"),
		"src":  "relative/payload",
	}).Last()
	if !ev.Failed {
		t.Fatal("failed=false для относительного src")
	}
	if !hasErr([]string{ev.Message}, "must be absolute") {
		t.Fatalf("message=%q want must-be-absolute", ev.Message)
	}
}

func TestValidate_Present_Src_Relative_Fails(t *testing.T) {
	m := file.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": "/etc/x", "src": "relative/payload"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для относительного src")
	}
	if !hasErr(reply.Errors, "must be absolute") {
		t.Fatalf("errors=%v want must-be-absolute", reply.Errors)
	}
}

// Atomicity of the src branch: no leftover temp files in the dest directory after a write.
func TestApply_Present_Src_AtomicNoLeftoverTemp(t *testing.T) {
	src := seedSrc(t, "payload\n")
	destDir := t.TempDir()
	dst := filepath.Join(destDir, "dest")

	if ev := applyPresent(t, map[string]any{"path": dst, "src": src}).Last(); !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "dest" {
			t.Fatalf("остаточный temp-файл после atomic-записи: %s", e.Name())
		}
	}
}

func hasErr(errs []string, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}
