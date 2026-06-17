//go:build unix

package config

import (
	"os"
	"syscall"
)

// statOwner извлекает (uid, gid) из FileInfo на unix-системах.
// На не-unix платформах (Windows) возвращает ok=false и chown не выполняется.
func statOwner(info os.FileInfo) (uid, gid int, ok bool) {
	st, ok2 := info.Sys().(*syscall.Stat_t)
	if !ok2 || st == nil {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
