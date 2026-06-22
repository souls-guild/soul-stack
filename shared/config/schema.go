package config

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Closed enum-ы, нормированные docs/keeper/config.md и docs/soul/config.md.
var (
	enumLogLevel       = []string{"debug", "info", "warn", "error"}
	enumLogFormat      = []string{"json", "text"}
	enumOTelExporter   = []string{"otlp"}
	enumVaultAuth      = []string{"token", "approle"}
	enumConflictPolicy = []string{"warn", "fail"}
	enumCapability     = []string{
		"run_as_root",
		"network_outbound",
		"network_inbound",
		"vault_access",
		"fs_write_root",
		"exec_subprocess",
	}
)

// schemaValidate — диспетчер по типу. Принимает заполненный *KeeperConfig,
// *SoulConfig или *DestinyManifest.
func schemaValidate(path string, root *ast.MappingNode, cfg any) []diag.Diagnostic {
	switch c := cfg.(type) {
	case *KeeperConfig:
		return schemaValidateKeeper(path, root, c)
	case *SoulConfig:
		return schemaValidateSoul(path, root, c)
	case *DestinyManifest:
		return schemaValidateDestiny(path, root, c)
	case *ServiceManifest:
		return schemaValidateService(path, root, c)
	case *ScenarioManifest:
		return schemaValidateScenario(path, root, c)
	}
	return nil
}

func schemaValidateKeeper(path string, root *ast.MappingNode, c *KeeperConfig) []diag.Diagnostic {
	var out []diag.Diagnostic

	// Top-level required-блоки (docs/keeper/config.md — `default: —` без
	// пометки `optional`). KID — скаляр, остальные — mapping-блоки;
	// проверка идёт по присутствию ключа в AST, чтобы отличить «отсутствует»
	// от «zero-value».
	topKeys := topLevelKeys(root)
	for _, req := range []struct{ key, hint string }{
		{"kid", "set kid: <keeper-instance-id>, see docs/keeper/config.md → kid"},
		{"listen", "declare listen.grpc / openapi / mcp / metrics, see docs/keeper/config.md → listen"},
		{"postgres", "declare postgres.dsn_ref + pool, see docs/keeper/config.md → postgres"},
		{"redis", "declare redis.addr + password_ref, see docs/keeper/config.md → redis"},
		{"vault", "declare vault.addr + auth + pki_mount, see docs/keeper/config.md → vault"},
	} {
		if !topKeys[req.key] {
			out = append(out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File:     path,
				Code:     "missing_required_field",
				Message:  fmt.Sprintf("%s is required at top-level", req.key),
				Hint:     req.hint,
				YAMLPath: "$." + req.key,
			})
		}
	}

	// listen: должен быть присутствующим и непустым по содержимому. Если
	// сам ключ есть, но все listener-ы пусты — это та же ошибка, что
	// «listen отсутствует» по контракту docs/keeper/config.md.
	if topKeys["listen"] {
		if c.Listen.GRPC.Bootstrap.Addr == "" && c.Listen.GRPC.EventStream.Addr == "" &&
			c.Listen.OpenAPI.Addr == "" &&
			c.Listen.MCP.Addr == "" && c.Listen.Metrics.Addr == "" {
			out = append(out, atPath(root, "$.listen", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: "listen must declare at least one of grpc.bootstrap.addr / grpc.event_stream.addr / openapi.addr / mcp.addr / metrics.addr",
				Hint:    "all listeners are required per docs/keeper/config.md → listen",
			}))
		}
		// MCP listener — обязательный по сквозному требованию «встроенный
		// MCP» (requirements.md → Архитектурные требования). Отключение
		// запрещено грамматикой (docs/keeper/config.md → listen.mcp.addr,
		// без `optional`).
		if c.Listen.MCP.Addr == "" {
			out = append(out, atPath(root, "$.listen.mcp.addr", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "mcp_listener_required",
				Message: "listen.mcp.addr is required",
				Hint:    "MCP listener is mandatory per requirements.md → «встроенный MCP»; see docs/keeper/config.md → listen.mcp",
			}))
		}
		// gRPC sub-listener-ы оба обязательны по ADR-002 / ADR-012: Soul
		// gRPC = mandatory transport, без обоих listener-ов keeper не
		// может ни принять онбординг, ни поднять EventStream.
		if c.Listen.GRPC.Bootstrap.Addr == "" {
			out = append(out, atPath(root, "$.listen.grpc.bootstrap.addr", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "grpc_bootstrap_listener_required",
				Message: "listen.grpc.bootstrap.addr is required",
				Hint:    "Bootstrap listener is mandatory per ADR-012(b); see docs/keeper/config.md → listen.grpc.bootstrap",
			}))
		}
		if c.Listen.GRPC.EventStream.Addr == "" {
			out = append(out, atPath(root, "$.listen.grpc.event_stream.addr", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "grpc_event_stream_listener_required",
				Message: "listen.grpc.event_stream.addr is required",
				Hint:    "EventStream listener is mandatory per ADR-012(a); see docs/keeper/config.md → listen.grpc.event_stream",
			}))
		}
		// Port-conflict: Bootstrap и EventStream обязаны слушать на
		// разных адресах (разные TLS-режимы — server-only vs mTLS).
		if c.Listen.GRPC.Bootstrap.Addr != "" &&
			c.Listen.GRPC.EventStream.Addr != "" &&
			c.Listen.GRPC.Bootstrap.Addr == c.Listen.GRPC.EventStream.Addr {
			out = append(out, atPath(root, "$.listen.grpc.event_stream.addr", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "bootstrap_eventstream_port_conflict",
				Message: fmt.Sprintf("listen.grpc.bootstrap.addr and listen.grpc.event_stream.addr must differ (both = %q)", c.Listen.GRPC.Bootstrap.Addr),
				Hint:    "Bootstrap and EventStream use distinct TLS modes (server-only vs mTLS); allocate separate ports",
			}))
		}
	}

	out = append(out, checkHostPort(root, "$.listen.grpc.bootstrap.addr", c.Listen.GRPC.Bootstrap.Addr)...)
	out = append(out, checkHostPort(root, "$.listen.grpc.event_stream.addr", c.Listen.GRPC.EventStream.Addr)...)
	out = append(out, checkHostPort(root, "$.listen.openapi.addr", c.Listen.OpenAPI.Addr)...)
	out = append(out, checkHostPort(root, "$.listen.mcp.addr", c.Listen.MCP.Addr)...)
	out = append(out, checkHostPort(root, "$.listen.metrics.addr", c.Listen.Metrics.Addr)...)
	out = append(out, checkHostPort(root, "$.redis.addr", c.Redis.Addr)...)

	if c.Postgres.Pool.Min != 0 || c.Postgres.Pool.Max != 0 {
		if c.Postgres.Pool.Min < 1 {
			out = append(out, atPath(root, "$.postgres.pool.min", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "pool_size_invalid",
				Message: fmt.Sprintf("postgres.pool.min must be >= 1, got %d", c.Postgres.Pool.Min),
			}))
		}
		if c.Postgres.Pool.Max < c.Postgres.Pool.Min {
			out = append(out, atPath(root, "$.postgres.pool.max", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code: "pool_size_invalid",
				Message: fmt.Sprintf("postgres.pool.max (%d) must be >= postgres.pool.min (%d)",
					c.Postgres.Pool.Max, c.Postgres.Pool.Min),
			}))
		}
	}

	// event_stream send-лимит ApplyRequest. 0 = «не задан» (дефолт
	// DefaultMaxApplySizeMB); заданное значение обязано быть ≥ MinMaxApplySizeMB.
	out = append(out, validateMaxApplySize(root,
		"$.listen.grpc.event_stream.max_apply_size_mb", c.Listen.GRPC.EventStream.MaxApplySizeMB)...)

	out = append(out, validateVaultAuth(root, &c.Vault, topKeys["vault"])...)

	if c.OTel != nil {
		if c.OTel.Exporter != "" {
			out = append(out, checkEnum(root, "$.otel.exporter", c.OTel.Exporter, enumOTelExporter)...)
		}
		if c.OTel.Enabled && c.OTel.Endpoint == "" {
			out = append(out, atPath(root, "$.otel.endpoint", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "otel_endpoint_required",
				Message: "otel.endpoint is required when otel.enabled is true",
			}))
		}
		if c.OTel.Endpoint != "" {
			out = append(out, checkHostPort(root, "$.otel.endpoint", c.OTel.Endpoint)...)
		}
	}

	if c.Metrics != nil && c.Metrics.Auth != nil && c.Metrics.Auth.Basic != nil {
		out = append(out, validateMetricsBasicAuth(root, c.Metrics.Auth.Basic)...)
	}

	if c.Sigil != nil {
		out = append(out, validateSigil(root, c.Sigil)...)
	}

	if c.Push != nil {
		out = append(out, validatePush(root, c.Push)...)
	}

	out = append(out, validateLogging(root, "$.logging", c.Logging.Level, c.Logging.Format, c.Logging.File, c.Logging.Rotation)...)

	if c.PluginRuntime != nil {
		out = append(out, validatePluginRuntime(root, "$.plugin_runtime", c.PluginRuntime)...)
	}

	if c.Plugins != nil && c.Plugins.CacheRoot != "" {
		if !filepath.IsAbs(c.Plugins.CacheRoot) {
			out = append(out, atPath(root, "$.plugins.cache_root", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "path_not_absolute",
				Message: fmt.Sprintf("plugins.cache_root must be an absolute path, got %q", c.Plugins.CacheRoot),
				Hint:    "use e.g. /var/lib/soul-stack-keeper/plugins",
			}))
		}
	}

	// plugins.work_root — корень рабочих git-клонов резолвера (ADR-026 F-fetch).
	// Абсолютный путь по тому же образцу, что cache_root. СТРОГО вне cache_root:
	// если оба заданы и work_root лежит внутри cache_root — .git/checkout попали
	// бы в кеш, читаемый Discover/ReadSlot (нарушение инварианта раскладки).
	if c.Plugins != nil && c.Plugins.WorkRoot != "" {
		if !filepath.IsAbs(c.Plugins.WorkRoot) {
			out = append(out, atPath(root, "$.plugins.work_root", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "path_not_absolute",
				Message: fmt.Sprintf("plugins.work_root must be an absolute path, got %q", c.Plugins.WorkRoot),
				Hint:    "use e.g. /var/lib/soul-stack-keeper/plugin-src",
			}))
		} else if c.Plugins.CacheRoot != "" && filepath.IsAbs(c.Plugins.CacheRoot) && pathWithin(c.Plugins.WorkRoot, c.Plugins.CacheRoot) {
			out = append(out, atPath(root, "$.plugins.work_root", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "plugins_work_root_within_cache_root",
				Message: fmt.Sprintf("plugins.work_root (%q) must be outside plugins.cache_root (%q)", c.Plugins.WorkRoot, c.Plugins.CacheRoot),
				Hint:    "git checkout/.git must not land inside the slot cache read by Discover/ReadSlot",
			}))
		}
	}

	// plugins.max_artifact_size_mb / max_clone_size_mb — size-лимиты git-egress
	// hardening (ADR-026(g)). 0 = «не задан» (дефолт в Resolved*); заданное
	// значение обязано быть ≥ MinPluginSizeMB — сабмегабайтный потолок отверг бы
	// любой реальный Go-бинарь плагина, превратив hardening в постоянный
	// fail-closed.
	if c.Plugins != nil {
		out = append(out, validatePluginSizeMB(root,
			"$.plugins.max_artifact_size_mb", c.Plugins.MaxArtifactSizeMB)...)
		out = append(out, validatePluginSizeMB(root,
			"$.plugins.max_clone_size_mb", c.Plugins.MaxCloneSizeMB)...)
	}

	if c.Audit != nil && c.Audit.RetentionDays != 0 && c.Audit.RetentionDays < 1 {
		out = append(out, atPath(root, "$.audit.retention_days", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("audit.retention_days must be >= 1, got %d", c.Audit.RetentionDays),
		}))
	}

	// reaper.batch_size: 0 = «не задан» (берётся дефолт runner-а). Любое
	// заданное значение обязано быть >= 1: 0-через-явный-ноль или отрицательное
	// — это бессмысленный размер чанка, который runner молча трактовал бы как
	// дефолт или зациклил бы выборку.
	if c.Reaper != nil && c.Reaper.BatchSize < 0 {
		out = append(out, atPath(root, "$.reaper.batch_size", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("reaper.batch_size must be >= 1, got %d", c.Reaper.BatchSize),
		}))
	}

	// keeper.acolytes (ADR-027): feature-flag числа воркеров пула исполнения.
	// 0 = «не задан» → пул не поднимается (прежний run-goroutine-путь).
	// Отрицательное — бессмысленный размер пула.
	if c.Acolytes < 0 {
		out = append(out, atPath(root, "$.acolytes", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("acolytes must be >= 0, got %d", c.Acolytes),
		}))
	}

	// keeper.acolyte_batch (ADR-027(d)): размер пачки одного claim-тика.
	// 0 = «не задан» → дефолт DefaultAcolyteBatch. Отрицательное — бессмысленный
	// LIMIT (симметрично reaper.batch_size).
	if c.AcolyteBatch < 0 {
		out = append(out, atPath(root, "$.acolyte_batch", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("acolyte_batch must be >= 0, got %d", c.AcolyteBatch),
		}))
	}

	// keeper.oracle_circuit_max_fires (ADR-030(a), beacons S4): порог авто-disable
	// Decree-а. nil (опущено) → дефолт DefaultOracleCircuitMaxFires в daemon;
	// явный 0 = breaker OFF (escape-hatch); отрицательное — бессмысленный порог.
	if c.OracleCircuitMaxFires != nil && *c.OracleCircuitMaxFires < 0 {
		out = append(out, atPath(root, "$.oracle_circuit_max_fires", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("oracle_circuit_max_fires must be >= 0, got %d", *c.OracleCircuitMaxFires),
		}))
	}

	// keeper.watchman_fail_threshold (soul-shedding S2): число подряд провалов
	// probe до shedding-а. 0 = «не задан» → дефолт DefaultWatchmanFailThreshold.
	// Отрицательное — бессмысленный порог (симметрично acolyte_batch).
	if c.WatchmanFailThreshold < 0 {
		out = append(out, atPath(root, "$.watchman_fail_threshold", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("watchman_fail_threshold must be >= 0, got %d", c.WatchmanFailThreshold),
		}))
	}

	// keeper.toll.threshold (Toll cluster detector, ADR-038): доля от baseline
	// `souls.status='connected'`. 0 (не задано) → дефолт DefaultTollThreshold;
	// явно указанный 0 трактуем как «не задан» (ноль-порог бессмыслен — будет
	// срабатывать на первом disconnect). Отрицательное и > 1 — out of range.
	if c.Toll != nil {
		if c.Toll.Threshold < 0 || c.Toll.Threshold > 1 {
			out = append(out, atPath(root, "$.toll.threshold", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("toll.threshold must be in (0, 1], got %v", c.Toll.Threshold),
			}))
		}

		// keeper.toll.per_coven_thresholds[*]: симметрично top-level threshold —
		// диапазон (0, 1]; пустая coven-метка ключа отвергается (member-value в
		// sorted-set несёт coven=" "; пустой ключ === global, для этого есть
		// top-level threshold).
		for coven, thr := range c.Toll.PerCovenThresholds {
			if coven == "" {
				out = append(out, atPath(root, "$.toll.per_coven_thresholds", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "value_out_of_range",
					Message: "toll.per_coven_thresholds: empty coven key not allowed (use top-level threshold for global)",
				}))
				continue
			}
			if thr <= 0 || thr > 1 {
				out = append(out, atPath(root, fmt.Sprintf("$.toll.per_coven_thresholds.%s", coven), diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "value_out_of_range",
					Message: fmt.Sprintf("toll.per_coven_thresholds[%q] must be in (0, 1], got %v", coven, thr),
				}))
			}
		}

		// keeper.toll.webhook (ADR-038 amendment, extensions): enabled → обязательны
		// url_ref + валидный format (closed enum). При enabled=false (или nil) —
		// валидаций нет (notifier не поднимется, в daemon-gate).
		if w := c.Toll.Webhook; w != nil && w.Enabled {
			if strings.TrimSpace(w.URLRef) == "" {
				out = append(out, atPath(root, "$.toll.webhook.url_ref", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "missing_required_field",
					Message: "toll.webhook.url_ref is required when toll.webhook.enabled=true",
					Hint:    "use vault:<mount>/<path> (recommended) or inline https://... URL",
				}))
			}
			if w.Format != "" &&
				w.Format != TollWebhookFormatGeneric &&
				w.Format != TollWebhookFormatPagerDutyV2 &&
				w.Format != TollWebhookFormatSlack {
				out = append(out, atPath(root, "$.toll.webhook.format", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code: "value_not_in_enum",
					Message: fmt.Sprintf("toll.webhook.format must be one of [%s, %s, %s], got %q",
						TollWebhookFormatGeneric, TollWebhookFormatPagerDutyV2, TollWebhookFormatSlack, w.Format),
				}))
			}
		}
	}

	// keeper.tempo.voyage_create.{rate,burst} / voyage_preview.{rate,burst}
	// (Tempo rate-limiter, ADR-050 + amendment 2026-06-17 — отдельный bucket
	// preview): rate (rps, refill-скорость) и burst (глубина бакета, capacity)
	// при ЯВНОМ задании не могут быть отрицательными (refill-скорость / capacity
	// ≥ 0); 0 либо опущенное поле резолвится к дефолту в Resolved*, поэтому
	// валидатор режет только < 0 (0 проходит и подменяется дефолтом). enabled
	// (любой bool) валиден; нулевой блок → дефолты.
	if c.Tempo != nil {
		if c.Tempo.VoyageCreate.Rate < 0 {
			out = append(out, atPath(root, "$.tempo.voyage_create.rate", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("tempo.voyage_create.rate must not be negative, got %v", c.Tempo.VoyageCreate.Rate),
			}))
		}
		if c.Tempo.VoyageCreate.Burst < 0 {
			out = append(out, atPath(root, "$.tempo.voyage_create.burst", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("tempo.voyage_create.burst must not be negative, got %d", c.Tempo.VoyageCreate.Burst),
			}))
		}
		if c.Tempo.VoyagePreview.Rate < 0 {
			out = append(out, atPath(root, "$.tempo.voyage_preview.rate", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("tempo.voyage_preview.rate must not be negative, got %v", c.Tempo.VoyagePreview.Rate),
			}))
		}
		if c.Tempo.VoyagePreview.Burst < 0 {
			out = append(out, atPath(root, "$.tempo.voyage_preview.burst", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("tempo.voyage_preview.burst must not be negative, got %d", c.Tempo.VoyagePreview.Burst),
			}))
		}
	}

	return out
}

func schemaValidateSoul(path string, root *ast.MappingNode, c *SoulConfig) []diag.Diagnostic {
	var out []diag.Diagnostic

	// Top-level required: блок `keeper:` с непустым `endpoints[]`.
	// docs/soul/config.md → keeper.endpoints — обязательное.
	topKeys := topLevelKeys(root)
	if !topKeys["keeper"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File:     path,
			Code:     "missing_required_field",
			Message:  "keeper is required at top-level",
			Hint:     "declare keeper.endpoints[] (fallback list of Keeper-cluster addresses)",
			YAMLPath: "$.keeper",
		})
	} else if len(c.Keeper.Endpoints) == 0 {
		out = append(out, atPath(root, "$.keeper.endpoints", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "keeper.endpoints must be a non-empty list",
			Hint:    "at least one endpoint is required (fallback-list per docs/soul/connection.md)",
		}))
	}

	for i, ep := range c.Keeper.Endpoints {
		// host — обязателен (общий для обеих фаз; docs/soul/config.md →
		// keeper.endpoints[].host).
		if ep.Host == "" {
			out = append(out, atPath(root, fmt.Sprintf("$.keeper.endpoints[%d].host", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: fmt.Sprintf("keeper.endpoints[%d].host is required", i),
				Hint:    "Keeper-cluster host shared by both bootstrap and event-stream phases; see docs/soul/config.md → keeper.endpoints",
			}))
		}
		// Оба порта обязательны по решению architect (ADR-012 — два
		// listener-а; «безопасность на первом месте»: явность > краткости,
		// никакого молчаливого ухода bootstrap на event_stream-порт).
		if ep.EventStreamPort == 0 {
			out = append(out, atPath(root, fmt.Sprintf("$.keeper.endpoints[%d].event_stream_port", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "event_stream_port_required",
				Message: fmt.Sprintf("keeper.endpoints[%d].event_stream_port is required", i),
				Hint:    "EventStream listener port (mTLS, `soul run`); see docs/soul/config.md → keeper.endpoints",
			}))
		} else {
			out = append(out, checkPort(root, fmt.Sprintf("$.keeper.endpoints[%d].event_stream_port", i), ep.EventStreamPort)...)
		}
		if ep.BootstrapPort == 0 {
			out = append(out, atPath(root, fmt.Sprintf("$.keeper.endpoints[%d].bootstrap_port", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "bootstrap_port_required",
				Message: fmt.Sprintf("keeper.endpoints[%d].bootstrap_port is required", i),
				Hint:    "Bootstrap listener port (server-only TLS, `soul init`); mandatory per ADR-012(b); see docs/soul/config.md → keeper.endpoints",
			}))
		} else {
			out = append(out, checkPort(root, fmt.Sprintf("$.keeper.endpoints[%d].bootstrap_port", i), ep.BootstrapPort)...)
		}
		// priority: 0 = «не задано» (normalizedPriority маппит 0→1, default).
		// Отвергаем только отрицательное; см. docs/soul/connection.md.
		if ep.Priority < 0 {
			out = append(out, atPath(root, fmt.Sprintf("$.keeper.endpoints[%d].priority", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("keeper.endpoints[%d].priority must not be negative (0 = не задано → default 1), got %d", i, ep.Priority),
			}))
		}
	}
	// keeper.retry.max_attempts: 0 = «не задано» (резолв в дефолт connection-runner-а).
	// Отвергаем только отрицательное — невозможное число попыток; молча
	// трактовалось бы как дефолт либо как «никогда не подключаться».
	if c.Keeper.Retry != nil && c.Keeper.Retry.MaxAttempts < 0 {
		out = append(out, atPath(root, "$.keeper.retry.max_attempts", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("keeper.retry.max_attempts must not be negative (0 = не задано → default), got %d", c.Keeper.Retry.MaxAttempts),
		}))
	}
	if c.OTel != nil {
		if c.OTel.Enabled && c.OTel.Endpoint == "" {
			out = append(out, atPath(root, "$.otel.endpoint", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "otel_endpoint_required",
				Message: "otel.endpoint is required when otel.enabled is true",
			}))
		}
		if c.OTel.Endpoint != "" {
			out = append(out, checkHostPort(root, "$.otel.endpoint", c.OTel.Endpoint)...)
		}
	}
	if c.Metrics != nil && c.Metrics.Listen != "" {
		out = append(out, checkHostPort(root, "$.metrics.listen", c.Metrics.Listen)...)
	}
	if c.Metrics != nil && c.Metrics.BasicAuth != nil {
		out = append(out, validateSoulMetricsBasicAuth(root, c.Metrics.BasicAuth)...)
	}
	// keeper.max_apply_size_mb — recv-лимит входящего ApplyRequest. 0 = «не
	// задан» (дефолт DefaultMaxApplySizeMB); заданное значение обязано быть
	// ≥ MinMaxApplySizeMB.
	out = append(out, validateMaxApplySize(root,
		"$.keeper.max_apply_size_mb", c.Keeper.MaxApplySizeMB)...)

	out = append(out, validateLogging(root, "$.logging", c.Logging.Level, c.Logging.Format, c.Logging.File, c.Logging.Rotation)...)
	if c.PluginRuntime != nil {
		out = append(out, validatePluginRuntime(root, "$.plugin_runtime", c.PluginRuntime)...)
	}
	return out
}

// validateMaxApplySize — общая range-проверка лимита ApplyRequest для обеих
// сторон (Keeper-send / Soul-recv). 0 трактуется как «не задан» (дефолт
// DefaultMaxApplySizeMB применяется в Resolved*); любое заданное значение
// обязано быть ≥ MinMaxApplySizeMB — иначе реальный Destiny упёрся бы в
// fail-fast (Keeper) / отказ (Soul) на сабмегабайтном потолке.
func validateMaxApplySize(root *ast.MappingNode, yamlPath string, mb int) []diag.Diagnostic {
	if mb == 0 {
		return nil
	}
	if mb < MinMaxApplySizeMB {
		return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("%s must be >= %d (MiB), got %d", yamlPath, MinMaxApplySizeMB, mb),
			Hint:    "Keeper-send-лимит должен быть ≤ Soul-recv-лимиту; дефолт обоих — 8 MiB",
		})}
	}
	return nil
}

// validatePluginSizeMB — range-проверка size-лимитов резолва плагина
// (`plugins.max_artifact_size_mb` / `max_clone_size_mb`, ADR-026(g)). 0 = «не
// задан» (дефолт применяется в Resolved*-методах); любое заданное значение
// обязано быть ≥ MinPluginSizeMB.
func validatePluginSizeMB(root *ast.MappingNode, yamlPath string, mb int) []diag.Diagnostic {
	if mb == 0 {
		return nil
	}
	if mb < MinPluginSizeMB {
		return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("%s must be >= %d (MiB), got %d", yamlPath, MinPluginSizeMB, mb),
			Hint:    "сабмегабайтный потолок отверг бы любой реальный бинарь плагина; дефолты — 256 MiB (артефакт) / 1024 MiB (клон)",
		})}
	}
	return nil
}

func validateLogging(root *ast.MappingNode, prefix, level, format, file string, rotation *LoggingRotation) []diag.Diagnostic {
	var out []diag.Diagnostic
	if level != "" {
		out = append(out, checkEnum(root, prefix+".level", level, enumLogLevel)...)
	}
	if format != "" {
		out = append(out, checkEnum(root, prefix+".format", format, enumLogFormat)...)
	}
	// `file` — путь к лог-файлу; пустой = stderr. Если задан, обязан быть
	// абсолютным (по образцу plugins.cache_root → path_not_absolute).
	if file != "" && !filepath.IsAbs(file) {
		out = append(out, atPath(root, prefix+".file", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "path_not_absolute",
			Message: fmt.Sprintf("%s.file must be an absolute path, got %q", prefix, file),
			Hint:    "use e.g. /var/log/soul/soul.log",
		}))
	}
	if rotation != nil {
		// 0 = «не задано» (берётся дефолт билдера, shared/log) для всех трёх
		// числовых полей ротации; отрицательное — невозможный размер/счётчик,
		// который иначе ушёл бы в lumberjack как мусор.
		if rotation.MaxAgeDays < 0 {
			out = append(out, atPath(root, prefix+".rotation.max_age_days", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("%s.rotation.max_age_days must be >= 0, got %d", prefix, rotation.MaxAgeDays),
			}))
		}
		if rotation.MaxSizeMB < 0 {
			out = append(out, atPath(root, prefix+".rotation.max_size_mb", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("%s.rotation.max_size_mb must be >= 0, got %d", prefix, rotation.MaxSizeMB),
			}))
		}
		if rotation.MaxFiles < 0 {
			out = append(out, atPath(root, prefix+".rotation.max_files", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("%s.rotation.max_files must be >= 0, got %d", prefix, rotation.MaxFiles),
			}))
		}
	}
	return out
}

func validatePluginRuntime(root *ast.MappingNode, prefix string, p *PluginRuntime) []diag.Diagnostic {
	var out []diag.Diagnostic
	if p.ConflictPolicy != "" {
		out = append(out, checkEnum(root, prefix+".conflict_policy", p.ConflictPolicy, enumConflictPolicy)...)
	}
	for i, c := range p.AllowedCapabilities {
		yp := fmt.Sprintf("%s.allowed_capabilities[%d]", prefix, i)
		if !contains(enumCapability, c) {
			out = append(out, atPath(root, yp, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "unknown_capability",
				Message: fmt.Sprintf("unknown capability %q", c),
				Hint:    fmt.Sprintf("allowed: %v", enumCapability),
			}))
		}
	}
	if p.EnableTLS {
		out = append(out, atPath(root, prefix+".enable_tls", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "tls_not_implemented",
			Message: "plugin_runtime.enable_tls: true is not implemented in MVP",
			Hint:    "use file-permissions 0700 on plugin socket; ADR-020(h)",
		}))
	}
	return out
}

// validateMetricsBasicAuth проверяет блок `metrics.auth.basic` (ADR-024):
// при `enabled: true` обязательны `username` и `password_ref`, причём
// password_ref обязан быть vault-ref-ом (`vault:<mount>/<path>`) — plaintext-
// пароль в конфиге запрещён («безопасность на первом месте»).
//
// При `enabled: false` поля не требуются (можно держать заготовку с пустыми
// значениями), но если password_ref задан — он всё равно валидируется как
// vault-ref, чтобы опечатку поймать до включения.
func validateMetricsBasicAuth(root *ast.MappingNode, b *KeeperMetricsBasicAuth) []diag.Diagnostic {
	var out []diag.Diagnostic
	if b.Enabled {
		if b.Username == "" {
			out = append(out, atPath(root, "$.metrics.auth.basic.username", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: "metrics.auth.basic.username is required when metrics.auth.basic.enabled is true",
				Hint:    "set a non-empty username; see docs/keeper/config.md → metrics.auth.basic",
			}))
		}
		if b.PasswordRef == "" {
			out = append(out, atPath(root, "$.metrics.auth.basic.password_ref", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: "metrics.auth.basic.password_ref is required when metrics.auth.basic.enabled is true",
				Hint:    "use a vault-ref, e.g. vault:secret/keeper/metrics-password",
			}))
		}
	}
	if b.PasswordRef != "" && !isVaultRef(b.PasswordRef) {
		out = append(out, atPath(root, "$.metrics.auth.basic.password_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_invalid",
			Message: fmt.Sprintf("metrics.auth.basic.password_ref must be a vault-ref (vault:<mount>/<path>), got %q", b.PasswordRef),
			Hint:    "plaintext passwords are not allowed; use vault:secret/keeper/metrics-password",
		}))
	}
	return out
}

// validateSoulMetricsBasicAuth проверяет блок soul-`metrics.basic_auth`
// (ADR-024): при `enabled: true` обязательны `username` и `password_file`.
//
// Зеркало keeper-`metrics.auth.basic`, но источник пароля — путь к файлу, не
// vault-ref (у Soul нет vault-клиента, ADR-012). Существование/читаемость
// файла здесь НЕ проверяется (это runtime-граница: файл может появиться
// между валидацией и стартом, проверять в схеме = ложные срабатывания на
// `soul-lint` офлайн); fail-fast на отсутствие — в main при резолве кред.
func validateSoulMetricsBasicAuth(root *ast.MappingNode, b *SoulMetricsBasicAuth) []diag.Diagnostic {
	var out []diag.Diagnostic
	if !b.Enabled {
		return out
	}
	if b.Username == "" {
		out = append(out, atPath(root, "$.metrics.basic_auth.username", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "metrics.basic_auth.username is required when metrics.basic_auth.enabled is true",
			Hint:    "set a non-empty username; see docs/soul/config.md → metrics.basic_auth",
		}))
	}
	if b.PasswordFile == "" {
		out = append(out, atPath(root, "$.metrics.basic_auth.password_file", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "metrics.basic_auth.password_file is required when metrics.basic_auth.enabled is true",
			Hint:    "point to a file holding the password, e.g. /etc/soul/metrics-password (mode 0400)",
		}))
	}
	return out
}

// validatePush проверяет блок `push` (pilot wire-up SshDispatcher, ADR-032
// amendment 2026-05-26). Блок целиком optional: пустой/отсутствующий →
// push-orchestrator не поднимается (см. setupPushDispatchers).
//
// Правила pilot-схемы:
//   - `host_ca_ref` (singular, deprecated S7-3) И `host_ca_refs[]` (multi-CA)
//     mutually exclusive — одновременное присутствие отвергается
//     `mutually_exclusive_keys`. Singular остаётся под 1-release WARN
//     deprecation window (auto-adapt в singleton делает daemon).
//   - `host_ca_ref` (если задан один singular) обязан быть vault-ref-ом
//     (`vault:<mount>/<path>`). Plaintext-inline-PEM отвергнут как нарушение
//     security policy (симметрия с `sigil.signing_key_ref` /
//     `auth.jwt.signing_key_ref`).
//   - `host_ca_refs[]` — каждый элемент должен иметь vault-ref `ref` и
//     уникальный kebab-case `name` (используется как label-значение в
//     `keeper_push_host_ca_used_total{ca_name=...}` и в логах).
//   - `targets[]` — кажный элемент должен иметь непустой `sid`; дубликаты sid
//     запрещены (ConfigTargetResolver индексирует по SID).
//   - `ssh_port` (если задан) — в диапазоне 1..65535. 0/опущено → дефолт 22
//     резолвится в push.ConfigTargetResolver.
//   - `providers[]` — каждый элемент должен иметь непустой `name`;
//     дубликаты `name` запрещены (env-payload-injection lookup по имени).
func validatePush(root *ast.MappingNode, p *KeeperPush) []diag.Diagnostic {
	var out []diag.Diagnostic
	// Mutually exclusive singular + plural. Auto-adapt singular в singleton
	// делает daemon, schema-фаза только отвергает двусмысленность.
	if p.HostCARef != "" && len(p.HostCARefs) > 0 {
		out = append(out, atPath(root, "$.push.host_ca_refs", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "mutually_exclusive_keys",
			Message: "push.host_ca_ref (deprecated singular) and push.host_ca_refs[] are mutually exclusive; set exactly one",
			Hint:    "remove deprecated push.host_ca_ref and keep push.host_ca_refs[]; singular auto-adapts only when refs[] is empty",
		}))
	}
	if p.HostCARef != "" && !isVaultRef(p.HostCARef) {
		out = append(out, atPath(root, "$.push.host_ca_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_invalid",
			Message: fmt.Sprintf("push.host_ca_ref must be a vault-ref (vault:<mount>/<path>), got %q", p.HostCARef),
			Hint:    "host-CA public key lives in Vault; use e.g. vault:secret/keeper/ssh-host-ca",
		}))
	}
	out = append(out, validatePushHostCARefs(root, p.HostCARefs)...)
	seenSIDs := make(map[string]int, len(p.Targets))
	for i, t := range p.Targets {
		yp := fmt.Sprintf("$.push.targets[%d]", i)
		if t.SID == "" {
			out = append(out, atPath(root, yp+".sid", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: fmt.Sprintf("push.targets[%d].sid is required", i),
				Hint:    "sid must match the souls.sid (= FQDN) of the push host",
			}))
		} else if prev, dup := seenSIDs[t.SID]; dup {
			out = append(out, atPath(root, yp+".sid", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "duplicate_push_target_sid",
				Message: fmt.Sprintf("push.targets[%d].sid duplicates push.targets[%d].sid (%q)", i, prev, t.SID),
			}))
		} else {
			seenSIDs[t.SID] = i
		}
		if t.SSHPort != 0 {
			out = append(out, checkPort(root, yp+".ssh_port", t.SSHPort)...)
		}
	}
	seenProviders := make(map[string]int, len(p.Providers))
	for i, pr := range p.Providers {
		yp := fmt.Sprintf("$.push.providers[%d]", i)
		if pr.Name == "" {
			out = append(out, atPath(root, yp+".name", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: fmt.Sprintf("push.providers[%d].name is required", i),
				Hint:    "name must match plugins.ssh_providers[].name (kebab-case)",
			}))
		} else if prev, dup := seenProviders[pr.Name]; dup {
			out = append(out, atPath(root, yp+".name", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "duplicate_push_provider_name",
				Message: fmt.Sprintf("push.providers[%d].name duplicates push.providers[%d].name (%q)", i, prev, pr.Name),
			}))
		} else {
			seenProviders[pr.Name] = i
		}
	}
	return out
}

// rePushCAName — kebab-case имя CA в `push.host_ca_refs[].name` (S7-3).
// Dash только между алфанумериков, без trailing/leading/double-dash. Используется
// как label-значение в `keeper_push_host_ca_used_total{ca_name=...}` —
// cardinality-safe форма (короткий closed-set имён, заданных оператором).
var rePushCAName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// validatePushHostCARefs проверяет multi-CA-список `push.host_ca_refs[]` (S7-3,
// ADR-032 amendment 2026-05-26):
//
//   - каждый `ref` — непустой vault-ref (`vault:<mount>/<path>`); plaintext-PEM
//     запрещён, симметрия с singular `host_ca_ref`;
//   - каждый `name` — непустой kebab-case (используется как label-значение в
//     метриках, поэтому форма закрепляется в схеме);
//   - имена в наборе уникальны (lookup по имени в логах/метриках без
//     двусмысленности).
func validatePushHostCARefs(root *ast.MappingNode, refs []KeeperPushCARef) []diag.Diagnostic {
	if len(refs) == 0 {
		return nil
	}
	var out []diag.Diagnostic
	seenNames := make(map[string]int, len(refs))
	for i, r := range refs {
		yp := fmt.Sprintf("$.push.host_ca_refs[%d]", i)
		if r.Ref == "" {
			out = append(out, atPath(root, yp+".ref", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: fmt.Sprintf("push.host_ca_refs[%d].ref is required", i),
				Hint:    "use a vault-ref, e.g. vault:secret/keeper/ssh-host-ca",
			}))
		} else if !isVaultRef(r.Ref) {
			out = append(out, atPath(root, yp+".ref", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "vault_ref_invalid",
				Message: fmt.Sprintf("push.host_ca_refs[%d].ref must be a vault-ref (vault:<mount>/<path>), got %q", i, r.Ref),
				Hint:    "host-CA public key lives in Vault; use e.g. vault:secret/keeper/ssh-host-ca-prod",
			}))
		}
		switch {
		case r.Name == "":
			out = append(out, atPath(root, yp+".name", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: fmt.Sprintf("push.host_ca_refs[%d].name is required", i),
				Hint:    "kebab-case operator-defined name, used as label in keeper_push_host_ca_used_total{ca_name=...}",
			}))
		case !rePushCAName.MatchString(r.Name):
			out = append(out, atPath(root, yp+".name", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "name_invalid_format",
				Message: fmt.Sprintf("push.host_ca_refs[%d].name = %q does not match kebab-case", i, r.Name),
				Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter",
			}))
		default:
			if prev, dup := seenNames[r.Name]; dup {
				out = append(out, atPath(root, yp+".name", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "duplicate_push_host_ca_name",
					Message: fmt.Sprintf("push.host_ca_refs[%d].name duplicates push.host_ca_refs[%d].name (%q)", i, prev, r.Name),
				}))
			} else {
				seenNames[r.Name] = i
			}
		}
	}
	return out
}

// validateSigil проверяет блок `sigil` (ADR-026, печать доверия Sigil).
//
// Блок optional: при отсутствии подпись плагинов недоступна (allow-операция S4
// вернёт ошибку), keeper стартует нормально. Если signing_key_ref задан — он
// обязан быть vault-ref-ом (`vault:<mount>/<path>`): приватник подписи живёт в
// Vault, plaintext-ключ в конфиге запрещён («безопасность на первом месте»),
// симметрично auth.jwt.signing_key_ref / metrics.auth.basic.password_ref.
func validateSigil(root *ast.MappingNode, s *KeeperSigil) []diag.Diagnostic {
	var out []diag.Diagnostic
	if s.SigningKeyRef != "" && !isVaultRef(s.SigningKeyRef) {
		out = append(out, atPath(root, "$.sigil.signing_key_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_invalid",
			Message: fmt.Sprintf("sigil.signing_key_ref must be a vault-ref (vault:<mount>/<path>), got %q", s.SigningKeyRef),
			Hint:    "the sigil signing key lives in Vault; use e.g. vault:secret/keeper/sigil-signing-key",
		}))
	}
	return out
}

// validateVaultAuth проверяет блок `vault.auth` (ADR-014, AppRole).
//
// vaultPresent — присутствует ли top-level `vault`-ключ в YAML; при его
// отсутствии блок не валидируется (top-level required-проверка уже выдала
// missing_required_field на сам `vault`, дублировать не нужно).
//
// Правила:
//   - `method` (если задан) — из enumVaultAuth (token | approle); пусто = token.
//   - method=token: approle-поля игнорируются (если заданы — warn про
//     неиспользуемые поля, не error: чтобы поймать опечатку в method).
//   - method=approle: `role_id` обязателен; ровно один источник secret_id —
//     `secret_id_file` ИЛИ `secret_id_env` (ни ноль, ни оба); `secret_id_file`
//     обязан быть абсолютным путём; `token` при approle задавать нельзя
//     (взаимоисключение источников аутентификации).
//
// role_id — не секрет, plaintext в конфиге допустим. secret_id plaintext в
// конфиге не предусмотрен схемой (нет такого поля) — только file/env.
func validateVaultAuth(root *ast.MappingNode, v *KeeperVault, vaultPresent bool) []diag.Diagnostic {
	if !vaultPresent {
		return nil
	}
	var out []diag.Diagnostic
	a := &v.Auth

	if a.Method != "" {
		out = append(out, checkEnum(root, "$.vault.auth.method", a.Method, enumVaultAuth)...)
		// Невалидный method → дальнейшие per-method проверки бессмысленны.
		if !contains(enumVaultAuth, a.Method) {
			return out
		}
	}

	switch a.ResolvedAuthMethod() {
	case AuthMethodToken:
		// approle-поля при token-методе не используются — мягкий сигнал.
		if a.RoleID != "" || a.SecretIDFile != "" || a.SecretIDEnv != "" {
			out = append(out, atPath(root, "$.vault.auth", diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:    "vault_auth_unused_fields",
				Message: "vault.auth.role_id/secret_id_file/secret_id_env are ignored when method=token",
				Hint:    "set vault.auth.method: approle to use them, or remove the fields",
			}))
		}

	case AuthMethodAppRole:
		if a.RoleID == "" {
			out = append(out, atPath(root, "$.vault.auth.role_id", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: "vault.auth.role_id is required when method=approle",
				Hint:    "role_id is not a secret; set it inline, see docs/keeper/config.md → vault.auth",
			}))
		}
		hasFile := a.SecretIDFile != ""
		hasEnv := a.SecretIDEnv != ""
		switch {
		case !hasFile && !hasEnv:
			out = append(out, atPath(root, "$.vault.auth", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: "vault.auth requires a secret_id source when method=approle: set secret_id_file or secret_id_env",
				Hint:    "secret_id is a secret; use secret_id_file (mode-restricted file) or secret_id_env",
			}))
		case hasFile && hasEnv:
			out = append(out, atPath(root, "$.vault.auth", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "vault_auth_conflicting_secret_source",
				Message: "vault.auth.secret_id_file and secret_id_env are mutually exclusive; set exactly one",
			}))
		}
		if hasFile && !filepath.IsAbs(a.SecretIDFile) {
			out = append(out, atPath(root, "$.vault.auth.secret_id_file", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "path_not_absolute",
				Message: fmt.Sprintf("vault.auth.secret_id_file must be an absolute path, got %q", a.SecretIDFile),
				Hint:    "use e.g. /etc/keeper/vault-secret-id",
			}))
		}
		if v.Token != "" {
			out = append(out, atPath(root, "$.vault.token", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "vault_auth_conflicting_method",
				Message: "vault.token must not be set when vault.auth.method=approle",
				Hint:    "approle obtains its own client-token; remove vault.token",
			}))
		}
	}
	return out
}

// isVaultRef — форма `vault:<mount>/<path>` (минимально: префикс `vault:`,
// непустое тело, разделитель `/` между mount и path не на краях). Зеркалит
// keeper/internal/vault.ParseRef, но без import-а keeper-пакета в shared/
// (ADR-011). Полный резолв ref-а делает keeper-vault на runtime.
func isVaultRef(ref string) bool {
	const prefix = "vault:"
	if len(ref) <= len(prefix) || ref[:len(prefix)] != prefix {
		return false
	}
	body := ref[len(prefix):]
	if len(body) > 0 && body[0] == '/' {
		body = body[1:]
	}
	slash := -1
	for i := 0; i < len(body); i++ {
		if body[i] == '/' {
			slash = i
			break
		}
	}
	return slash > 0 && slash != len(body)-1
}

func checkEnum(root *ast.MappingNode, yamlPath, val string, allowed []string) []diag.Diagnostic {
	if contains(allowed, val) {
		return nil
	}
	return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:    "enum_invalid",
		Message: fmt.Sprintf("%q is not in allowed set %v", val, allowed),
	})}
}

func checkHostPort(root *ast.MappingNode, yamlPath, val string) []diag.Diagnostic {
	if val == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(val)
	if err != nil {
		return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "host_port_invalid",
			Message: fmt.Sprintf("invalid host:port %q: %v", val, err),
		})}
	}
	_ = host
	p, err := strconv.Atoi(port)
	if err != nil {
		return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "host_port_invalid",
			Message: fmt.Sprintf("invalid port in %q: %v", val, err),
		})}
	}
	if p < 1 || p > 65535 {
		return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "port_out_of_range",
			Message: fmt.Sprintf("port %d out of range 1..65535", p),
		})}
	}
	return nil
}

// checkPort валидирует целочисленный порт в диапазоне 1..65535. Порт 0
// трактуется как «не задан» вызывающим (отдельный *_required diag), сюда
// доходят только ненулевые значения.
func checkPort(root *ast.MappingNode, yamlPath string, port int) []diag.Diagnostic {
	if port >= 1 && port <= 65535 {
		return nil
	}
	return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:    "port_out_of_range",
		Message: fmt.Sprintf("port %d out of range 1..65535", port),
	})}
}

// atPath обогащает диагностику line/column через резолв yaml-пути в AST.
//
// Если путь не находится — возвращается diag как есть (без позиции).
func atPath(root *ast.MappingNode, yamlPath string, d diag.Diagnostic) diag.Diagnostic {
	d.YAMLPath = yamlPath
	if line, col, ok := lookupPath(root, yamlPath); ok {
		d.Line = line
		d.Column = col
	}
	return d
}

// topLevelKeys возвращает множество имён ключей верхнего уровня в mapping.
// Используется для проверки присутствия обязательных блоков: zero-value
// в Go-структуре не отличает «ключ отсутствует» от «ключ есть с пустым
// телом», а для `missing_required_field` нам важен именно факт присутствия.
func topLevelKeys(m *ast.MappingNode) map[string]bool {
	out := make(map[string]bool, len(m.Values))
	for _, kv := range m.Values {
		if t := kv.Key.GetToken(); t != nil {
			out[t.Value] = true
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// pathWithin — true, если sub лежит внутри (или равен) base. Оба ожидаются
// абсолютными (вызывается после filepath.IsAbs-проверки). Сравнение по
// очищенным путям с граничным разделителем, чтобы `/a/bc` не считался внутри
// `/a/b`.
func pathWithin(sub, base string) bool {
	b := filepath.Clean(base)
	s := filepath.Clean(sub)
	if s == b {
		return true
	}
	return strings.HasPrefix(s, b+string(filepath.Separator))
}
