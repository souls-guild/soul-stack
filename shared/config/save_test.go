package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Round-trip: Load → SaveToBytes without mutation → byte-for-byte identical to the
// source. Base guarantee of preserving comments/order/anchors: for an unmutated
// Document, Save returns `doc.source` directly.
func TestSaveKeeper_GoldenRoundTrip(t *testing.T) {
	path := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	_, doc, diags, err := LoadKeeperFromBytes(path, src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io err: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("load returned errors")
	}
	out, warns, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected 0 warnings on unmutated round-trip, got %d", len(warns))
	}
	if string(out) != string(src) {
		t.Fatalf("round-trip not byte-identical\nlen src=%d out=%d", len(src), len(out))
	}
}

func TestSaveSoul_GoldenRoundTrip(t *testing.T) {
	path := filepath.FromSlash("../../examples/soul/soul.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	_, doc, diags, err := LoadSoulFromBytes(path, src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io err: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("load returned errors")
	}
	out, warns, err := SaveSoulToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected 0 warnings on unmutated round-trip, got %d", len(warns))
	}
	if string(out) != string(src) {
		t.Fatalf("round-trip not byte-identical for soul")
	}
}

// SaveKeeper(path, doc) writes to a file; the file exists and matches the in-memory
// render. On an unmutated document — byte-identical too.
func TestSaveKeeper_WritesToFile(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	dst := filepath.Join(dir, "keeper.yml")
	if err := os.WriteFile(dst, src, 0o640); err != nil {
		t.Fatal(err)
	}
	diags, err := SaveKeeper(dst, doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(diags) != 0 {
		dump(t, diags)
		t.Fatalf("unexpected diagnostics")
	}
	out, _ := os.ReadFile(dst)
	if string(out) != string(src) {
		t.Fatalf("on-disk write not byte-identical")
	}
}

// Permission preservation: source 0640 → result 0640 (mode inherited via Chmod on the
// tmp before Write).
func TestSaveKeeper_PreservesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	dst := filepath.Join(dir, "keeper.yml")
	if err := os.WriteFile(dst, src, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveKeeper(dst, doc); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("perm not preserved: got %v, want 0640", info.Mode().Perm())
	}
}

// Symlink reject: keeper.yml is a symlink, Save rejects with an error and diagnostic
// `symlink_write_not_supported`. The target file is not modified.
func TestSaveKeeper_SymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	target := filepath.Join(dir, "real.yml")
	if err := os.WriteFile(target, []byte("kid: real-keeper\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "keeper.yml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	diags, err := SaveKeeper(link, doc)
	if err == nil {
		t.Fatalf("expected error for symlink write, got nil")
	}
	found := false
	for _, d := range diags {
		if d.Code == "symlink_write_not_supported" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected symlink_write_not_supported diagnostic")
	}
	// Target untouched.
	tgtAfter, _ := os.ReadFile(target)
	if string(tgtAfter) != "kid: real-keeper\n" {
		t.Fatalf("symlink target was modified")
	}
}

// Atomic-rename fault injection: tmp is created successfully, but we can't emulate a
// rename failure with built-in means without a private API. We check the minimal
// invariant: if the source existed before Save, it is preserved after a Save error
// (covered by the symlink case and any stage error after CreateTemp). Here — the
// read-only-directory case.
func TestSaveKeeper_FailedWriteDoesNotCorruptSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	dst := filepath.Join(dir, "keeper.yml")
	if err := os.WriteFile(dst, src, 0o640); err != nil {
		t.Fatal(err)
	}
	// Make the directory read-only: CreateTemp will fail.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := SaveKeeper(dst, doc)
	if err == nil {
		t.Fatalf("expected write error in read-only dir")
	}
	// Source must still be there, untouched.
	after, statErr := os.ReadFile(dst)
	if statErr != nil {
		t.Fatalf("source disappeared after failed save: %v", statErr)
	}
	if string(after) != string(src) {
		t.Fatalf("source was modified after failed save")
	}
}

// PatchKeeper changes a scalar and on the subsequent Save the new file contains the
// new value. The mutated flag switches to AST render.
func TestPatchKeeper_ScalarReplace(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.kid", "keeper-new-name"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	out, _, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(string(out), "kid: keeper-new-name") {
		t.Fatalf("patched value not present in output\n%s", out)
	}
	if strings.Contains(string(out), "kid: keeper-eu-west-01") {
		t.Fatalf("old value still present in output")
	}
}

// An inline comment next to the scalar node that became the Patch target is preserved
// via snapshot+restore (see the PatchKeeper doc comment).
func TestPatchKeeper_PreservesInlineComment(t *testing.T) {
	src := []byte(`kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:9442"  # MVP listener
      tls:
        cert: /c
        key: /k
    event_stream:
      addr: "0.0.0.0:8443"
      tls:
        cert: /c
        key: /k
        ca: /a
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
`)
	_, doc, _, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.listen.grpc.bootstrap.addr", "0.0.0.0:5443"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	out, _, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// Goccy normalises multi-space to single space before `#`, so check the
	// single-space variant:
	if !strings.Contains(string(out), "MVP listener") {
		t.Fatalf("inline comment lost\n%s", out)
	}
	if !strings.Contains(string(out), "0.0.0.0:5443") {
		t.Fatalf("patched value missing\n%s", out)
	}
}

// Non-existent path → ErrPathNotFound (no silent create-on-write).
func TestPatchKeeper_PathNotFound(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	err := PatchKeeper(doc, "$.no.such.path", "x")
	if err == nil {
		t.Fatalf("expected ErrPathNotFound, got nil")
	}
	if !errors.Is(err, ErrPathNotFound) {
		t.Fatalf("expected ErrPathNotFound, got %v", err)
	}
}

// Patch + Save → round_trip_warning (mutated flag; differences from the source are
// nearly inevitable).
func TestPatchKeeper_EmitsRoundTripWarning(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.kid", "keeper-new"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	_, warns, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	found := false
	for _, d := range warns {
		if d.Code == "round_trip_warning" {
			found = true
		}
	}
	if !found {
		dump(t, warns)
		t.Fatalf("expected round_trip_warning after mutating patch")
	}
}

// Save diagnostics (round_trip_warning / symlink_write_not_supported /
// atomic_rename_failed) are generated in the write phase, so Phase must be
// PhaseWriteBack, not PhaseParse.
func TestSaveDiagnostics_PhaseWriteBack(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	// round_trip_warning — via Patch + SaveToBytes.
	if err := PatchKeeper(doc, "$.kid", "keeper-new"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	_, warns, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	foundRT := false
	for _, d := range warns {
		if d.Code == "round_trip_warning" {
			foundRT = true
			if d.Phase != diag.PhaseWriteBack {
				t.Fatalf("round_trip_warning: expected PhaseWriteBack, got %q", d.Phase)
			}
		}
	}
	if !foundRT {
		t.Fatalf("expected round_trip_warning diagnostic after mutation, got: %+v", warns)
	}

	// symlink_write_not_supported — via SaveKeeper on a symlink.
	if runtime.GOOS != "windows" {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.yml")
		if err := os.WriteFile(target, []byte("kid: real\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "keeper.yml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		diags, _ := SaveKeeper(link, doc)
		foundSym := false
		for _, d := range diags {
			if d.Code == "symlink_write_not_supported" {
				foundSym = true
				if d.Phase != diag.PhaseWriteBack {
					t.Fatalf("symlink_write_not_supported: expected PhaseWriteBack, got %q", d.Phase)
				}
			}
		}
		if !foundSym {
			t.Fatalf("expected symlink_write_not_supported diagnostic for symlink, got: %+v", diags)
		}
	}
}

// PatchSoul variant: checks API symmetry on soul.yml.
func TestPatchSoul_ScalarReplace(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/soul/soul.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadSoulFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchSoul(doc, "$.keeper.endpoints[0].host", "k9.dc1.example"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	out, _, err := SaveSoulToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(string(out), "k9.dc1.example") {
		t.Fatalf("patched value missing in soul.yml\n%s", out)
	}
}

// Nil Document → error, not panic.
func TestSave_NilDocument(t *testing.T) {
	if _, _, err := SaveKeeperToBytes(nil); err == nil {
		t.Fatalf("expected error for nil doc")
	}
	if err := PatchKeeper(nil, "$.kid", "x"); err == nil {
		t.Fatalf("expected error for nil doc")
	}
}

// CG1 — an empty / whitespace-only / no-`$`-prefix yaml-path must be rejected with an
// explicit error BEFORE calling goccy `PathString`+`FilterFile`, else goccy hits a
// SIGSEGV (`path.go:491`). Regression test for a bug found by qa.1.
func TestPatchKeeper_InvalidPathRejected(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	cases := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"no_dollar_prefix", "kid"},
		{"dotted_no_dollar", "listen.grpc.bootstrap.addr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := PatchKeeper(doc, tc.path, "x")
			if err == nil {
				t.Fatalf("expected error for invalid path %q, got nil", tc.path)
			}
		})
	}
}

// CG2 — a non-scalar target (a whole mapping/sequence) is rejected with an error
// containing the path and node type. Regression test for a bug found by qa.1.
// Additionally: after a failed Patch the document is not marked mutated, and
// SaveToBytes returns byte-identical to source.
func TestPatchKeeper_RejectNonScalarTarget(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	// $.listen — a whole mapping (grpc/openapi/mcp/metrics), not a scalar.
	err := PatchKeeper(doc, "$.listen", map[string]any{"grpc": "x"})
	if err == nil {
		t.Fatalf("expected reject for non-scalar target, got nil")
	}
	if !strings.Contains(err.Error(), "non-scalar") {
		t.Fatalf("expected 'non-scalar' in error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "$.listen") {
		t.Fatalf("expected path '$.listen' in error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "non_scalar_patch_target") {
		t.Fatalf("expected error code 'non_scalar_patch_target' in error message, got: %v", err)
	}

	// Sequence target — $.services is not a scalar either.
	err = PatchSoul_AsKeeper_sequenceCheck(t, doc)
	_ = err
	// (sanity: the real sequence case — on soul.yml via PatchSoul below)

	// After a failed Patch — byte-identical Save.
	out, warns, saveErr := SaveKeeperToBytes(doc)
	if saveErr != nil {
		t.Fatalf("save after failed patch: %v", saveErr)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected no warnings after failed patch (doc not mutated)")
	}
	if string(out) != string(src) {
		t.Fatalf("doc was modified by failed patch; expected byte-identical save")
	}
}

// Helper: soul.yml has a sequence `$.keeper.endpoints`. Check the non-scalar reject
// for a sequence target separately.
func PatchSoul_AsKeeper_sequenceCheck(t *testing.T, _ *Document) error {
	t.Helper()
	srcPath := filepath.FromSlash("../../examples/soul/soul.yml")
	src, _ := os.ReadFile(srcPath)
	_, soulDoc, _, _ := LoadSoulFromBytes(srcPath, src, ValidateOptions{})

	err := PatchSoul(soulDoc, "$.keeper.endpoints", []string{"x"})
	if err == nil {
		t.Fatalf("expected reject for non-scalar (sequence) target")
	}
	if !strings.Contains(err.Error(), "non-scalar") {
		t.Fatalf("expected 'non-scalar' in error, got: %v", err)
	}
	return err
}

// CG3 — concurrent Saves to different files from one doc — race detector clean, files
// byte-identical to the source (unmutated doc → source).
func TestSave_ConcurrentDifferentFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	const N = 4
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := filepath.Join(dir, "keeper-"+itoa(i)+".yml")
			_, err := SaveKeeper(dst, doc)
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("save #%d: %v", i, err)
		}
	}
	for i := 0; i < N; i++ {
		dst := filepath.Join(dir, "keeper-"+itoa(i)+".yml")
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read #%d: %v", i, err)
		}
		if string(got) != string(src) {
			t.Fatalf("file #%d not byte-identical to source", i)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// CG4 — after a failed Patch (invalid path) the document is not marked mutated and
// SaveToBytes returns byte-identical to source.
func TestPatchKeeper_FailedPatchPreservesByteIdentity(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.no.such.path", "x"); err == nil {
		t.Fatalf("expected ErrPathNotFound")
	}
	if err := PatchKeeper(doc, "", "x"); err == nil {
		t.Fatalf("expected empty-path error")
	}
	if err := PatchKeeper(doc, "$.listen", map[string]any{"x": 1}); err == nil {
		t.Fatalf("expected non-scalar reject")
	}

	out, warns, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected no warnings (doc not mutated)")
	}
	if string(out) != string(src) {
		t.Fatalf("doc was modified by failed patches; expected byte-identical save")
	}
}

// CG5 — Load → Patch → SaveToBytes → LoadFromBytes again without parse errors.
// Guarantees the AST render after mutation stays valid YAML in the full Load pipeline
// sense (parse + schema + semantic).
func TestPatchKeeper_RoundTripReloadable(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.kid", "keeper-reloaded"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	out, _, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	cfg, _, diags, err := LoadKeeperFromBytes(srcPath, out, ValidateOptions{})
	if err != nil {
		t.Fatalf("re-load io: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("re-load returned errors")
	}
	if cfg == nil || cfg.KID != "keeper-reloaded" {
		var got string
		if cfg != nil {
			got = cfg.KID
		}
		t.Fatalf("expected KID 'keeper-reloaded' after reload, got %q", got)
	}
}

// CG6 — Patch on a document with an anchor/alias. We pin goccy's real behaviour: the
// anchor is the "mapping value" node itself (a block anchor over a mapping), so
// resolving `$.listen.grpc.addr` hits the anchor node and returns the error "expected
// node type is map or map value. but got Anchor". This is not our regression but a
// property of goccy/go-yaml v1.19 — pinned explicitly so the regression test catches a
// behaviour change.
//
// What matters to check on our side: the error is handled via fmt.Errorf, without
// SIGSEGV and without mutated=true.
func TestPatchKeeper_AnchorPathHandledCleanly(t *testing.T) {
	src := []byte(`kid: keeper-anchor-fixture
listen:
  grpc:
    bootstrap: &g
      addr: "0.0.0.0:9442"
      tls:
        cert: /c
        key: /k
    event_stream:
      addr: "0.0.0.0:8443"
      tls:
        cert: /c
        key: /k
        ca: /a
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
`)
	_, doc, _, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})

	err := PatchKeeper(doc, "$.listen.grpc.bootstrap.addr", "0.0.0.0:5443")
	if err == nil {
		t.Fatalf("expected resolve error for path under anchor (current goccy v1.19 behaviour)")
	}
	// After a failed Patch the document is not marked mutated → byte-identical Save.
	out, warns, saveErr := SaveKeeperToBytes(doc)
	if saveErr != nil {
		t.Fatalf("save after failed anchor patch: %v", saveErr)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected no warnings (doc not mutated)")
	}
	if string(out) != string(src) {
		t.Fatalf("doc was modified by failed anchor patch")
	}
}

// CG7 — permissions 0o600 / 0o400 preserved. Extends the existing
// TestSaveKeeper_PreservesPermissions to tighter modes.
func TestSaveKeeper_PreservesPermissions_TighterModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)

	cases := []os.FileMode{0o600, 0o400}
	for _, mode := range cases {
		mode := mode
		t.Run(mode.String(), func(t *testing.T) {
			_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})
			dir := t.TempDir()
			dst := filepath.Join(dir, "keeper.yml")
			if err := os.WriteFile(dst, src, mode); err != nil {
				t.Fatal(err)
			}
			// 0o400 read-only: writeFileAtomically does Chmod on the tmp, then
			// Rename. Rename works by directory permissions, not the file's.
			if _, err := SaveKeeper(dst, doc); err != nil {
				t.Fatalf("save with mode %v: %v", mode, err)
			}
			info, err := os.Stat(dst)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if info.Mode().Perm() != mode {
				t.Fatalf("perm not preserved: got %v, want %v", info.Mode().Perm(), mode)
			}
		})
	}
}

// CG8 — dangling symlink (target doesn't exist): Save must reject with an error and
// diagnostic `symlink_write_not_supported`. Lstat detects the symlink without
// following it.
func TestSaveKeeper_DanglingSymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	link := filepath.Join(dir, "keeper.yml")
	// The symlink target intentionally doesn't exist.
	if err := os.Symlink(filepath.Join(dir, "nonexistent.yml"), link); err != nil {
		t.Fatal(err)
	}

	diags, err := SaveKeeper(link, doc)
	if err == nil {
		t.Fatalf("expected error for dangling symlink, got nil")
	}
	found := false
	for _, d := range diags {
		if d.Code == "symlink_write_not_supported" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected symlink_write_not_supported diagnostic")
	}
}
