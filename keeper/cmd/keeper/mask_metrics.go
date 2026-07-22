package main

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// setupMaskMetrics registers keeper_mask_regex_fallback_total and wires up
// the process-global [audit.SetSealHooks] -- observability for the
// regex-last-resort secret-masking layer ([ADR-010] §7.4, layer 4).
// Decoupling: shared/audit does not pull in prometheus, keeper wires the
// metric here on top of the shared [obs.Registry] (the same approach as
// watchmanMetrics/renderMetrics). logger -- the fallback warn-log channel.
//
// The metric grows when a secret was caught ONLY by the key-name regex (the
// declarative layer -- schema/seal/vault -- stayed silent): this signals a
// declarative gap (the expected class ii -- internal bootstrap_token/jwt/creds
// without a schema). The rate of this series shows how often masking relies
// on last-resort rather than on the declarative layer.
func setupMaskMetrics(reg *obs.Registry, logger *slog.Logger) {
	fallbackTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "keeper_mask_regex_fallback_total",
		Help: "Number of secret values caught ONLY by regex-last-resort secret masking (declarative schema/seal/vault stayed silent) -- signals a declarative gap.",
	})
	reg.Registerer().MustRegister(fallbackTotal)

	audit.SetSealHooks(audit.SealHooks{
		RegexFallback: func(string) { fallbackTotal.Inc() },
		Logger:        logger,
	})
}
