package vault

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterVaultMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterVaultMetrics(reg)
	if m == nil {
		t.Fatal("RegisterVaultMetrics returned nil")
	}

	// A Histogram/CounterVec doesn't publish its family before the first
	// Observe/Inc — observe both ok and error, then check family presence.
	m.ObserveRead("secret", 3*time.Millisecond, nil)
	m.ObserveRead("secret", time.Millisecond, errors.New("transport"))
	m.ObserveWrite("secret", time.Millisecond, nil)
	m.ObserveWrite("secret", time.Millisecond, errors.New("transport"))
	m.ObserveList("secret", time.Millisecond, nil)
	m.ObserveList("secret", time.Millisecond, errors.New("transport"))

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_vault_read_duration_seconds",
		"keeper_vault_read_errors_total",
		"keeper_vault_write_duration_seconds",
		"keeper_vault_write_errors_total",
		"keeper_vault_list_duration_seconds",
		"keeper_vault_list_errors_total",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestVaultMetrics_ListErrorsByKind(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterVaultMetrics(reg)

	// ObserveList uses the same notfound/error mapping as ObserveRead.
	m.ObserveList("secret", time.Millisecond, nil)
	m.ObserveList("secret", time.Millisecond, fmt.Errorf("%w: path", ErrVaultKVNotFound))
	m.ObserveList("secret", time.Millisecond, errors.New("transport down"))

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_vault_list_errors_total{kind="notfound",mount="secret"} 1`) {
		t.Errorf("list notfound count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_vault_list_errors_total{kind="error",mount="secret"} 1`) {
		t.Errorf("list error count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_vault_list_duration_seconds_count{mount="secret"} 3`) {
		t.Errorf("list duration count should be 3 for mount secret; got=\n%s", body)
	}
}

func TestRegisterVaultMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterVaultMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterVaultMetrics(reg)
}

func TestVaultMetrics_ErrorsByKind(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterVaultMetrics(reg)

	// notfound maps to ErrVaultKVNotFound (via errors.Is); everything else is error.
	m.ObserveRead("secret", time.Millisecond, nil)
	m.ObserveRead("secret", time.Millisecond, fmt.Errorf("%w: path", ErrVaultKVNotFound))
	m.ObserveRead("secret", time.Millisecond, errors.New("transport down"))

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_vault_read_errors_total{kind="notfound",mount="secret"} 1`) {
		t.Errorf("notfound count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_vault_read_errors_total{kind="error",mount="secret"} 1`) {
		t.Errorf("error count mismatch; got=\n%s", body)
	}
	// Three calls — three latency observations for mount=secret.
	if !strings.Contains(body, `keeper_vault_read_duration_seconds_count{mount="secret"} 3`) {
		t.Errorf("duration count should be 3 for mount secret; got=\n%s", body)
	}
}

func TestVaultMetrics_MountLabel(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterVaultMetrics(reg)

	m.ObserveRead("secret", time.Millisecond, nil)
	m.ObserveRead("kv-prod", time.Millisecond, nil)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_vault_read_duration_seconds_count{mount="secret"} 1`) {
		t.Errorf("secret mount sample missing; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_vault_read_duration_seconds_count{mount="kv-prod"} 1`) {
		t.Errorf("kv-prod mount sample missing; got=\n%s", body)
	}
}

func TestVaultMetrics_NilReceiver_NoOp(t *testing.T) {
	// Client can come up without the obs stack (keeper init bootstrap path
	// without a registry, unit tests). A method on a nil receiver is a no-op,
	// no panic.
	var m *VaultMetrics
	m.ObserveRead("secret", time.Second, nil)
	m.ObserveRead("secret", time.Second, errors.New("x"))
	m.ObserveWrite("secret", time.Second, nil)
	m.ObserveWrite("secret", time.Second, errors.New("x"))
	m.ObserveList("secret", time.Second, nil)
	m.ObserveList("secret", time.Second, errors.New("x"))
}
