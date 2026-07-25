package consolerunner

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// killSession signals every process in the terminal session led by leader, and
// returns how many it hit.
//
// Killing the process group is not enough on its own. A shell attached to a pty
// is interactive, so it turns job control ON and puts each job into its OWN
// process group — `top` running in the console is NOT in the shell's group. Only
// the SESSION is common to everything the operator started, and the kernel offers
// no "signal a session" call, so we resolve it from /proc: field 6 of
// /proc/<pid>/stat is the session id.
//
// This is the backstop of the kill-on-disconnect invariant; the polite SIGHUP and
// the pty hangup run first and normally do the job. Processes that deliberately
// left the session (setsid, nohup) are out of reach by design — the same contract
// SSH gives.
func killSession(leader int, sig syscall.Signal) int {
	if leader <= 1 {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	self := os.Getpid()
	killed := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		if sessionOf(pid) != leader {
			continue
		}
		if syscall.Kill(pid, sig) == nil {
			killed++
		}
	}
	return killed
}

// sessionOf reads the session id of pid, or 0 when it cannot be determined
// (the process is gone, or /proc is not readable).
//
// The comm field may itself contain spaces and parentheses, so the fields are
// counted from the LAST ')': state, ppid, pgrp, session.
func sessionOf(pid int) int {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return 0
	}
	fields := strings.Fields(string(raw)[end+1:])
	if len(fields) < 4 {
		return 0
	}
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		return 0
	}
	return sid
}
