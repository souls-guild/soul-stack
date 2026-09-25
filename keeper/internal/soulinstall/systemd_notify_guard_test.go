package soulinstall_test

// Guard for the unit pushed to onboarded VMs (SSH install + cloud-init): it must keep the
// systemd supervision contract of NIM-157 — WatchdogSec only works when systemd hands the
// agent a NOTIFY_SOCKET, which it does for Type=notify with NotifyAccess set.
//
// ★ TWO FORMS OF THIS CHECK DO NOT WORK, and both were found by mutating the code rather
// than by reading it (NIM-904):
//
//   - `strings.Contains(unit, "Type=notify")` is UNFALSIFIABLE here. The unit carries a
//     comment explaining why Type=notify is there, so the substring survives deleting the
//     directive and the guard stays green on a unit systemd no longer supervises.
//   - keying a FLAT map by directive name fixes that and leaves the next one: the directive
//     moved into [Unit] or [Install], where systemd answers "Unknown key name … ignoring"
//     and carries on, while a flat map still finds it. That is not hypothetical —
//     `deploy/systemd/soul.service` records StartLimit* in [Service] having left the
//     restart limiter dead.
//
// So a directive counts only outside a comment AND under its own section.

// ⚠ NOT CHECKED HERE, AND WORTH A TICKET: this unit has a THIRD spelling — the one the
// distribution package installs, `deploy/systemd/soul.service`, at the very same path
// /etc/systemd/system/soul.service. The two have drifted and nothing compares them: the
// packaged unit carries Documentation, StartLimitIntervalSec/StartLimitBurst in [Unit] and
// a soft hardening profile (NoNewPrivileges, PrivateTmp, ProtectKernelModules,
// ProtectControlGroups, RestrictSUIDSGID, LockPersonality, ReadWritePaths) that
// [soulinstall.SystemdUnit] never got. Converging them means changing this package's body,
// which is a decision of its own and not this file's to take.

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soulinstall"
)

func TestSystemdUnit_NotifyContract(t *testing.T) {
	assertNotifyContract(t, "generated soul.service", soulinstall.SystemdUnit(),
		soulinstall.SoulBinaryPath+" run --config "+soulinstall.SoulConfigPath)
}

// assertNotifyContract checks the supervision contract in directive position and in the
// right section.
func assertNotifyContract(t *testing.T, label, unit, wantExecStart string) {
	t.Helper()
	d := unitDirectives(unit)
	for _, want := range []struct{ key, value string }{
		{"Service/Type", "notify"},
		{"Service/NotifyAccess", "main"},
		{"Service/Restart", "on-failure"},
		{"Install/WantedBy", "multi-user.target"},
	} {
		if d[want.key] != want.value {
			t.Errorf("%s: %s=%q, want %q:\n%s", label, want.key, d[want.key], want.value, unit)
		}
	}
	// A watchdog with no interval and a start with no deadline are both "supervised" on
	// paper and unsupervised in fact.
	for _, key := range []string{"Service/WatchdogSec", "Service/TimeoutStartSec"} {
		if d[key] == "" {
			t.Errorf("%s: %s is unset or outside [Service]:\n%s", label, key, unit)
		}
	}
	if wantExecStart != "" && d["Service/ExecStart"] != wantExecStart {
		t.Errorf("%s: Service/ExecStart=%q, want %q", label, d["Service/ExecStart"], wantExecStart)
	}
}

// unitDirectives parses a rendered systemd unit into its directives, keyed
// `<Section>/<Key>` and dropping comments and blank lines. A key before any section header
// is keyed `/<Key>` — systemd ignores those too, so they must not satisfy a `Service/...`
// assertion.
func unitDirectives(unit string) map[string]string {
	out := map[string]string{}
	section := ""
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, ";"):
			continue
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[section+"/"+strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}
