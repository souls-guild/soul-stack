//go:build unix

package config

import (
	"os"
	"syscall"
)

// statOwner extracts (uid, gid) from FileInfo on unix systems.
// On non-unix platforms (Windows) it returns ok=false and chown is skipped.
func statOwner(info os.FileInfo) (uid, gid int, ok bool) {
	st, ok2 := info.Sys().(*syscall.Stat_t)
	if !ok2 || st == nil {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
