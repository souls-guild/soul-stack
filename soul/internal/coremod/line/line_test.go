package line_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/line"

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

func seed(t *testing.T, content string) string {
	t.Helper()
	return seedMode(t, content, 0o644)
}

func seedMode(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "conf")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// WriteFile honors umask — set the exact mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("seed chmod: %v", err)
	}
	return path
}

func apply(t *testing.T, state string, params map[string]any) (*internaltest.ApplyStream, error) {
	t.Helper()
	m := line.New()
	stream := &internaltest.ApplyStream{}
	err := m.Apply(&pluginv1.ApplyRequest{State: state, Params: mustStruct(t, params)}, stream)
	return stream, err
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// --- Validate ---

func TestValidate_UnknownState(t *testing.T) {
	reply, _ := line.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"path": "/etc/x", "line": "a"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true for an unknown state")
	}
}

func TestValidate_PresentRequiresLine(t *testing.T) {
	reply, _ := line.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": "/etc/x"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true for present without line")
	}
}

func TestValidate_AbsentRequiresLineOrRegexp(t *testing.T) {
	reply, _ := line.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": "/etc/x"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true for absent without line and regexp")
	}
}

func TestValidate_InvalidRegexp(t *testing.T) {
	reply, _ := line.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"path": "/etc/x", "line": "a", "regexp": "("}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true for a broken regexp")
	}
}

func TestValidate_InsertAfterBeforeMutualExclusive(t *testing.T) {
	reply, _ := line.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"path": "/etc/x", "line": "a",
			"insertafter": "EOF", "insertbefore": "BOF",
		}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true with simultaneous insertafter/insertbefore")
	}
}

func TestValidate_AcceptsAbsentRegexpOnly(t *testing.T) {
	reply, _ := line.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"path": "/etc/x", "regexp": "^foo"}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok=false for valid absent+regexp: %v", reply.Errors)
	}
}

// --- present: appending a new line ---

func TestPresent_AppendsToExistingFile(t *testing.T) {
	path := seed(t, "alpha\nbeta\n")
	stream, err := apply(t, "present", map[string]any{"path": path, "line": "gamma"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v msg=%s", ev.Changed, ev.Failed, ev.Message)
	}
	if got := read(t, path); got != "alpha\nbeta\ngamma\n" {
		t.Fatalf("content=%q", got)
	}
}

func TestPresent_AlreadyPresent_NoOp(t *testing.T) {
	path := seed(t, "alpha\nbeta\n")
	stream, _ := apply(t, "present", map[string]any{"path": path, "line": "beta"})
	if stream.Last().Changed {
		t.Fatal("changed=true for an already-present line")
	}
	if got := read(t, path); got != "alpha\nbeta\n" {
		t.Fatalf("content changed: %q", got)
	}
}

func TestPresent_InsertAfterLiteral(t *testing.T) {
	path := seed(t, "alpha\nbeta\ngamma\n")
	_, _ = apply(t, "present", map[string]any{
		"path": path, "line": "INSERTED", "insertafter": "beta",
	})
	if got := read(t, path); got != "alpha\nbeta\nINSERTED\ngamma\n" {
		t.Fatalf("content=%q", got)
	}
}

func TestPresent_InsertBeforeLiteral(t *testing.T) {
	path := seed(t, "alpha\nbeta\ngamma\n")
	_, _ = apply(t, "present", map[string]any{
		"path": path, "line": "INSERTED", "insertbefore": "gamma",
	})
	if got := read(t, path); got != "alpha\nbeta\nINSERTED\ngamma\n" {
		t.Fatalf("content=%q", got)
	}
}

func TestPresent_InsertBeforeBOF(t *testing.T) {
	path := seed(t, "alpha\nbeta\n")
	_, _ = apply(t, "present", map[string]any{
		"path": path, "line": "HEADER", "insertbefore": "BOF",
	})
	if got := read(t, path); got != "HEADER\nalpha\nbeta\n" {
		t.Fatalf("content=%q", got)
	}
}

func TestPresent_InsertAfterMissingAnchor_FallbackEOF(t *testing.T) {
	path := seed(t, "alpha\nbeta\n")
	_, _ = apply(t, "present", map[string]any{
		"path": path, "line": "X", "insertafter": "nonexistent",
	})
	if got := read(t, path); got != "alpha\nbeta\nX\n" {
		t.Fatalf("content=%q", got)
	}
}

// --- present + regexp ---

func TestPresent_Regexp_ReplaceFirst(t *testing.T) {
	path := seed(t, "# config\nport = 80\nhost = local\n")
	stream, _ := apply(t, "present", map[string]any{
		"path": path, "line": "port = 443", "regexp": "^port = ",
	})
	ev := stream.Last()
	if !ev.Changed {
		t.Fatal("changed=false on regexp-replace")
	}
	if got := read(t, path); got != "# config\nport = 443\nhost = local\n" {
		t.Fatalf("content=%q", got)
	}
	if r := ev.Output.Fields["replaced"].GetNumberValue(); r != 1 {
		t.Fatalf("replaced=%v want 1", r)
	}
}

func TestPresent_Regexp_MultipleMatch_ReplacesFirstWithWarning(t *testing.T) {
	path := seed(t, "x = 1\nx = 2\nx = 3\n")
	stream, _ := apply(t, "present", map[string]any{
		"path": path, "line": "x = 9", "regexp": "^x = ",
	})
	ev := stream.Last()
	if !ev.Changed {
		t.Fatal("changed=false on multiple match")
	}
	if got := read(t, path); got != "x = 9\nx = 2\nx = 3\n" {
		t.Fatalf("replaced more than just the first: %q", got)
	}
	if m := ev.Output.Fields["matched"].GetNumberValue(); m != 3 {
		t.Fatalf("matched=%v want 3", m)
	}
	w := ev.Output.Fields["warning"].GetStringValue()
	if w == "" || !strings.Contains(w, "3") {
		t.Fatalf("warning missing/incorrect: %q", w)
	}
}

func TestPresent_Regexp_FirstAlreadyEqual_NoOp(t *testing.T) {
	path := seed(t, "port = 443\nhost = local\n")
	stream, _ := apply(t, "present", map[string]any{
		"path": path, "line": "port = 443", "regexp": "^port = ",
	})
	if stream.Last().Changed {
		t.Fatal("changed=true when the first match already equals line")
	}
}

func TestPresent_Regexp_NoMatch_Appends(t *testing.T) {
	path := seed(t, "alpha\nbeta\n")
	stream, _ := apply(t, "present", map[string]any{
		"path": path, "line": "gamma = 1", "regexp": "^gamma = ",
	})
	if !stream.Last().Changed {
		t.Fatal("changed=false when regexp has no matches and line is appended")
	}
	if got := read(t, path); got != "alpha\nbeta\ngamma = 1\n" {
		t.Fatalf("content=%q", got)
	}
}

// --- absent ---

func TestAbsent_Regexp_RemoveAll(t *testing.T) {
	path := seed(t, "keep\n# c1\ndrop1\n# c2\ndrop2\nkeep2\n")
	stream, _ := apply(t, "absent", map[string]any{
		"path": path, "regexp": "^drop",
	})
	ev := stream.Last()
	if !ev.Changed {
		t.Fatal("changed=false on absent+regexp")
	}
	if got := read(t, path); got != "keep\n# c1\n# c2\nkeep2\n" {
		t.Fatalf("content=%q", got)
	}
	if r := ev.Output.Fields["removed"].GetNumberValue(); r != 2 {
		t.Fatalf("removed=%v want 2", r)
	}
}

func TestAbsent_Line_RemovesAllExactMatches(t *testing.T) {
	path := seed(t, "a\ndup\nb\ndup\nc\n")
	stream, _ := apply(t, "absent", map[string]any{
		"path": path, "line": "dup",
	})
	if got := read(t, path); got != "a\nb\nc\n" {
		t.Fatalf("content=%q", got)
	}
	if r := stream.Last().Output.Fields["removed"].GetNumberValue(); r != 2 {
		t.Fatalf("removed=%v want 2", r)
	}
}

func TestAbsent_NoMatch_NoOp(t *testing.T) {
	path := seed(t, "a\nb\nc\n")
	stream, _ := apply(t, "absent", map[string]any{"path": path, "line": "zzz"})
	if stream.Last().Changed {
		t.Fatal("changed=true when there's nothing to remove")
	}
}

// --- create ---

func TestCreate_True_FileMissing_Present_CreatesWithLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.conf")
	stream, _ := apply(t, "present", map[string]any{
		"path": path, "line": "first = 1", "create": true, "mode": "0640",
	})
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v msg=%s", ev.Changed, ev.Failed, ev.Message)
	}
	if got := read(t, path); got != "first = 1\n" {
		t.Fatalf("content=%q", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v want 0640", info.Mode().Perm())
	}
}

func TestCreate_False_FileMissing_Present_Fails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.conf")
	stream, _ := apply(t, "present", map[string]any{"path": path, "line": "x"})
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("failed=false when the file doesn't exist and create=false")
	}
	if !strings.Contains(ev.Message, "create:true") {
		t.Fatalf("expected a hint about create:true, got %q", ev.Message)
	}
}

func TestAbsent_FileMissing_NoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.conf")
	stream, _ := apply(t, "absent", map[string]any{"path": path, "line": "x"})
	ev := stream.Last()
	if ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v for absent with a missing file", ev.Changed, ev.Failed)
	}
}

// --- idempotency ---

func TestIdempotency_Present(t *testing.T) {
	path := seed(t, "alpha\n")
	for i := 0; i < 3; i++ {
		stream, _ := apply(t, "present", map[string]any{"path": path, "line": "beta"})
		if i == 0 {
			if !stream.Last().Changed {
				t.Fatal("first run: changed=false")
			}
		} else if stream.Last().Changed {
			t.Fatalf("run %d: changed=true (not idempotent)", i)
		}
	}
	if got := read(t, path); got != "alpha\nbeta\n" {
		t.Fatalf("content=%q", got)
	}
}

func TestIdempotency_Absent(t *testing.T) {
	path := seed(t, "a\ndrop\nb\n")
	for i := 0; i < 3; i++ {
		stream, _ := apply(t, "absent", map[string]any{"path": path, "line": "drop"})
		if i == 0 {
			if !stream.Last().Changed {
				t.Fatal("first run: changed=false")
			}
		} else if stream.Last().Changed {
			t.Fatalf("run %d: changed=true (not idempotent)", i)
		}
	}
}

func TestIdempotency_PresentRegexp(t *testing.T) {
	path := seed(t, "port = 80\n")
	for i := 0; i < 3; i++ {
		stream, _ := apply(t, "present", map[string]any{
			"path": path, "line": "port = 443", "regexp": "^port = ",
		})
		if i == 0 {
			if !stream.Last().Changed {
				t.Fatal("first regexp run: changed=false")
			}
		} else if stream.Last().Changed {
			t.Fatalf("regexp run %d: changed=true (not idempotent)", i)
		}
	}
	if got := read(t, path); got != "port = 443\n" {
		t.Fatalf("content=%q", got)
	}
}

// --- write atomicity / preserving trailing newline ---

func TestPreservesNoTrailingNewline(t *testing.T) {
	path := seed(t, "alpha\nbeta") // no final \n
	_, _ = apply(t, "present", map[string]any{"path": path, "line": "gamma"})
	// gamma is appended at EOF; source file has no trailing NL → result has none either.
	if got := read(t, path); got != "alpha\nbeta\ngamma" {
		t.Fatalf("content=%q", got)
	}
}

func TestMissingPath_Fails(t *testing.T) {
	stream, _ := apply(t, "present", map[string]any{"line": "x"})
	if !stream.Last().Failed {
		t.Fatal("failed=false when path is missing")
	}
}

// --- preserve mode/owner for in-place edits of an existing file (ADR-015) ---

func TestPresent_Edit_PreservesMode(t *testing.T) {
	path := seedMode(t, "alpha\nbeta\n", 0o600)
	stream, err := apply(t, "present", map[string]any{"path": path, "line": "gamma"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false when appending a line")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600 (mode not set → preserve)", info.Mode().Perm())
	}
}

func TestPresent_Edit_ExplicitModeOverrides(t *testing.T) {
	path := seedMode(t, "alpha\nbeta\n", 0o600)
	_, err := apply(t, "present", map[string]any{"path": path, "line": "gamma", "mode": "0640"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v want 0640 (explicit mode → override)", info.Mode().Perm())
	}
}

func TestAbsent_Rewrite_PreservesMode(t *testing.T) {
	path := seedMode(t, "keep\ndrop\nkeep2\n", 0o600)
	stream, err := apply(t, "absent", map[string]any{"path": path, "line": "drop"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false on absent-removal")
	}
	if got := read(t, path); got != "keep\nkeep2\n" {
		t.Fatalf("content=%q", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600 (absent → preserve)", info.Mode().Perm())
	}
}

// TestPresent_Edit_PreservesOwner verifies that rename doesn't reset the
// owner: the preserve branch restores the original uid/gid. The test
// environment usually isn't root, so changing ownership to a DIFFERENT uid
// isn't possible; instead we verify that the file's owner/group after the
// edit match the original (chown to the same values is allowed for the
// owner). Cross-owner override (explicit owner/group for another user)
// requires root and isn't covered here.
func TestPresent_Edit_PreservesOwner(t *testing.T) {
	path := seedMode(t, "alpha\n", 0o644)
	beforeUID, beforeGID := ownerOf(t, path)

	_, err := apply(t, "present", map[string]any{"path": path, "line": "beta"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	afterUID, afterGID := ownerOf(t, path)
	if beforeUID != afterUID || beforeGID != afterGID {
		t.Fatalf("owner changed: before=%d:%d after=%d:%d", beforeUID, beforeGID, afterUID, afterGID)
	}
}

func ownerOf(t *testing.T, path string) (uid, gid int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("Stat_t unavailable on this platform")
	}
	return int(sys.Uid), int(sys.Gid)
}
