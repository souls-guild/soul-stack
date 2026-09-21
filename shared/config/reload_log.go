package config

import (
	"log/slog"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// SignalEnabled reports whether SIGHUP reload (ADR-021(b)) is enabled for this
// hot_reload block. Default true: the `hot_reload` block in keeper.yml/soul.yml
// is optional, and its absence = default behavior (the watcher runs). An
// explicit `enable_signal: false` disables it. Nil-receiver guard — both mains
// pass the optional config pointer without a prior nil check.
func (hr *HotReload) SignalEnabled() bool {
	if hr == nil {
		return true
	}
	return hr.EnableSignal
}

// LogReloads reads SIGHUP reload results from the [WatchSIGHUP] channel and
// logs each: succeeded (with correlation_id) or failed (with phase and the
// first error diagnostic). Returns when the watcher closes the channel (on
// ctx.Done). Audit emission of config.reload_* happens inside Store.Reload —
// here only slog observability (Soul has no audit_log DB, so this is the only
// visibility channel for reloads).
func LogReloads(ch <-chan ReloadResult, logger *slog.Logger) {
	for res := range ch {
		if res.Swapped {
			logger.Info("config: SIGHUP reload succeeded",
				slog.String("correlation_id", res.CorrelationID),
				slog.String("source", string(res.Source)),
			)
			continue
		}
		attrs := []any{
			slog.String("correlation_id", res.CorrelationID),
			slog.String("phase", string(res.Phase)),
		}
		if d := firstErrorDiag(res.Diagnostics); d != nil {
			attrs = append(attrs,
				slog.String("code", d.Code),
				slog.String("detail", d.Message),
			)
		}
		logger.Warn("config: SIGHUP reload failed, keeping previous snapshot", attrs...)
	}
}

// firstErrorDiag returns the first error diagnostic for the failed-reload log.
func firstErrorDiag(ds []diag.Diagnostic) *diag.Diagnostic {
	for i := range ds {
		if ds[i].Level == diag.LevelError {
			return &ds[i]
		}
	}
	return nil
}
