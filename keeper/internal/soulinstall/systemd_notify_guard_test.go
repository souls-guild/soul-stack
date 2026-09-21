package soulinstall_test

// Guard for the unit pushed to onboarded VMs (SSH install + cloud-init): it must
// keep the systemd supervision contract of NIM-157 — WatchdogSec only works when
// systemd hands the agent a NOTIFY_SOCKET, which it does for Type=notify with
// NotifyAccess set.

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soulinstall"
)

func TestSystemdUnit_NotifyContract(t *testing.T) {
	unit := soulinstall.SystemdUnit()
	for _, want := range []string{
		"Type=notify",
		"NotifyAccess=main",
		"WatchdogSec=",
		"TimeoutStartSec=",
		"Restart=on-failure",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("generated soul.service lacks %q:\n%s", want, unit)
		}
	}
}
