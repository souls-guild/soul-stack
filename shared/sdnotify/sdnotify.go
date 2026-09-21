// Package sdnotify speaks the systemd notification protocol (sd_notify) for the
// Soul Stack daemons: readiness, live status, watchdog keep-alive and graceful
// stop.
//
// Enablement is runtime autodetect, never a build tag: systemd exports
// NOTIFY_SOCKET (Type=notify) and WATCHDOG_USEC (WatchdogSec=) to the service it
// starts, so the same binary reports state under a unit and degrades to a no-op
// in docker/k8s or a bare foreground run, where liveness belongs to the
// orchestrator instead.
package sdnotify

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
)

// notifySocketEnv is set by systemd for Type=notify units and is the single
// switch that turns this package on.
const notifySocketEnv = "NOTIFY_SOCKET"

// statusMaxLen caps a STATUS= payload; systemd truncates long lines anyway.
const statusMaxLen = 256

// notifyFunc matches [daemon.SdNotify] and is the seam tests replace.
type notifyFunc func(unsetEnvironment bool, state string) (bool, error)

// Notifier reports daemon state to the systemd supervisor. A nil or zero
// Notifier is a valid disabled notifier: every method is a no-op.
type Notifier struct {
	enabled  bool
	watchdog time.Duration
	notify   notifyFunc
	logger   *slog.Logger
}

// New detects the systemd environment and returns a notifier that is disabled
// outside it.
func New(logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	n := &Notifier{
		enabled: os.Getenv(notifySocketEnv) != "",
		notify:  daemon.SdNotify,
		logger:  logger,
	}
	if !n.enabled {
		return n
	}
	// SdWatchdogEnabled also checks WATCHDOG_PID, so a child process that
	// inherited the environment does not claim the parent's watchdog.
	interval, err := daemon.SdWatchdogEnabled(false)
	if err != nil {
		logger.Warn("sdnotify: unusable watchdog environment, watchdog disabled", slog.Any("error", err))
	}
	n.watchdog = interval
	return n
}

// Enabled reports whether the process runs under a Type=notify unit.
func (n *Notifier) Enabled() bool { return n != nil && n.enabled }

// WatchdogInterval returns the WatchdogSec= the unit declared, or 0 when the
// watchdog is off.
func (n *Notifier) WatchdogInterval() time.Duration {
	if n == nil {
		return 0
	}
	return n.watchdog
}

// Ready reports that startup finished and the daemon can serve; status is
// optional and shows up in `systemctl status`.
func (n *Notifier) Ready(status string) { n.send(joinState("READY=1", statusField(status))) }

// Status updates the live status line without changing the unit's state.
func (n *Notifier) Status(status string) { n.send(statusField(status)) }

// Reloading marks a config reload in flight; the caller sends [Notifier.Ready]
// again once the new snapshot is live.
func (n *Notifier) Reloading() { n.send("RELOADING=1") }

// ExtendTimeout gives the current start/stop job another `d` before systemd
// calls it hung. A daemon with a long startup calls it per step, so
// TimeoutStartSec= bounds a single step instead of the whole sequence.
func (n *Notifier) ExtendTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	n.send(fmt.Sprintf("EXTEND_TIMEOUT_USEC=%d", d.Microseconds()))
}

// Stopping reports a graceful shutdown in progress, so systemd does not read
// the closing sockets as a failure.
func (n *Notifier) Stopping(status string) { n.send(joinState("STOPPING=1", statusField(status))) }

// RunWatchdog pings WATCHDOG=1 at half the interval systemd asked for and
// returns when ctx is done, or immediately when the unit declares no
// WatchdogSec. The ping proves the process still schedules goroutines — it is
// deliberately not a Postgres/Redis health check, otherwise a dependency outage
// would turn into a cluster-wide restart loop.
func (n *Notifier) RunWatchdog(ctx context.Context) {
	if !n.Enabled() || n.watchdog <= 0 {
		return
	}
	every := max(n.watchdog/2, time.Second)
	n.logger.Info("sdnotify: systemd watchdog active",
		slog.Duration("watchdog_sec", n.watchdog),
		slog.Duration("ping_every", every),
	)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.send("WATCHDOG=1")
		}
	}
}

func (n *Notifier) send(state string) {
	if !n.Enabled() || state == "" {
		return
	}
	if _, err := n.notify(false, state); err != nil {
		n.logger.Warn("sdnotify: notification not delivered",
			slog.String("state", strings.ReplaceAll(state, "\n", " ")),
			slog.Any("error", err),
		)
	}
}

// statusField folds a status message into one protocol line: the datagram is
// newline-separated key=value, so an embedded newline would forge a field.
func statusField(status string) string {
	status = strings.Join(strings.Fields(status), " ")
	if status == "" {
		return ""
	}
	if len(status) > statusMaxLen {
		status = strings.ToValidUTF8(status[:statusMaxLen], "")
	}
	return "STATUS=" + status
}

func joinState(fields ...string) string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return strings.Join(out, "\n")
}
