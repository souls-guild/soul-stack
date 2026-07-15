package util

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// ParseMode parses an octal mode param (e.g. "0644"). Empty string → default
// 0o644. Invalid octal string → error naming the param. Bits beyond ModePerm
// are dropped.
//
// Single source for all core modules that materialize files (core.file,
// core.url, …); don't duplicate locally.
func ParseMode(modeStr string) (fs.FileMode, error) {
	if modeStr == "" {
		return fs.FileMode(0o644), nil
	}
	parsed, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("param %q: invalid octal mode %q", "mode", modeStr)
	}
	return fs.FileMode(parsed) & fs.ModePerm, nil
}

// ReadRegularFile reads the contents of a regular file at absolute path src.
// Used by core modules that copy the contents of a file already on the host
// (core.file.present with src). Contract (security boundary):
//
//   - src must be absolute (filepath.IsAbs) — relative paths are rejected so
//     resolving against the Soul daemon's cwd (usually root) can't become a
//     footgun;
//   - type is checked via os.Lstat + IsRegular(), Lstat SPECIFICALLY: a
//     symlink is rejected, not followed — guards against swapping the source
//     for a symlink to a sensitive file between declaration and apply;
//   - directory / symlink / device / socket / fifo → explicit reject (MVP —
//     regular files only);
//   - missing-file / permission errors are propagated as-is.
//
// The file is read into memory once; the caller hashes and writes that same
// buffer (no double read — guards against TOCTOU between check and write).
func ReadRegularFile(src string) ([]byte, error) {
	if !filepath.IsAbs(src) {
		return nil, fmt.Errorf("src must be absolute: %q", src)
	}
	info, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read src %s: no such file", src)
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("src %s is not a regular file", src)
	}
	return os.ReadFile(src)
}

// AtomicWrite materializes data at path atomically: temp file in the same
// directory + rename. Guarantees an observer sees either the old file or the
// full new one, never a partial write. The temp file is removed on any error.
//
// Single source for core modules that need write atomicity (core.file
// rendered, core.url fetched); don't duplicate locally.
func AtomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("fsync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// AtomicWritePreserving materializes data at path atomically (like
// AtomicWrite), but with preserve-by-default semantics for in-place edits of
// an EXISTING file ([ADR-015], core.line pilot, pattern for future in-place
// core modules like core.repo):
//
//   - mode: modeStr=="" and the file existed → keep the original file's
//     current mode; modeStr set → ParseMode(modeStr) (override). File didn't
//     exist → ParseMode(modeStr) ("" defaults to 0644).
//   - owner/group: unset and the file existed → restore the old uid/gid; set →
//     ApplyOwnership (override). File didn't exist → ApplyOwnership only if
//     something is set (otherwise keeps the current process's owner).
//
// rename loses the original's mode/owner (the temp file is created with the
// process's own permissions), so preserve is restored EXPLICITLY after the
// write. The mode+uid+gid snapshot is taken BEFORE the write (Stat of the
// original file).
//
// lookupUser/lookupGroup — substitution points for unit tests (as in
// ApplyOwnership). A single inherited form: in-place core modules call this
// function instead of duplicating preserve logic locally.
func AtomicWritePreserving(
	path string, data []byte, modeStr, owner, group string,
	lookupUser func(name string) (*user.User, error),
	lookupGroup func(name string) (*user.Group, error),
) error {
	var (
		prevMode    fs.FileMode
		prevUID     int
		prevGID     int
		hadOwnerSys bool
	)
	info, statErr := os.Stat(path)
	existed := statErr == nil
	if existed {
		prevMode = info.Mode().Perm()
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			prevUID = int(sys.Uid)
			prevGID = int(sys.Gid)
			hadOwnerSys = true
		}
	}

	mode, err := ParseMode(modeStr)
	if err != nil {
		return err
	}
	if modeStr == "" && existed {
		mode = prevMode
	}

	if err := AtomicWrite(path, data, mode); err != nil {
		return err
	}

	if owner != "" || group != "" {
		if _, err := ApplyOwnership(path, owner, group, lookupUser, lookupGroup); err != nil {
			return err
		}
		return nil
	}
	// owner/group unset: for an existing file, rename reset the owner to the
	// process — restore the original uid/gid. For a new file, leave the
	// process owner as-is (AtomicWrite's behavior without ownership).
	if existed && hadOwnerSys {
		if err := os.Chown(path, prevUID, prevGID); err != nil {
			return fmt.Errorf("restore ownership %s: %v", path, err)
		}
	}
	return nil
}

// ApplyOwnership resolves owner/group → uid/gid, compares against current,
// and chowns if they differ. changed=true only if at least one value actually
// changed. lookupUser/lookupGroup — substitution points for unit tests
// (production uses user.Lookup / user.LookupGroup).
//
// Single source for core modules that set owner/group on files (core.file,
// core.url); don't duplicate locally.
func ApplyOwnership(
	path, owner, group string,
	lookupUser func(name string) (*user.User, error),
	lookupGroup func(name string) (*user.Group, error),
) (bool, error) {
	drift, wantUID, wantGID, err := OwnershipDrift(path, owner, group, lookupUser, lookupGroup)
	if err != nil {
		return false, err
	}
	if !drift {
		return false, nil
	}
	if err := os.Chown(path, wantUID, wantGID); err != nil {
		return false, fmt.Errorf("chown %s: %v", path, err)
	}
	return true, nil
}

// OwnershipDrift is the pure-read half of ApplyOwnership (ADR-031 Scry):
// resolves owner/group → uid/gid and compares against current WITHOUT
// chowning. Returns drift (at least one value differs) and the target
// wantUID/wantGID (for a subsequent chown in ApplyOwnership). Pure read —
// ApplyOwnership is built on top of it, and the Plan path uses it directly
// (drift without mutation).
func OwnershipDrift(
	path, owner, group string,
	lookupUser func(name string) (*user.User, error),
	lookupGroup func(name string) (*user.Group, error),
) (drift bool, wantUID, wantGID int, err error) {
	info, serr := os.Stat(path)
	if serr != nil {
		return false, 0, 0, fmt.Errorf("stat %s: %v", path, serr)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// On systems where Sys() isn't Stat_t (theoretically Windows), chown
		// makes no sense. The Soul agent targets unix, so this isn't a blocker.
		return false, 0, 0, fmt.Errorf("chown not supported on this platform")
	}
	wantUID = int(sys.Uid)
	wantGID = int(sys.Gid)
	if owner != "" {
		u, lerr := lookupUser(owner)
		if lerr != nil {
			return false, 0, 0, fmt.Errorf("lookup user %q: %v", owner, lerr)
		}
		uid, _ := strconv.Atoi(u.Uid)
		if uid != int(sys.Uid) {
			wantUID = uid
			drift = true
		}
	}
	if group != "" {
		g, lerr := lookupGroup(group)
		if lerr != nil {
			return false, 0, 0, fmt.Errorf("lookup group %q: %v", group, lerr)
		}
		gid, _ := strconv.Atoi(g.Gid)
		if gid != int(sys.Gid) {
			wantGID = gid
			drift = true
		}
	}
	return drift, wantUID, wantGID, nil
}
