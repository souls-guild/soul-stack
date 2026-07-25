package sdnotify_test

// Sync guard between this package and the units that switch it on: a unit that
// arms WatchdogSec= without Type=notify + NotifyAccess=main gets no
// NOTIFY_SOCKET, so the daemon never pings and systemd kills it once per
// interval. The units live outside the module (deploy/systemd), so they are read
// as files.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShippedUnitsPairWatchdogWithNotify(t *testing.T) {
	for _, name := range []string{"soul.service", "keeper.service"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "deploy", "systemd", name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			unit := directives(string(raw))

			if got := unit["WatchdogSec"]; got == "" {
				t.Fatal("WatchdogSec is not set — the daemon watchdog is off")
			}
			if got := unit["Type"]; got != "notify" {
				t.Fatalf("Type=%q, want notify (WatchdogSec without it is a restart loop)", got)
			}
			if got := unit["NotifyAccess"]; got != "main" {
				t.Fatalf("NotifyAccess=%q, want main (the daemon is the only sender)", got)
			}
			if got := unit["TimeoutStartSec"]; got == "" {
				t.Fatal("TimeoutStartSec is not set — a missing READY=1 would hang the start indefinitely")
			}
		})
	}
}

// directives parses `Key=value` lines, ignoring comments; the last assignment
// wins, as in systemd.
func directives(unit string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}
