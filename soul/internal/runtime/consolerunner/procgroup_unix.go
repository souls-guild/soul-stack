//go:build unix

package consolerunner

import (
	"errors"
	"os/exec"
	"syscall"
)

const (
	sigHUP  = syscall.SIGHUP
	sigKILL = syscall.SIGKILL
)

// killGroup signals the whole process group led by pid. The shell is started
// with Setsid, so its pgid equals its pid, and a negative target reaches every
// process the operator spawned inside the console — not just the shell itself.
// A missing group (already reaped) is not an error worth propagating.
func killGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// signalExitCode renders a signal death as the shell convention 128+N.
// (*os.ProcessState).ExitCode reports -1 for a signalled process, which would
// otherwise be indistinguishable from "Wait failed".
func signalExitCode(ee *exec.ExitError) (int32, bool) {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return int32(128 + int(ws.Signal())), true
}
