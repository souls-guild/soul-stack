package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestHotReload_SignalEnabled(t *testing.T) {
	t.Parallel()
	if !(*HotReload)(nil).SignalEnabled() {
		t.Errorf("nil hot_reload → want enabled (default true)")
	}
	if !(&HotReload{EnableSignal: true}).SignalEnabled() {
		t.Errorf("enable_signal=true → want enabled")
	}
	if (&HotReload{EnableSignal: false}).SignalEnabled() {
		t.Errorf("enable_signal=false → want disabled")
	}
}

// TestLogReloads_FormatsSuccessAndFailure — LogReloads различает
// succeeded/failed по ReloadResult.Swapped и завершается на close(ch).
func TestLogReloads_FormatsSuccessAndFailure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	ch := make(chan ReloadResult, 2)
	ch <- ReloadResult{Swapped: true, Source: ReloadSourceSignal, CorrelationID: "OK1"}
	ch <- ReloadResult{Swapped: false, Source: ReloadSourceSignal, CorrelationID: "BAD1", Phase: "schema_validate"}
	close(ch)

	LogReloads(ch, logger)

	out := buf.String()
	if !strings.Contains(out, "reload succeeded") || !strings.Contains(out, "OK1") {
		t.Errorf("success line missing: %s", out)
	}
	if !strings.Contains(out, "reload failed") || !strings.Contains(out, "BAD1") || !strings.Contains(out, "schema_validate") {
		t.Errorf("failure line missing: %s", out)
	}
}
