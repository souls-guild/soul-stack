package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/archive"
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

// tarEntry describes one tar entry for the generator.
type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
	linkname string
}

// makeTar builds a tar in-memory from entries.
func makeTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Linkname: e.linkname,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader %q: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar Write %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

// gzipBytes wraps data with gzip.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

// zipEntry describes one zip entry for the generator.
type zipEntry struct {
	name    string
	mode    os.FileMode
	body    string
	symlink bool // body = target
}

// makeZip builds a zip in-memory from entries.
func makeZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if e.symlink {
			mode |= os.ModeSymlink
		}
		if strings.HasSuffix(e.name, "/") {
			mode |= os.ModeDir
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip CreateHeader %q: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("zip Write %q: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

// writeArchive writes archive bytes to disk and returns the path.
func writeArchive(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write archive %s: %v", p, err)
	}
	return p
}

func sha(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// apply runs Apply with the given params and returns the stream.
func apply(t *testing.T, params map[string]any) *internaltest.ApplyStream {
	t.Helper()
	m := archive.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "extracted",
		Params: mustStruct(t, params),
	}, stream); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	return stream
}

func TestValidate(t *testing.T) {
	m := archive.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "extracted",
		Params: mustStruct(t, map[string]any{"path": "/a.tar"}),
	})
	if reply.Ok {
		t.Fatal("Validate without dest: ok unexpectedly")
	}
	reply, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "extracted",
		Params: mustStruct(t, map[string]any{
			"path":   "/a.tar",
			"dest":   "/srv/extract",
			"format": "rar",
		}),
	})
	if reply.Ok {
		t.Fatal("Validate with unknown format: ok unexpectedly")
	}
}

// happy-path per format: extract real bytes, verify the file.
func TestApply_HappyPath_AllFormats(t *testing.T) {
	tarBytes := makeTar(t, []tarEntry{
		{name: "dir/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "dir/hello.txt", typeflag: tar.TypeReg, mode: 0o644, body: "tar-content"},
	})

	cases := []struct {
		name    string
		file    string
		data    []byte
		content string
	}{
		{"tar", "blob.tar", tarBytes, "tar-content"},
		{"tar.gz", "blob.tar.gz", gzipBytes(t, tarBytes), "tar-content"},
		{"tar.bz2", "blob.tar.bz2", bz2Fixture(t), "bz2-content"},
		{"zip", "blob.zip", makeZip(t, []zipEntry{
			{name: "dir/", mode: 0o755},
			{name: "dir/hello.txt", mode: 0o644, body: "zip-content"},
		}), "zip-content"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := writeArchive(t, dir, c.file, c.data)
			dest := filepath.Join(dir, "out")

			stream := apply(t, map[string]any{"path": src, "dest": dest})
			if stream.Last().Failed {
				t.Fatalf("Failed: %s", stream.Last().Message)
			}
			if !stream.Last().Changed {
				t.Fatal("Changed=false on first extraction")
			}
			got, err := os.ReadFile(filepath.Join(dest, "dir", "hello.txt"))
			if err != nil {
				t.Fatalf("read extracted: %v", err)
			}
			if string(got) != c.content {
				t.Fatalf("content=%q want %q", got, c.content)
			}
			marker, err := os.ReadFile(filepath.Join(dest, archive.MarkerFile))
			if err != nil {
				t.Fatalf("marker: %v", err)
			}
			if string(marker) != sha(c.data)+"\n" {
				t.Fatalf("marker=%q want %q", marker, sha(c.data)+"\n")
			}
		})
	}
}

// bz2FixtureB64 is a real tar.bz2 (written with `tar cjf`, single entry
// dir/hello.txt = "bz2-content"). stdlib bzip2 is decompress-only, no
// encoder — the fixture is hardcoded so the bzip2-decompress branch is
// exercised against a real bzip2 stream.
const bz2FixtureB64 = "QlpoOTFBWSZTWZVOi44AAD77kNIAAMBAA/+ACAB+ZZ7wBAABCCAAcjMjRoaaNAYIwg8oJU9U9CTygNMj1A0ekGTt8rslo4aIDnvCrOBgRqL1e3LDCspwmDreCKJpKyMmC5N+3U6LfZFNN79qhGzWXZtTjplkrc8fcDwrae6J+AICX4AVeKyE/F3JFOFCQlU6Ljg="

func bz2Fixture(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(bz2FixtureB64)
	if err != nil {
		t.Fatalf("bz2 fixture decode: %v", err)
	}
	return data
}

func TestApply_Idempotent(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{{name: "f.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"}})
	src := writeArchive(t, dir, "blob.tar", tarBytes)
	dest := filepath.Join(dir, "out")

	first := apply(t, map[string]any{"path": src, "dest": dest})
	if !first.Last().Changed {
		t.Fatal("first extraction: Changed=false")
	}
	// corrupt the extracted file — a re-run must not touch it (no-op by
	// marker), proving idempotency goes through the marker, not a walk.
	if err := os.WriteFile(filepath.Join(dest, "f.txt"), []byte("touched"), 0o644); err != nil {
		t.Fatalf("touch: %v", err)
	}
	second := apply(t, map[string]any{"path": src, "dest": dest})
	if second.Last().Changed {
		t.Fatal("re-run with a matching marker: Changed=true")
	}
	got, _ := os.ReadFile(filepath.Join(dest, "f.txt"))
	if string(got) != "touched" {
		t.Fatalf("no-op overwrote the file: %q", got)
	}
}

func TestApply_ChangesOnNewHash(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, archive.MarkerFile), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	tarBytes := makeTar(t, []tarEntry{{name: "new.txt", typeflag: tar.TypeReg, mode: 0o644, body: "new"}})
	src := writeArchive(t, dir, "blob.tar", tarBytes)

	stream := apply(t, map[string]any{"path": src, "dest": dest})
	if !stream.Last().Changed {
		t.Fatal("Changed=false with a mismatched marker")
	}
}

func TestApply_AutoDetectFails_UnknownExtension(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "blob.bin", []byte("x"))
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed {
		t.Fatal("Failed=false on auto-detect failure")
	}
}

func TestApply_MissingSource(t *testing.T) {
	dir := t.TempDir()
	stream := apply(t, map[string]any{
		"path":   filepath.Join(dir, "ghost.tar"),
		"dest":   filepath.Join(dir, "out"),
		"format": "tar",
	})
	if !stream.Last().Failed {
		t.Fatal("Failed=false for a missing archive")
	}
}

func TestApply_CorruptArchive(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "broken.tar.gz", []byte("not-a-gzip-stream"))
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed {
		t.Fatal("Failed=false for a corrupt archive")
	}
}

func TestApply_ZipSlip_FailFast(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "ok.txt", typeflag: tar.TypeReg, mode: 0o644, body: "ok"},
		{name: "../escape", typeflag: tar.TypeReg, mode: 0o644, body: "evil"},
	})
	src := writeArchive(t, dir, "evil.tar", tarBytes)
	dest := filepath.Join(dir, "out")

	stream := apply(t, map[string]any{"path": src, "dest": dest})
	if !stream.Last().Failed {
		t.Fatal("zip-slip: Failed=false (expected fail-fast)")
	}
	if !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("zip-slip: message=%q (expected escapes dest)", stream.Last().Message)
	}
	// fail-fast: no entry created outside dest.
	if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
		t.Fatal("zip-slip: file outside dest was created anyway (clamp instead of fail-fast)")
	}
	// marker not written — extraction aborted entirely.
	if _, err := os.Stat(filepath.Join(dest, archive.MarkerFile)); err == nil {
		t.Fatal("zip-slip: marker was written despite the failure")
	}
}

func TestApply_ZipSlip_AbsolutePath_FailFast(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "/etc/evil", typeflag: tar.TypeReg, mode: 0o644, body: "evil"},
	})
	src := writeArchive(t, dir, "abs.tar", tarBytes)
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed || !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("absolute path: expected escape-fail, got failed=%v msg=%q",
			stream.Last().Failed, stream.Last().Message)
	}
}

// zip format: path traversal via `../` in a zip entry. Symmetric to the tar
// case, but exercises the extractZip branch separately (zip.File.Name is
// resolved by the same resolveEntry; the fail-fast contract must hold in
// both branches).
func TestApply_ZipSlip_Zip_FailFast(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "evil.zip", makeZip(t, []zipEntry{
		{name: "ok.txt", mode: 0o644, body: "ok"},
		{name: "../escape", mode: 0o644, body: "evil"},
	}))
	dest := filepath.Join(dir, "out")

	stream := apply(t, map[string]any{"path": src, "dest": dest})
	if !stream.Last().Failed {
		t.Fatal("zip-slip (zip): Failed=false (expected fail-fast)")
	}
	if !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("zip-slip (zip): message=%q (expected escapes dest)", stream.Last().Message)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
		t.Fatal("zip-slip (zip): file outside dest was created (clamp instead of fail-fast)")
	}
	if _, err := os.Stat(filepath.Join(dest, archive.MarkerFile)); err == nil {
		t.Fatal("zip-slip (zip): marker was written despite the failure")
	}
}

// zip format: absolute path in a zip entry → fail-fast.
func TestApply_ZipSlip_Zip_AbsolutePath_FailFast(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "abs.zip", makeZip(t, []zipEntry{
		{name: "/etc/evil", mode: 0o644, body: "evil"},
	}))
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed || !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("zip absolute path: expected escape-fail, got failed=%v msg=%q",
			stream.Last().Failed, stream.Last().Message)
	}
}

func TestApply_ZipBomb_MaxSize(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "big.txt", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("A", 4096)},
	})
	src := writeArchive(t, dir, "big.tar", tarBytes)
	stream := apply(t, map[string]any{
		"path":     src,
		"dest":     filepath.Join(dir, "out"),
		"max_size": "1KiB",
	})
	if !stream.Last().Failed {
		t.Fatal("max_size: Failed=false when exceeded")
	}
	if !strings.Contains(stream.Last().Message, "max_size") {
		t.Fatalf("max_size: message=%q (expected max_size)", stream.Last().Message)
	}
}

func TestApply_ZipBomb_MaxEntries(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "a.txt", typeflag: tar.TypeReg, mode: 0o644, body: "1"},
		{name: "b.txt", typeflag: tar.TypeReg, mode: 0o644, body: "2"},
		{name: "c.txt", typeflag: tar.TypeReg, mode: 0o644, body: "3"},
	})
	src := writeArchive(t, dir, "many.tar", tarBytes)
	stream := apply(t, map[string]any{
		"path":        src,
		"dest":        filepath.Join(dir, "out"),
		"max_entries": float64(2),
	})
	if !stream.Last().Failed {
		t.Fatal("max_entries: Failed=false when exceeded")
	}
	if !strings.Contains(stream.Last().Message, "max_entries") {
		t.Fatalf("max_entries: message=%q (expected max_entries)", stream.Last().Message)
	}
}

// a high compression ratio (tar.gz of repeated bytes) is rejected by the
// max_ratio check even if the total size is below max_size: a classic
// zip-bomb evades the size limit with a small compressed size.
func TestApply_ZipBomb_Ratio_TarGz(t *testing.T) {
	dir := t.TempDir()
	// 1 MiB of identical bytes → gzip compresses to ~kilobytes, ratio in the hundreds.
	tarBytes := makeTar(t, []tarEntry{
		{name: "bomb.txt", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("A", 1<<20)},
	})
	src := writeArchive(t, dir, "bomb.tar.gz", gzipBytes(t, tarBytes))
	stream := apply(t, map[string]any{
		"path":      src,
		"dest":      filepath.Join(dir, "out"),
		"max_ratio": float64(50),
	})
	if !stream.Last().Failed {
		t.Fatal("max_ratio: Failed=false at a high compression ratio")
	}
	if !strings.Contains(stream.Last().Message, "max_ratio") {
		t.Fatalf("max_ratio: message=%q (expected max_ratio)", stream.Last().Message)
	}
	// fail-fast: marker not written, extraction aborted.
	if _, err := os.Stat(filepath.Join(dir, "out", archive.MarkerFile)); err == nil {
		t.Fatal("max_ratio: marker was written despite the failure")
	}
}

// a normal (low-compressibility) tar.gz passes with the default max_ratio:
// pseudo-random content barely compresses, ratio ~1.
func TestApply_Ratio_NormalArchivePasses(t *testing.T) {
	dir := t.TempDir()
	// pseudo-random content: gzip barely compresses it, ratio close to 1.
	var sb strings.Builder
	for i := 0; i < 4096; i++ {
		sb.WriteByte(byte((i*2654435761 + 12345) % 251))
	}
	tarBytes := makeTar(t, []tarEntry{
		{name: "data.bin", typeflag: tar.TypeReg, mode: 0o644, body: sb.String()},
	})
	src := writeArchive(t, dir, "normal.tar.gz", gzipBytes(t, tarBytes))
	stream := apply(t, map[string]any{
		"path": src,
		"dest": filepath.Join(dir, "out"),
		// default max_ratio=100 — a normal archive fits within it.
	})
	if stream.Last().Failed {
		t.Fatalf("normal archive rejected by max_ratio: %s", stream.Last().Message)
	}
}

// max_ratio=0 disables the check: a high-ratio bomb passes (an escape hatch
// for legitimately highly-compressible archives, on the operator's own risk).
func TestApply_Ratio_Disabled(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "bomb.txt", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("A", 1<<20)},
	})
	src := writeArchive(t, dir, "bomb.tar.gz", gzipBytes(t, tarBytes))
	stream := apply(t, map[string]any{
		"path":      src,
		"dest":      filepath.Join(dir, "out"),
		"max_ratio": float64(0),
	})
	if stream.Last().Failed {
		t.Fatalf("max_ratio=0 should disable the check, but Failed=%s", stream.Last().Message)
	}
}

// zip with a high compression ratio: per-entry CompressedSize64/Uncompressed
// is accounted for, the bomb gets caught.
func TestApply_ZipBomb_Ratio_Zip(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "bomb.zip", makeZip(t, []zipEntry{
		{name: "bomb.txt", mode: 0o644, body: strings.Repeat("A", 1<<20)},
	}))
	stream := apply(t, map[string]any{
		"path":      src,
		"dest":      filepath.Join(dir, "out"),
		"max_ratio": float64(50),
	})
	if !stream.Last().Failed {
		t.Fatal("zip max_ratio: Failed=false at a high ratio")
	}
	if !strings.Contains(stream.Last().Message, "max_ratio") {
		t.Fatalf("zip max_ratio: message=%q (expected max_ratio)", stream.Last().Message)
	}
}

// a plain (uncompressed) tar is NOT ratio-checked even with a tiny max_ratio:
// compressed=0, the format isn't compressed — it can't be a ratio bomb,
// max_size bounds it instead.
func TestApply_Ratio_PlainTarSkipped(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "f.txt", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("A", 4096)},
	})
	src := writeArchive(t, dir, "plain.tar", tarBytes)
	stream := apply(t, map[string]any{
		"path":      src,
		"dest":      filepath.Join(dir, "out"),
		"max_ratio": float64(1),
	})
	if stream.Last().Failed {
		t.Fatalf("a plain tar should not be ratio-checked: %s", stream.Last().Message)
	}
}

// negative max_ratio is a configuration error.
func TestApply_Ratio_NegativeInvalid(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{{name: "f.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"}})
	src := writeArchive(t, dir, "blob.tar", tarBytes)
	stream := apply(t, map[string]any{
		"path":      src,
		"dest":      filepath.Join(dir, "out"),
		"max_ratio": float64(-1),
	})
	if !stream.Last().Failed || !strings.Contains(stream.Last().Message, "max_ratio") {
		t.Fatalf("negative max_ratio: failed=%v msg=%q", stream.Last().Failed, stream.Last().Message)
	}
}

func TestApply_Symlink_WithinDest_OK(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "target.txt", typeflag: tar.TypeReg, mode: 0o644, body: "real"},
		{name: "link", typeflag: tar.TypeSymlink, linkname: "target.txt"},
	})
	src := writeArchive(t, dir, "ln.tar", tarBytes)
	dest := filepath.Join(dir, "out")

	stream := apply(t, map[string]any{"path": src, "dest": dest})
	if stream.Last().Failed {
		t.Fatalf("within-dest symlink: Failed=%s", stream.Last().Message)
	}
	link, err := os.Readlink(filepath.Join(dest, "link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if link != "target.txt" {
		t.Fatalf("symlink target=%q want target.txt", link)
	}
}

func TestApply_Symlink_EscapesDest_Fails(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "evil-link", typeflag: tar.TypeSymlink, linkname: "../../etc/passwd"},
	})
	src := writeArchive(t, dir, "ln.tar", tarBytes)
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed {
		t.Fatal("symlink-escape: Failed=false")
	}
	if !strings.Contains(stream.Last().Message, "symlink") || !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("symlink-escape: message=%q", stream.Last().Message)
	}
}

func TestApply_Symlink_AbsoluteEscape_Fails(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "abs-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	src := writeArchive(t, dir, "ln.tar", tarBytes)
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed || !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("absolute symlink: failed=%v msg=%q", stream.Last().Failed, stream.Last().Message)
	}
}

func TestApply_Hardlink_Unsupported(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "orig.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "hard", typeflag: tar.TypeLink, linkname: "orig.txt"},
	})
	src := writeArchive(t, dir, "hl.tar", tarBytes)
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed || !strings.Contains(stream.Last().Message, "unsupported type") {
		t.Fatalf("hardlink: failed=%v msg=%q", stream.Last().Failed, stream.Last().Message)
	}
}

func TestApply_Devnode_Unsupported(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "dev/null", typeflag: tar.TypeChar, mode: 0o666},
	})
	src := writeArchive(t, dir, "dev.tar", tarBytes)
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed || !strings.Contains(stream.Last().Message, "unsupported type") {
		t.Fatalf("devnode: failed=%v msg=%q", stream.Last().Failed, stream.Last().Message)
	}
}

func TestApply_SetuidBit_Masked(t *testing.T) {
	dir := t.TempDir()
	// mode with setuid(04000)+setgid(02000)+sticky(01000)+0777.
	tarBytes := makeTar(t, []tarEntry{
		{name: "suid", typeflag: tar.TypeReg, mode: 0o7777, body: "x"},
	})
	src := writeArchive(t, dir, "suid.tar", tarBytes)
	dest := filepath.Join(dir, "out")
	stream := apply(t, map[string]any{"path": src, "dest": dest})
	if stream.Last().Failed {
		t.Fatalf("setuid: Failed=%s", stream.Last().Message)
	}
	info, err := os.Stat(filepath.Join(dest, "suid"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("setuid/setgid/sticky not masked: mode=%v", info.Mode())
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatalf("perm bits corrupted: %v want 0777", info.Mode().Perm())
	}
}

// write-through within-dest symlink: directory subdir/ + symlink alias→subdir
// (within-dest, legitimate on its own) + a write to alias/file.txt "through"
// it. securejoin resolves alias against the symlink already on disk →
// joined≠naive → the step fails with escapes dest. Pins that the protection
// works on the RESOLVED path, not lexically: naively swapping securejoin for
// filepath.Join would break this test.
func TestApply_WriteThroughWithinDestSymlink_Fails(t *testing.T) {
	dir := t.TempDir()
	tarBytes := makeTar(t, []tarEntry{
		{name: "subdir/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "alias", typeflag: tar.TypeSymlink, linkname: "subdir"},
		{name: "alias/file.txt", typeflag: tar.TypeReg, mode: 0o644, body: "evil"},
	})
	src := writeArchive(t, dir, "thru.tar", tarBytes)
	dest := filepath.Join(dir, "out")

	stream := apply(t, map[string]any{"path": src, "dest": dest})
	if !stream.Last().Failed {
		t.Fatal("write-through symlink: Failed=false (expected a fail on the resolved path)")
	}
	if !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("write-through symlink: message=%q (expected escapes dest)", stream.Last().Message)
	}
	// the file written "through" the symlink isn't materialized as either
	// alias/file.txt or under subdir.
	if _, err := os.Stat(filepath.Join(dest, "subdir", "file.txt")); err == nil {
		t.Fatal("write-through symlink: file created in the target directory (protection bypassed)")
	}
}

// max_size boundary: the sum across multiple files is tracked, exactly at
// the limit passes, +1 byte → failed. Pins the fragile
// budget := maxSize-usedSize+1 in writeFileEntry.
func TestApply_MaxSize_Boundary(t *testing.T) {
	const limit = 1024 // bytes; bare number = bytes

	// two files summing exactly to the limit (600 + 424 = 1024) — passes.
	t.Run("exact_limit_ok", func(t *testing.T) {
		dir := t.TempDir()
		tarBytes := makeTar(t, []tarEntry{
			{name: "a.txt", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("A", 600)},
			{name: "b.txt", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("B", 424)},
		})
		src := writeArchive(t, dir, "exact.tar", tarBytes)
		stream := apply(t, map[string]any{
			"path":     src,
			"dest":     filepath.Join(dir, "out"),
			"max_size": "1024",
		})
		if stream.Last().Failed {
			t.Fatalf("exactly at the limit (%d bytes): Failed=%s", limit, stream.Last().Message)
		}
	})

	// sum one byte over the limit (600 + 425 = 1025) — failed.
	t.Run("over_by_one_fails", func(t *testing.T) {
		dir := t.TempDir()
		tarBytes := makeTar(t, []tarEntry{
			{name: "a.txt", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("A", 600)},
			{name: "b.txt", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("B", 425)},
		})
		src := writeArchive(t, dir, "over.tar", tarBytes)
		stream := apply(t, map[string]any{
			"path":     src,
			"dest":     filepath.Join(dir, "out"),
			"max_size": "1024",
		})
		if !stream.Last().Failed {
			t.Fatalf("limit+1 byte (%d): Failed=false", limit+1)
		}
		if !strings.Contains(stream.Last().Message, "max_size") {
			t.Fatalf("limit+1: message=%q (expected max_size)", stream.Last().Message)
		}
	})
}

// empty archive (tar and zip): extraction ok, marker written, re-run is a no-op.
func TestApply_EmptyArchive(t *testing.T) {
	cases := []struct {
		name string
		file string
		data []byte
	}{
		{"tar", "empty.tar", makeTar(t, nil)},
		{"zip", "empty.zip", makeZip(t, nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := writeArchive(t, dir, c.file, c.data)
			dest := filepath.Join(dir, "out")

			first := apply(t, map[string]any{"path": src, "dest": dest})
			if first.Last().Failed {
				t.Fatalf("empty archive: Failed=%s", first.Last().Message)
			}
			if !first.Last().Changed {
				t.Fatal("empty archive: Changed=false on first extraction")
			}
			marker, err := os.ReadFile(filepath.Join(dest, archive.MarkerFile))
			if err != nil {
				t.Fatalf("empty archive: marker not written: %v", err)
			}
			if string(marker) != sha(c.data)+"\n" {
				t.Fatalf("empty archive: marker=%q want %q", marker, sha(c.data)+"\n")
			}

			second := apply(t, map[string]any{"path": src, "dest": dest})
			if second.Last().Changed {
				t.Fatal("empty archive: re-run Changed=true (expected no-op)")
			}
		})
	}
}

func TestApply_UnknownFormat_Explicit(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "blob.dat", []byte("x"))
	m := archive.New()
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "extracted",
		Params: mustStruct(t, map[string]any{
			"path":   src,
			"dest":   filepath.Join(dir, "out"),
			"format": "rar",
		}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("unknown format: Failed=false")
	}
}
