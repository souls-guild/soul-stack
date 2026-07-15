package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeFile — a "remote host" registry for tests: path → contents (+ sha256).
// Thread-safe: tests aren't parallel, but the mutex keeps the code simpler.
type fakeFile struct {
	mu       sync.Mutex
	files    map[string][]byte
	dirs     map[string]bool
	execLog  []string
	stdinLog [][]byte
}

func newFakeFile() *fakeFile {
	return &fakeFile{files: map[string][]byte{}, dirs: map[string]bool{}}
}

func (f *fakeFile) sha256(p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[p]
	if !ok {
		return "", false
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), true
}

// fakeShellSession implements Session as a mini shell emulator: understands
// `mkdir -p ...`, `test -f ... && sha256sum ... || echo MISSING`,
// `set -e; cat > path && chmod 0755 path`, `rm -rf ...`. Sufficient for unit
// tests — no sftp dependency, the Deliverer surface is 100% covered.
type fakeShellSession struct {
	fs       *fakeFile
	failNext map[string]error // error override by command substring
}

func newFakeShell(fs *fakeFile) *fakeShellSession {
	return &fakeShellSession{fs: fs, failNext: map[string]error{}}
}

func (s *fakeShellSession) Run(_ context.Context, cmd string, stdin []byte) (string, error) {
	s.fs.mu.Lock()
	s.fs.execLog = append(s.fs.execLog, cmd)
	if stdin != nil {
		buf := make([]byte, len(stdin))
		copy(buf, stdin)
		s.fs.stdinLog = append(s.fs.stdinLog, buf)
	}
	s.fs.mu.Unlock()
	for needle, err := range s.failNext {
		if strings.Contains(cmd, needle) {
			return "", err
		}
	}
	switch {
	case strings.HasPrefix(cmd, "mkdir -p"):
		paths := strings.Fields(strings.TrimPrefix(cmd, "mkdir -p"))
		s.fs.mu.Lock()
		for _, p := range paths {
			s.fs.dirs[p] = true
		}
		s.fs.mu.Unlock()
		return "", nil
	case strings.HasPrefix(cmd, "test -f "):
		// Format: test -f '<p>' && sha256sum '<p>' || echo MISSING
		// (single-quote escape, see delivery.go::remoteSha256).
		fields := strings.Fields(cmd)
		if len(fields) < 3 {
			return "", fmt.Errorf("плохая команда: %q", cmd)
		}
		path := strings.Trim(fields[2], "'")
		sum, ok := s.fs.sha256(path)
		if !ok {
			return "MISSING\n", nil
		}
		return fmt.Sprintf("%s  %s\n", sum, path), nil
	case strings.HasPrefix(cmd, "set -e; cat > "):
		// `set -e; cat > path && chmod 0755 path`
		rest := strings.TrimPrefix(cmd, "set -e; cat > ")
		// rest = "path && chmod 0755 path"
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) < 1 {
			return "", fmt.Errorf("плохая cat-команда: %q", cmd)
		}
		path := parts[0]
		s.fs.mu.Lock()
		// Confirm the dir exists.
		parent := filepath.Dir(path)
		if !s.fs.dirs[parent] {
			s.fs.mu.Unlock()
			return "", fmt.Errorf("каталог %q не создан", parent)
		}
		buf := make([]byte, len(stdin))
		copy(buf, stdin)
		s.fs.files[path] = buf
		s.fs.mu.Unlock()
		return "", nil
	case strings.HasPrefix(cmd, "rm -rf"):
		paths := strings.Fields(strings.TrimPrefix(cmd, "rm -rf"))
		s.fs.mu.Lock()
		for _, p := range paths {
			delete(s.fs.dirs, p)
			for fp := range s.fs.files {
				if strings.HasPrefix(fp, p+"/") || fp == p {
					delete(s.fs.files, fp)
				}
			}
		}
		s.fs.mu.Unlock()
		return "", nil
	default:
		return "", fmt.Errorf("неподдержанная команда: %q", cmd)
	}
}

func (s *fakeShellSession) Close() error { return nil }

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestDeliver_UploadsWhenMissing(t *testing.T) {
	fs := newFakeFile()
	sess := newFakeShell(fs)
	soulPath := writeTemp(t, "soul", "SOUL-BINARY-V1")
	modPath := writeTemp(t, "soul-mod-pkg", "MOD-PKG-V1")

	d := NewShaDeliverer()
	err := d.Deliver(context.Background(), sess, SoulSpec{
		SoulBinaryPath: soulPath,
		Modules:        []ModuleSpec{{Name: "soul-mod-pkg", Path: modPath}},
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	// Files delivered.
	if got, ok := fs.files[hostSoulDir+"/"+hostSoulFile]; !ok || string(got) != "SOUL-BINARY-V1" {
		t.Errorf("soul не доставлен, got %q ok=%v", got, ok)
	}
	if got, ok := fs.files[hostModulesDir+"/soul-mod-pkg"]; !ok || string(got) != "MOD-PKG-V1" {
		t.Errorf("модуль не доставлен, got %q ok=%v", got, ok)
	}
	// Check chmod by the presence of a subcommand in the exec log.
	var sawChmod bool
	for _, c := range fs.execLog {
		if strings.Contains(c, "chmod 0755") {
			sawChmod = true
			break
		}
	}
	if !sawChmod {
		t.Errorf("chmod 0755 не вызван; execLog: %v", fs.execLog)
	}
}

func TestDeliver_IdempotentSkipsWhenSha256Matches(t *testing.T) {
	fs := newFakeFile()
	sess := newFakeShell(fs)
	soulPath := writeTemp(t, "soul", "BIN")

	// Pre-place a file on the host with the right sha256 — Deliver must
	// skip the upload.
	fs.dirs[hostSoulDir] = true
	fs.dirs[hostModulesDir] = true
	fs.files[hostSoulDir+"/"+hostSoulFile] = []byte("BIN")

	d := NewShaDeliverer()
	if err := d.Deliver(context.Background(), sess, SoulSpec{SoulBinaryPath: soulPath}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	for _, c := range fs.execLog {
		if strings.Contains(c, "cat > ") {
			t.Errorf("upload должен быть skipped (sha256 совпал), а в логе есть cat: %q", c)
		}
	}
}

func TestDeliver_UploadsWhenSha256Differs(t *testing.T) {
	fs := newFakeFile()
	sess := newFakeShell(fs)
	soulPath := writeTemp(t, "soul", "NEW")

	fs.dirs[hostSoulDir] = true
	fs.dirs[hostModulesDir] = true
	fs.files[hostSoulDir+"/"+hostSoulFile] = []byte("OLD")

	d := NewShaDeliverer()
	if err := d.Deliver(context.Background(), sess, SoulSpec{SoulBinaryPath: soulPath}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if string(fs.files[hostSoulDir+"/"+hostSoulFile]) != "NEW" {
		t.Errorf("файл не перезаписан, got %q", fs.files[hostSoulDir+"/"+hostSoulFile])
	}
}

func TestDeliver_FailClosedOnExecError(t *testing.T) {
	fs := newFakeFile()
	sess := newFakeShell(fs)
	sess.failNext["mkdir -p"] = errors.New("permission denied")
	soulPath := writeTemp(t, "soul", "x")

	d := NewShaDeliverer()
	err := d.Deliver(context.Background(), sess, SoulSpec{SoulBinaryPath: soulPath})
	if err == nil {
		t.Fatal("ждали fail-closed на mkdir-ошибке")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("ошибка не про mkdir: %v", err)
	}
}

func TestDeliver_FailClosedOnPostVerifyMismatch(t *testing.T) {
	// Emulate the scenario "cat wrote something other than what was sent" —
	// after the upload, swap the content on the host, sha256 won't match
	// the local one.
	fs := newFakeFile()
	sess := &corruptingShell{inner: newFakeShell(fs), corruptOn: hostSoulDir + "/" + hostSoulFile}
	soulPath := writeTemp(t, "soul", "ORIGINAL")

	d := NewShaDeliverer()
	err := d.Deliver(context.Background(), sess, SoulSpec{SoulBinaryPath: soulPath})
	if err == nil {
		t.Fatal("ждали ошибку: post-verify sha256 разошёлся")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("ошибка не про sha256: %v", err)
	}
}

// corruptingShell wraps fakeShellSession and corrupts a file's contents
// after it's written — simulating "the network mangled the upload" to
// exercise post-verify.
type corruptingShell struct {
	inner     *fakeShellSession
	corruptOn string
}

func (c *corruptingShell) Run(ctx context.Context, cmd string, stdin []byte) (string, error) {
	out, err := c.inner.Run(ctx, cmd, stdin)
	if err == nil && strings.HasPrefix(cmd, "set -e; cat > ") && strings.Contains(cmd, c.corruptOn) {
		c.inner.fs.mu.Lock()
		c.inner.fs.files[c.corruptOn] = []byte("CORRUPTED")
		c.inner.fs.mu.Unlock()
	}
	return out, err
}

func (c *corruptingShell) Close() error { return c.inner.Close() }

func TestDeliver_RejectsBadModuleName(t *testing.T) {
	fs := newFakeFile()
	sess := newFakeShell(fs)
	soulPath := writeTemp(t, "soul", "x")
	d := NewShaDeliverer()
	err := d.Deliver(context.Background(), sess, SoulSpec{
		SoulBinaryPath: soulPath,
		Modules:        []ModuleSpec{{Name: "../etc/passwd", Path: soulPath}},
	})
	if err == nil {
		t.Fatal("ждали валидацию имени модуля (path traversal)")
	}
	if !strings.Contains(err.Error(), "недопустимое имя модуля") {
		t.Errorf("ошибка не про имя модуля: %v", err)
	}
}

func TestDeliver_NilSession(t *testing.T) {
	d := NewShaDeliverer()
	if err := d.Deliver(context.Background(), nil, SoulSpec{SoulBinaryPath: "/x"}); err == nil {
		t.Fatal("ждали ошибку при nil session")
	}
}

func TestDeliver_EmptySoulBinaryPath(t *testing.T) {
	fs := newFakeFile()
	sess := newFakeShell(fs)
	d := NewShaDeliverer()
	if err := d.Deliver(context.Background(), sess, SoulSpec{}); err == nil {
		t.Fatal("ждали ошибку при пустом SoulBinaryPath")
	}
}

func TestCleanup_RemovesArtifactDirs(t *testing.T) {
	fs := newFakeFile()
	fs.dirs[hostSoulDir] = true
	fs.dirs[hostModulesDir] = true
	fs.files[hostSoulDir+"/"+hostSoulFile] = []byte("SOUL")
	fs.files[hostModulesDir+"/soul-mod-pkg"] = []byte("MOD")
	sess := newFakeShell(fs)

	c := NewShaCleaner()
	if err := c.Cleanup(context.Background(), sess); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, ok := fs.files[hostSoulDir+"/"+hostSoulFile]; ok {
		t.Error("soul-бинарь не удалён")
	}
	if _, ok := fs.files[hostModulesDir+"/soul-mod-pkg"]; ok {
		t.Error("модуль не удалён")
	}
	if fs.dirs[hostSoulDir] || fs.dirs[hostModulesDir] {
		t.Error("каталоги артефактов не удалены")
	}
}

func TestCleanup_FailClosedOnExecError(t *testing.T) {
	fs := newFakeFile()
	sess := newFakeShell(fs)
	sess.failNext["rm -rf"] = errors.New("readonly fs")
	c := NewShaCleaner()
	err := c.Cleanup(context.Background(), sess)
	if err == nil {
		t.Fatal("ждали fail-closed на rm-ошибке")
	}
	if !strings.Contains(err.Error(), "rm -rf") {
		t.Errorf("ошибка не про rm: %v", err)
	}
}

func TestCleanup_NilSession(t *testing.T) {
	c := NewShaCleaner()
	if err := c.Cleanup(context.Background(), nil); err == nil {
		t.Fatal("ждали ошибку при nil session")
	}
}

func TestCleanup_PreservesLogsLayout(t *testing.T) {
	// Cleanup must not touch /var/log/soul-stack/. Emulate a log file next
	// to the artifacts and confirm the rm command doesn't touch it.
	fs := newFakeFile()
	fs.files["/var/log/soul-stack/audit.log"] = []byte("AUDIT")
	fs.dirs[hostSoulDir] = true
	fs.dirs[hostModulesDir] = true
	sess := newFakeShell(fs)

	if err := NewShaCleaner().Cleanup(context.Background(), sess); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, ok := fs.files["/var/log/soul-stack/audit.log"]; !ok {
		t.Error("Cleanup затронул /var/log/soul-stack/ — это аудит-данные, не trash")
	}
}
