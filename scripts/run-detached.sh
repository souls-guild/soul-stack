#!/usr/bin/env bash
# run-detached.sh — run one make target in its OWN session and process group, so
# that a SIGTERM aimed at the caller's group does not reach it.
#
# WHY THIS EXISTS
# ---------------
# The long tiers outlive the thing that starts them. `make test-integration` is
# 12-15 minutes, `e2e` and `e2e-live` are longer. An interactive agent session,
# an editor task runner or a terminal multiplexer pane that goes away in the
# meantime tears down its process group, and every child in that group gets a
# SIGTERM -- including make, including the `go test` processes under it.
#
# The corpse is the problem, not the death. What is left behind is a truncated
# log whose last line is
#
#     make: *** [Makefile:411: test-integration] Terminated
#
# and NO exit-code file. Read carelessly that looks exactly like a tier that
# failed partway, and the packages it never reached look like packages that
# passed -- so a run killed at 32 of 49 reads as "32 green" when the truth is
# "17 unknown". That is the failure mode this script removes: it either produces
# a complete verdict or says plainly that it is still running.
#
# It is deliberately NOT folded into the tier targets themselves. CI runs them in
# the foreground and needs the process tree and the exit status; detaching there
# would hand CI a zero for a run it never waited for -- the same lie in the other
# direction.
#
# USAGE
#   scripts/run-detached.sh <make-target> [VAR=value ...]
#
# Writes:
#   $LOG   the tier's full output          (default /tmp/soul-stack-<target>.log)
#   $LOG.rc  its exit code, ONLY once it finished
#   $LOG.pid the process-group leader while it runs
#
# The `.rc` file is the completion marker: its ABSENCE means "still running or
# killed", never "passed". Wait on it, do not poll the log.

set -euo pipefail

if [ "$#" -lt 1 ]; then
	echo "usage: $0 <make-target> [VAR=value ...]" >&2
	exit 2
fi

target="$1"
shift

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
log="${LOG:-/tmp/soul-stack-${target}.log}"
rc="${log}.rc"
pidfile="${log}.pid"

# Refuse to start a second one IN THIS WORKTREE. These tiers are docker-heavy and
# CPU-hungry under `-race`; two at once on one box is how a timing-sensitive test
# becomes "flaky".
#
# The check is on live processes rather than on our own pidfile, because the run
# already in flight may have been started by hand or by another tool -- which is
# exactly the case a pidfile-only guard misses. A run in a DIFFERENT worktree is
# left alone: parallel tickets each own their tree and their containers, and
# refusing there would block a neighbour for no reason.
running="$(pgrep -f "make $target" 2>/dev/null || true)"
for pid in $running; do
	[ "$pid" = "$$" ] && continue
	cwd="$(readlink "/proc/$pid/cwd" 2>/dev/null || true)"
	if [ "$cwd" = "$root" ]; then
		echo "run-detached: '$target' is already running in this worktree (PID $pid)" >&2
		echo "  root: $root" >&2
		echo "  two concurrent runs share this tree and the docker daemon; wait for it" >&2
		exit 1
	fi
done

rm -f "$log" "$rc" "$pidfile"

# setsid puts it in a new session AND a new process group, so it is no longer a
# member of the caller's. The rc is written by the wrapper itself rather than by
# the caller, so it exists if and only if the tier actually ran to completion.
setsid nohup bash -c '
	cd "$1" || exit 127
	shift
	log="$1"; shift
	target="$1"; shift
	make "$target" "$@" > "$log" 2>&1
	echo "$?" > "$log.rc"
	rm -f "$log.pid"
' _ "$root" "$log" "$target" "$@" < /dev/null > /dev/null 2>&1 &

leader=$!
echo "$leader" > "$pidfile"
disown "$leader" 2>/dev/null || true

cat <<EOF
run-detached: '$target' started in its own process group (PGID $leader)
  log:      $log
  verdict:  $rc  — written ONLY on completion; its absence is not a pass
  wait:     until [ -f $rc ]; do sleep 20; done; echo "rc=\$(cat $rc)"
EOF
