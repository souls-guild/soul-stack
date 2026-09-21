package file_test

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
	"testing"
)

// statUIDGID — uid/gid of a directory/file via syscall.Stat_t. The Soul agent
// targets unix, Stat_t is guaranteed (see util.OwnershipDrift).
func statUIDGID(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() is not *syscall.Stat_t on this platform")
	}
	return sys.Uid, sys.Gid
}

// lookupGID — a LookupGroup mock returning a fixed gid for any name.
func lookupGID(gid uint32) func(string) (*user.Group, error) {
	return func(string) (*user.Group, error) {
		return &user.Group{Gid: strconv.Itoa(int(gid))}, nil
	}
}

// lookupUID — a LookupUser mock returning a fixed uid for any name.
func lookupUID(uid uint32) func(string) (*user.User, error) {
	return func(string) (*user.User, error) {
		return &user.User{Uid: strconv.Itoa(int(uid))}, nil
	}
}

// foreignGID looks for a process supplementary group other than the file's
// gid, into which chgrp will succeed without root. Returns (gid, true) if found.
func foreignGID(t *testing.T, ownGID uint32) (uint32, bool) {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("Getgroups: %v", err)
	}
	for _, g := range groups {
		if uint32(g) != ownGID {
			return uint32(g), true
		}
	}
	return 0, false
}
