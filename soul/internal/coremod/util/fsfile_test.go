package util_test

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    fs.FileMode
		wantErr bool
	}{
		{"empty defaults to 0644", "", 0o644, false},
		{"explicit 0600", "0600", 0o600, false},
		{"no leading zero still octal", "755", 0o755, false},
		{"setuid bits stripped to perm", "4755", 0o755, false},
		{"non-octal letters", "rwxr-xr-x", 0, true},
		{"decimal-only-but-octal-invalid", "0888", 0, true},
		{"negative", "-1", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := util.ParseMode(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseMode(%q): want error", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMode(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ParseMode(%q)=%o want %o", c.in, got, c.want)
			}
		})
	}
}

func TestAtomicWrite(t *testing.T) {
	t.Run("creates file with content and mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := util.AtomicWrite(path, []byte("data"), 0o600); err != nil {
			t.Fatalf("AtomicWrite: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode=%o want 0600", got)
		}
		if data, _ := os.ReadFile(path); string(data) != "data" {
			t.Errorf("content=%q want %q", data, "data")
		}
	})

	t.Run("overwrites existing file fully", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("old-long-content"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := util.AtomicWrite(path, []byte("new"), 0o644); err != nil {
			t.Fatalf("AtomicWrite: %v", err)
		}
		if data, _ := os.ReadFile(path); string(data) != "new" {
			t.Errorf("content=%q want %q (полная подмена, не частичная)", data, "new")
		}
	})

	// Атомарность: после успешной записи в директории не остаётся temp-файлов
	// (.<name>.tmp-*), только целевой файл.
	t.Run("no temp leftovers on success", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "target")
		if err := util.AtomicWrite(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("AtomicWrite: %v", err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 || entries[0].Name() != "target" {
			t.Fatalf("dir entries=%v want только [target] (temp утёк)", names(entries))
		}
	})

	// Ошибка создания temp (директории нет) → ошибка, цель не появляется.
	t.Run("error when dir missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-such-dir", "f")
		err := util.AtomicWrite(path, []byte("x"), 0o644)
		if err == nil {
			t.Fatal("AtomicWrite: want error (parent dir missing)")
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("target должен отсутствовать, stat err=%v", statErr)
		}
	})

	// Ошибка rename (цель — существующая директория) → ошибка, temp подчищен.
	t.Run("error when target is a directory and temp cleaned", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "asdir")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// файл внутри, чтобы rename непустого каталога точно упал
		if err := os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed child: %v", err)
		}
		err := util.AtomicWrite(target, []byte("x"), 0o644)
		if err == nil {
			t.Fatal("AtomicWrite over non-empty dir: want error")
		}
		// temp-файл (.asdir.tmp-*) не должен остаться в dir
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != "asdir" {
				t.Fatalf("temp leftover after rename failure: %s", e.Name())
			}
		}
	})
}

func TestApplyOwnership(t *testing.T) {
	// Запуск не под root: реальный chown на чужой uid невозможен. Проверяем
	// логику резолва/сравнения через подменные lookup, не трогая реальный chown
	// там, где меняться ничего не должно.

	// Baseline берётся из фактического os.Stat созданного файла, а не из
	// os.Getuid()/os.Getgid(): на darwin/BSD новый файл наследует gid
	// родительской директории, а не egid процесса. statUIDGID работает на любой
	// unix-платформе.

	mkLookupUser := func(uid uint32) func(string) (*user.User, error) {
		return func(name string) (*user.User, error) {
			return &user.User{Uid: strconv.Itoa(int(uid))}, nil
		}
	}
	mkLookupGroup := func(gid uint32) func(string) (*user.Group, error) {
		return func(name string) (*user.Group, error) {
			return &user.Group{Gid: strconv.Itoa(int(gid))}, nil
		}
	}

	t.Run("no change when owner equals current", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		fileUID, fileGID := statUIDGID(t, path)
		changed, err := util.ApplyOwnership(path, "me", "grp",
			mkLookupUser(fileUID), mkLookupGroup(fileGID))
		if err != nil {
			t.Fatalf("ApplyOwnership: %v", err)
		}
		if changed {
			t.Fatal("changed=true want false (uid/gid совпадают)")
		}
	})

	t.Run("no change when owner/group empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		changed, err := util.ApplyOwnership(path, "", "", user.Lookup, user.LookupGroup)
		if err != nil {
			t.Fatalf("ApplyOwnership: %v", err)
		}
		if changed {
			t.Fatal("changed=true want false (ничего не задано)")
		}
	})

	t.Run("stat error on missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nope")
		// Файла нет → stat падает раньше lookup, значение uid в lookup не важно.
		if _, err := util.ApplyOwnership(path, "me", "", mkLookupUser(0), user.LookupGroup); err == nil {
			t.Fatal("ApplyOwnership on missing file: want error")
		}
	})

	t.Run("lookup user error propagates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		badLookup := func(string) (*user.User, error) { return nil, errors.New("nope") }
		if _, err := util.ApplyOwnership(path, "ghost", "", badLookup, user.LookupGroup); err == nil {
			t.Fatal("ApplyOwnership unknown user: want error")
		}
	})

	t.Run("lookup group error propagates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		badLookup := func(string) (*user.Group, error) { return nil, errors.New("nope") }
		if _, err := util.ApplyOwnership(path, "", "ghost", user.Lookup, badLookup); err == nil {
			t.Fatal("ApplyOwnership unknown group: want error")
		}
	})

	// chown на «другой» uid под обычным пользователем → ошибка chown
	// (EPERM). Под root этот кейс был бы успешным — пропускаем, чтобы тест был
	// детерминирован вне CI-root.
	t.Run("chown to foreign uid errors as non-root", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("running as root: chown на чужой uid не даёт EPERM")
		}
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		fileUID, _ := statUIDGID(t, path)
		foreign := fileUID + 12345
		if _, err := util.ApplyOwnership(path, "other", "", mkLookupUser(foreign), user.LookupGroup); err == nil {
			t.Fatal("ApplyOwnership chown foreign uid: want error (EPERM)")
		}
	})
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// statUIDGID извлекает uid/gid файла через syscall.Stat_t. На unix Soul-агент
// всегда таргетит платформу с Stat_t (см. ApplyOwnership).
func statUIDGID(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: Sys() not *syscall.Stat_t", path)
	}
	return sys.Uid, sys.Gid
}

func TestAtomicWritePreserving(t *testing.T) {
	t.Run("preserve mode when modeStr empty and file existed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := util.AtomicWritePreserving(path, []byte("new"), "", "", "", user.Lookup, user.LookupGroup); err != nil {
			t.Fatalf("AtomicWritePreserving: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			t.Errorf("mode preserve: got %o, want 0640", got)
		}
		if data, _ := os.ReadFile(path); string(data) != "new" {
			t.Errorf("content: got %q, want %q", data, "new")
		}
	})

	t.Run("override mode when modeStr set", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := util.AtomicWritePreserving(path, []byte("new"), "0600", "", "", user.Lookup, user.LookupGroup); err != nil {
			t.Fatalf("AtomicWritePreserving: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode override: got %o, want 0600", got)
		}
	})

	t.Run("create path uses default mode when file absent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := util.AtomicWritePreserving(path, []byte("new"), "", "", "", user.Lookup, user.LookupGroup); err != nil {
			t.Fatalf("AtomicWritePreserving: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("create default mode: got %o, want 0644", got)
		}
	})

	t.Run("preserve uid/gid when owner/group not set", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Снимок владельца до перезаписи. Под обычным пользователем chown под
		// рутом невозможен — проверяем, что rename+preserve не изменили исходных
		// uid/gid (а не насильно меняем владельца).
		wantUID, wantGID := statUIDGID(t, path)
		if err := util.AtomicWritePreserving(path, []byte("new"), "", "", "", user.Lookup, user.LookupGroup); err != nil {
			t.Fatalf("AtomicWritePreserving: %v", err)
		}
		gotUID, gotGID := statUIDGID(t, path)
		if gotUID != wantUID || gotGID != wantGID {
			t.Errorf("ownership preserve: got uid=%d gid=%d, want uid=%d gid=%d", gotUID, gotGID, wantUID, wantGID)
		}
	})

	// Невалидный mode пробрасывается из ParseMode до записи (файл не создаётся).
	t.Run("invalid mode errors before write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		err := util.AtomicWritePreserving(path, []byte("x"), "garbage", "", "", user.Lookup, user.LookupGroup)
		if err == nil {
			t.Fatal("AtomicWritePreserving: want error on invalid mode")
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Fatal("файл не должен был создаться при ошибке mode")
		}
	})

	// Override-ветка ownership для нового файла: owner задан → ApplyOwnership
	// вызывается. Под non-root chown своего же uid даёт changed=false без ошибки.
	t.Run("new file with owner set runs ApplyOwnership branch", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root: проверяется только non-root путь без реального chown")
		}
		path := filepath.Join(t.TempDir(), "f")
		// owner резолвится в текущего пользователя → chown не нужен, ошибки нет.
		lookupSelf := func(string) (*user.User, error) {
			return &user.User{Uid: strconv.Itoa(os.Getuid())}, nil
		}
		err := util.AtomicWritePreserving(path, []byte("new"), "", "me", "", lookupSelf, user.LookupGroup)
		if err != nil {
			t.Fatalf("AtomicWritePreserving: %v", err)
		}
		if data, _ := os.ReadFile(path); string(data) != "new" {
			t.Errorf("content=%q want %q", data, "new")
		}
	})

	// Override-ветка с ошибкой ApplyOwnership (unknown user) пробрасывается.
	t.Run("owner lookup error propagates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		badLookup := func(string) (*user.User, error) { return nil, errors.New("nope") }
		err := util.AtomicWritePreserving(path, []byte("x"), "", "ghost", "", badLookup, user.LookupGroup)
		if err == nil {
			t.Fatal("AtomicWritePreserving: want error from ApplyOwnership")
		}
	})
}
