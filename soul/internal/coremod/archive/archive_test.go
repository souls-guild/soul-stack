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

// tarEntry — описание одной записи tar для генератора.
type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
	linkname string
}

// makeTar собирает tar in-memory из записей.
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

// gzipBytes оборачивает данные gzip-ом.
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

// zipEntry — описание одной записи zip для генератора.
type zipEntry struct {
	name    string
	mode    os.FileMode
	body    string
	symlink bool // body = target
}

// makeZip собирает zip in-memory из записей.
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

// writeArchive пишет байты архива на диск и возвращает путь.
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

// apply прогоняет Apply с заданными params и возвращает stream.
func apply(t *testing.T, params map[string]any) *internaltest.ApplyStream {
	t.Helper()
	m := archive.New()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "extracted",
		Params: mustStruct(t, params),
	}, stream); err != nil {
		t.Fatalf("Apply вернул error: %v", err)
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
		t.Fatal("Validate без dest: ok unexpectedly")
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
		t.Fatal("Validate с unknown format: ok unexpectedly")
	}
}

// happy-path по каждому формату: распаковка реальных байтов, проверка файла.
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
				t.Fatal("Changed=false при первой распаковке")
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

// bz2FixtureB64 — реальный tar.bz2 (записан `tar cjf`, единственная запись
// dir/hello.txt = "bz2-content"). bzip2 в stdlib decompress-only, своего
// энкодера нет — фикстура захардкожена, чтобы проверить именно ветку
// bzip2-decompress на настоящем bzip2-потоке.
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
		t.Fatal("первая распаковка: Changed=false")
	}
	// портим извлечённый файл — повторный прогон не должен его трогать (no-op
	// по marker), что и доказывает идемпотентность через marker, а не walk.
	if err := os.WriteFile(filepath.Join(dest, "f.txt"), []byte("touched"), 0o644); err != nil {
		t.Fatalf("touch: %v", err)
	}
	second := apply(t, map[string]any{"path": src, "dest": dest})
	if second.Last().Changed {
		t.Fatal("повторный прогон при совпавшем marker: Changed=true")
	}
	got, _ := os.ReadFile(filepath.Join(dest, "f.txt"))
	if string(got) != "touched" {
		t.Fatalf("no-op перезаписал файл: %q", got)
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
		t.Fatal("Changed=false при несовпадающем marker")
	}
}

func TestApply_AutoDetectFails_UnknownExtension(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "blob.bin", []byte("x"))
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed {
		t.Fatal("Failed=false при auto-detect неудаче")
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
		t.Fatal("Failed=false для отсутствующего архива")
	}
}

func TestApply_CorruptArchive(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "broken.tar.gz", []byte("not-a-gzip-stream"))
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed {
		t.Fatal("Failed=false для битого архива")
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
		t.Fatal("zip-slip: Failed=false (ожидался fail-fast)")
	}
	if !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("zip-slip: message=%q (ожидалось escapes dest)", stream.Last().Message)
	}
	// fail-fast: запись за пределы dest не создана.
	if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
		t.Fatal("zip-slip: файл вне dest всё-таки создан (clamp вместо fail-fast)")
	}
	// marker не записан — распаковка прервана целиком.
	if _, err := os.Stat(filepath.Join(dest, archive.MarkerFile)); err == nil {
		t.Fatal("zip-slip: marker записан несмотря на провал")
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
		t.Fatalf("абсолютный путь: ожидался escape-fail, получено failed=%v msg=%q",
			stream.Last().Failed, stream.Last().Message)
	}
}

// zip-формат: path-traversal через `../` в zip-entry. Симметрично tar-кейсу,
// но проверяет ветку extractZip отдельно (zip.File.Name резолвится тем же
// resolveEntry, контракт fail-fast обязан выполняться в обеих ветках).
func TestApply_ZipSlip_Zip_FailFast(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "evil.zip", makeZip(t, []zipEntry{
		{name: "ok.txt", mode: 0o644, body: "ok"},
		{name: "../escape", mode: 0o644, body: "evil"},
	}))
	dest := filepath.Join(dir, "out")

	stream := apply(t, map[string]any{"path": src, "dest": dest})
	if !stream.Last().Failed {
		t.Fatal("zip-slip (zip): Failed=false (ожидался fail-fast)")
	}
	if !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("zip-slip (zip): message=%q (ожидалось escapes dest)", stream.Last().Message)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
		t.Fatal("zip-slip (zip): файл вне dest создан (clamp вместо fail-fast)")
	}
	if _, err := os.Stat(filepath.Join(dest, archive.MarkerFile)); err == nil {
		t.Fatal("zip-slip (zip): marker записан несмотря на провал")
	}
}

// zip-формат: абсолютный путь в zip-entry → fail-fast.
func TestApply_ZipSlip_Zip_AbsolutePath_FailFast(t *testing.T) {
	dir := t.TempDir()
	src := writeArchive(t, dir, "abs.zip", makeZip(t, []zipEntry{
		{name: "/etc/evil", mode: 0o644, body: "evil"},
	}))
	stream := apply(t, map[string]any{"path": src, "dest": filepath.Join(dir, "out")})
	if !stream.Last().Failed || !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("zip абсолютный путь: ожидался escape-fail, получено failed=%v msg=%q",
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
		t.Fatal("max_size: Failed=false при превышении")
	}
	if !strings.Contains(stream.Last().Message, "max_size") {
		t.Fatalf("max_size: message=%q (ожидалось max_size)", stream.Last().Message)
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
		t.Fatal("max_entries: Failed=false при превышении")
	}
	if !strings.Contains(stream.Last().Message, "max_entries") {
		t.Fatalf("max_entries: message=%q (ожидалось max_entries)", stream.Last().Message)
	}
}

// высокий compression-ratio (tar.gz из повторяющихся байт) отвергается
// max_ratio-проверкой, даже если суммарный размер ниже max_size: классический
// zip-bomb обходит size-лимит маленьким сжатым размером.
func TestApply_ZipBomb_Ratio_TarGz(t *testing.T) {
	dir := t.TempDir()
	// 1 MiB одинаковых байт → gzip сожмёт в ~килобайты, ratio в сотни.
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
		t.Fatal("max_ratio: Failed=false при высоком compression-ratio")
	}
	if !strings.Contains(stream.Last().Message, "max_ratio") {
		t.Fatalf("max_ratio: message=%q (ожидалось max_ratio)", stream.Last().Message)
	}
	// fail-fast: marker не записан, распаковка прервана.
	if _, err := os.Stat(filepath.Join(dir, "out", archive.MarkerFile)); err == nil {
		t.Fatal("max_ratio: marker записан несмотря на провал")
	}
}

// нормальный (низкосжимаемый) tar.gz проходит при дефолтном max_ratio:
// случайно-подобное содержимое почти не сжимается, ratio ~1.
func TestApply_Ratio_NormalArchivePasses(t *testing.T) {
	dir := t.TempDir()
	// псевдослучайный контент: gzip почти не сожмёт, ratio близок к 1.
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
		// дефолтный max_ratio=100 — нормальный архив укладывается.
	})
	if stream.Last().Failed {
		t.Fatalf("нормальный архив отвергнут max_ratio: %s", stream.Last().Message)
	}
}

// max_ratio=0 отключает проверку: высокоратио-бомба проходит (escape для
// легитимных высокосжимаемых архивов под ответственность оператора).
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
		t.Fatalf("max_ratio=0 должен отключать проверку, но Failed=%s", stream.Last().Message)
	}
}

// zip с высоким compression-ratio: per-entry CompressedSize64/Uncompressed
// учитывается, бомба ловится.
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
		t.Fatal("zip max_ratio: Failed=false при высоком ratio")
	}
	if !strings.Contains(stream.Last().Message, "max_ratio") {
		t.Fatalf("zip max_ratio: message=%q (ожидалось max_ratio)", stream.Last().Message)
	}
}

// голый tar (несжатый) НЕ проверяется по ratio даже при крошечном max_ratio:
// compressed=0, формат не сжат — бомбой по ratio быть не может, max_size его
// ограничивает.
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
		t.Fatalf("голый tar не должен проверяться по ratio: %s", stream.Last().Message)
	}
}

// отрицательный max_ratio — ошибка конфигурации.
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
		t.Fatalf("отрицательный max_ratio: failed=%v msg=%q", stream.Last().Failed, stream.Last().Message)
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
	// mode с setuid(04000)+setgid(02000)+sticky(01000)+0777.
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
		t.Fatalf("setuid/setgid/sticky не замаскированы: mode=%v", info.Mode())
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatalf("perm-биты искажены: %v want 0777", info.Mode().Perm())
	}
}

// write-through within-dest symlink: каталог subdir/ + symlink alias→subdir
// (within-dest, легитимный сам по себе) + запись alias/file.txt «сквозь» него.
// securejoin резолвит alias по уже лежащему на диске symlink-у → joined≠naive →
// шаг падает с escapes dest. Фиксирует, что защита идёт по РЕЗОЛВНУТОМУ пути, а
// не лексически: наивная замена securejoin на filepath.Join сломала бы этот тест.
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
		t.Fatal("write-through symlink: Failed=false (ожидался fail по резолвнутому пути)")
	}
	if !strings.Contains(stream.Last().Message, "escapes dest") {
		t.Fatalf("write-through symlink: message=%q (ожидалось escapes dest)", stream.Last().Message)
	}
	// файл «сквозь» symlink не материализован ни как alias/file.txt, ни в subdir.
	if _, err := os.Stat(filepath.Join(dest, "subdir", "file.txt")); err == nil {
		t.Fatal("write-through symlink: файл создан в target-каталоге (защита обойдена)")
	}
}

// граница max_size: сумма по нескольким файлам учитывается, ровно лимит проходит,
// +1 байт → failed. Фиксирует хрупкий budget := maxSize-usedSize+1 в writeFileEntry.
func TestApply_MaxSize_Boundary(t *testing.T) {
	const limit = 1024 // байт; голое число = байты

	// два файла суммарно ровно на лимите (600 + 424 = 1024) — проходит.
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
			t.Fatalf("ровно лимит (%d байт): Failed=%s", limit, stream.Last().Message)
		}
	})

	// сумма на байт больше лимита (600 + 425 = 1025) — failed.
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
			t.Fatalf("лимит+1 байт (%d): Failed=false", limit+1)
		}
		if !strings.Contains(stream.Last().Message, "max_size") {
			t.Fatalf("лимит+1: message=%q (ожидалось max_size)", stream.Last().Message)
		}
	})
}

// пустой архив (tar и zip): распаковка ok, marker пишется, повторный прогон no-op.
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
				t.Fatalf("пустой архив: Failed=%s", first.Last().Message)
			}
			if !first.Last().Changed {
				t.Fatal("пустой архив: Changed=false при первой распаковке")
			}
			marker, err := os.ReadFile(filepath.Join(dest, archive.MarkerFile))
			if err != nil {
				t.Fatalf("пустой архив: marker не записан: %v", err)
			}
			if string(marker) != sha(c.data)+"\n" {
				t.Fatalf("пустой архив: marker=%q want %q", marker, sha(c.data)+"\n")
			}

			second := apply(t, map[string]any{"path": src, "dest": dest})
			if second.Last().Changed {
				t.Fatal("пустой архив: повторный прогон Changed=true (ожидался no-op)")
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
