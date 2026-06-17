package config

import (
	"log/slog"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// SignalEnabled — включён ли SIGHUP-reload (ADR-021(b)) по этому блоку
// hot_reload. Default true: блок `hot_reload` в keeper.yml/soul.yml
// опционален, его отсутствие = поведение по умолчанию (watcher работает).
// Явное `enable_signal: false` отключает. Nil-receiver-guard — оба main-а
// передают опц. указатель из конфига без предварительной nil-проверки.
func (hr *HotReload) SignalEnabled() bool {
	if hr == nil {
		return true
	}
	return hr.EnableSignal
}

// LogReloads читает результаты SIGHUP-reload-ов из канала [WatchSIGHUP] и
// логирует каждый: succeeded (с correlation_id) или failed (с phase и первой
// error-диагностикой). Завершается, когда watcher закрывает канал (на
// ctx.Done). Audit-эмиссия config.reload_* идёт внутри Store.Reload — здесь
// только slog-наблюдаемость (на Soul-е audit_log-БД нет, поэтому это
// единственный канал видимости reload-ов).
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

// firstErrorDiag — первая error-диагностика для лога failed-reload-а.
func firstErrorDiag(ds []diag.Diagnostic) *diag.Diagnostic {
	for i := range ds {
		if ds[i].Level == diag.LevelError {
			return &ds[i]
		}
	}
	return nil
}
