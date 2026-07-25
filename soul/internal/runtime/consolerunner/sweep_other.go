//go:build !linux

package consolerunner

import "syscall"

// killSession has no portable implementation: resolving a terminal session's
// members needs /proc. `soul` ships for linux only (see .goreleaser.yaml), so
// elsewhere the process-group kill and the pty hangup are the whole teardown.
func killSession(int, syscall.Signal) int { return 0 }
