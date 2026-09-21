package main

import (
	"context"
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/shared/sdnotify"
)

// statusRefreshInterval — cadence of the systemd STATUS= line while serving.
const statusRefreshInterval = 30 * time.Second

// startupStepBudget — how much time each finished startup step grants the
// systemd start job (EXTEND_TIMEOUT_USEC).
const startupStepBudget = 60 * time.Second

// soulCounter reports live Keeper↔Soul streams on this instance
// ([keepergrpc.StreamManager]).
type soulCounter interface{ Count() int }

// keeperStatus renders the single line `systemctl status keeper` shows under
// the unit description.
func keeperStatus(addr string, souls soulCounter) string {
	if souls == nil {
		return fmt.Sprintf("serving: api=%s", addr)
	}
	return fmt.Sprintf("serving: api=%s souls=%d", addr, souls.Count())
}

// runStatusReporter refreshes the systemd status line until ctx is done, and
// returns immediately when the daemon runs outside a Type=notify unit.
func runStatusReporter(ctx context.Context, notifier *sdnotify.Notifier, addr string, souls soulCounter, every time.Duration) {
	if !notifier.Enabled() {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			notifier.Status(keeperStatus(addr, souls))
		}
	}
}
