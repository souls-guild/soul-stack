package util_test

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
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
			t.Errorf("content=%q want %q (full replacement, not partial)", data, "new")
		}
	})

	// Atomicity: after a successful write, no temp files remain
	// (.<name>.tmp-*), only the target file.
	t.Run("no temp leftovers on success", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "target")
		if err := util.AtomicWrite(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("AtomicWrite: %v", err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 || entries[0].Name() != "target" {
			t.Fatalf("dir entries=%v want only [target] (temp leaked)", names(entries))
		}
	})

	// Error creating temp (missing directory) → error, target never appears.
	t.Run("error when dir missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-such-dir", "f")
		err := util.AtomicWrite(path, []byte("x"), 0o644)
		if err == nil {
			t.Fatal("AtomicWrite: want error (parent dir missing)")
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("target must be absent, stat err=%v", statErr)
		}
	})

	// Rename error (target is an existing directory) → error, temp cleaned up.
	t.Run("error when target is a directory and temp cleaned", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "asdir")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// file inside, so renaming a non-empty dir is guaranteed to fail
		if err := os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed child: %v", err)
		}
		err := util.AtomicWrite(target, []byte("x"), 0o644)
		if err == nil {
			t.Fatal("AtomicWrite over non-empty dir: want error")
		}
		// temp file (.asdir.tmp-*) must not remain in dir
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != "asdir" {
				t.Fatalf("temp leftover after rename failure: %s", e.Name())
			}
		}
	})
}

func TestReadRegularFile(t *testing.T) {
	t.Run("reads regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("payload\n"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := util.ReadRegularFile(path)
		if err != nil {
			t.Fatalf("ReadRegularFile: %v", err)
		}
		if string(got) != "payload\n" {
			t.Fatalf("content=%q", string(got))
		}
	})

	t.Run("relative path rejected", func(t *testing.T) {
		if _, err := util.ReadRegularFile("relative/f"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
			t.Fatalf("err=%v want must-be-absolute", err)
		}
	})

	t.Run("missing file gives no-such-file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nope")
		if _, err := util.ReadRegularFile(path); err == nil || !strings.Contains(err.Error(), "no such file") {
			t.Fatalf("err=%v want no-such-file", err)
		}
	})

	t.Run("directory rejected", func(t *testing.T) {
		if _, err := util.ReadRegularFile(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err=%v want not-a-regular-file", err)
		}
	})

	// Lstat, not Stat: a symlink to a regular file is still rejected (swap protection).
	t.Run("symlink rejected via Lstat", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "real")
		if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := util.ReadRegularFile(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err=%v want not-a-regular-file (Lstat reject)", err)
		}
	})
}

func TestApplyOwnership(t *testing.T) {
	// Not running as root: a real chown to a foreign uid is impossible. We test
	// resolve/compare logic via mock lookups, without touching real chown where
	// nothing should change.

	// Baseline comes from the actual os.Stat of the created file, not from
	// os.Getuid()/os.Getgid(): on darwin/BSD a new file inherits the parent
	// dir's gid, not the process egid. statUIDGID works on any unix platform.

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
			t.Fatal("changed=true want false (uid/gid match)")
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
			t.Fatal("changed=true want false (nothing set)")
		}
	})

	t.Run("stat error on missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nope")
		// File doesn't exist → stat fails before lookup; the lookup's uid value
		// doesn't matter.
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

	// chown to a "foreign" uid as a regular user → chown error (EPERM). Under
	// root this case would succeed, so skip to keep the test deterministic
	// outside CI-root.
	t.Run("chown to foreign uid errors as non-root", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("running as root: chown on a foreign uid does not return EPERM")
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

// statUIDGID extracts a file's uid/gid via syscall.Stat_t. On unix the Soul
// agent always targets a platform with Stat_t (see ApplyOwnership).
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
		// Snapshot the owner before overwriting. As a regular user, chown to
		// root is impossible — we verify rename+preserve didn't change the
		// original uid/gid (not that we force ownership).
		wantUID, wantGID := statUIDGID(t, path)
		if err := util.AtomicWritePreserving(path, []byte("new"), "", "", "", user.Lookup, user.LookupGroup); err != nil {
			t.Fatalf("AtomicWritePreserving: %v", err)
		}
		gotUID, gotGID := statUIDGID(t, path)
		if gotUID != wantUID || gotGID != wantGID {
			t.Errorf("ownership preserve: got uid=%d gid=%d, want uid=%d gid=%d", gotUID, gotGID, wantUID, wantGID)
		}
	})

	// An invalid mode propagates from ParseMode before writing (no file created).
	t.Run("invalid mode errors before write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		err := util.AtomicWritePreserving(path, []byte("x"), "garbage", "", "", user.Lookup, user.LookupGroup)
		if err == nil {
			t.Fatal("AtomicWritePreserving: want error on invalid mode")
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Fatal("file should not have been created on a mode error")
		}
	})

	// Override branch for ownership on a new file: owner is set →
	// ApplyOwnership is called. Under non-root, chown to one's own uid gives
	// changed=false without error.
	t.Run("new file with owner set runs ApplyOwnership branch", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root: only the non-root path without a real chown is checked")
		}
		path := filepath.Join(t.TempDir(), "f")
		// owner resolves to the current user → no chown needed, no error.
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

	// Override branch with an ApplyOwnership error (unknown user) propagates.
	t.Run("owner lookup error propagates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		badLookup := func(string) (*user.User, error) { return nil, errors.New("nope") }
		err := util.AtomicWritePreserving(path, []byte("x"), "", "ghost", "", badLookup, user.LookupGroup)
		if err == nil {
			t.Fatal("AtomicWritePreserving: want error from ApplyOwnership")
		}
	})
}
