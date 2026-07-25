//go:build !unix

package consolerunner

import (
	"errors"
	"os/exec"
	"syscall"
)

// Console sessions are a Unix pty feature; `soul` itself only ships for linux
// (see .goreleaser.yaml). These stubs exist so the package still compiles on
// other platforms — pty.StartWithSize fails there before anything reaches them.
const (
	sigHUP  = syscall.Signal(0x1)
	sigKILL = syscall.Signal(0x9)
)

func killGroup(int, syscall.Signal) error {
	return errors.New("console: process groups are not supported on this platform")
}

func signalExitCode(*exec.ExitError) (int32, bool) { return 0, false }
