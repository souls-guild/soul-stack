package main

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// setupMaskMetrics регистрирует keeper_mask_regex_fallback_total и подключает
// process-global [audit.SetSealHooks] — наблюдаемость слоя regex-last-resort
// secret-маскинга ([ADR-010] §7.4, слой 4). Декуплинг: shared/audit не тянет
// prometheus, keeper wired-up метрику здесь поверх общего [obs.Registry] (тот же
// приём, что watchmanMetrics/renderMetrics). logger — канал warn-лога fallback-а.
//
// Метрика растёт, когда секрет поймал ТОЛЬКО regex по имени ключа (декларатив —
// schema/seal/vault — молчал): это сигнал пробела декларатива (ожидаемый класс ii
// — внутренние bootstrap_token/jwt/creds без схемы). Rate этой серии показывает,
// насколько часто маскинг держится на last-resort, а не на декларативе.
func setupMaskMetrics(reg *obs.Registry, logger *slog.Logger) {
	fallbackTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "keeper_mask_regex_fallback_total",
		Help: "Число secret-значений, пойманных ТОЛЬКО regex-last-resort secret-маскинга (декларатив schema/seal/vault молчал) — сигнал пробела декларатива.",
	})
	reg.Registerer().MustRegister(fallbackTotal)

	audit.SetSealHooks(audit.SealHooks{
		RegexFallback: func(string) { fallbackTotal.Inc() },
		Logger:        logger,
	})
}
