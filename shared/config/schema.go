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
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// Closed enums, normalized per docs/keeper/config.md and docs/soul/config.md.
var (
	enumLogLevel       = []string{"debug", "info", "warn", "error"}
	enumLogFormat      = []string{"json", "text"}
	enumOTelExporter   = []string{"otlp"}
	enumVaultAuth      = []string{"token", "approle"}
	enumRedisMode      = []string{"standalone", "sentinel", "cluster"}
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

// schemaValidate dispatches by type. Accepts a populated *KeeperConfig,
// *SoulConfig, or *DestinyManifest.
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
	case *ScenarioFragment:
		return schemaValidateCovenant(path, root, c)
	}
	return nil
}

func schemaValidateKeeper(path string, root *ast.MappingNode, c *KeeperConfig) []diag.Diagnostic {
	var out []diag.Diagnostic

	// Top-level required blocks (docs/keeper/config.md — `default: —` without an
	// `optional` marker). KID is a scalar, the rest are mapping blocks; the check
	// is by key presence in the AST, to tell "absent" from "zero-value".
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

	// listen: must be present and non-empty in content. If the key exists but
	// all listeners are empty, that's the same error as "listen absent" per the
	// docs/keeper/config.md contract.
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
		// MCP listener is mandatory per the cross-cutting "built-in MCP"
		// requirement (requirements.md → Architectural requirements). Disabling
		// is forbidden by the grammar (docs/keeper/config.md → listen.mcp.addr,
		// no `optional`).
		if c.Listen.MCP.Addr == "" {
			out = append(out, atPath(root, "$.listen.mcp.addr", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "mcp_listener_required",
				Message: "listen.mcp.addr is required",
				Hint:    "MCP listener is mandatory per requirements.md -> 'built-in MCP'; see docs/keeper/config.md -> listen.mcp",
			}))
		}
		// Both gRPC sub-listeners are mandatory per ADR-002 / ADR-012: Soul gRPC =
		// mandatory transport; without both listeners the keeper can neither
		// accept onboarding nor bring up EventStream.
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
		// Port conflict: Bootstrap and EventStream must listen on different
		// addresses (different TLS modes — server-only vs mTLS).
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
	out = append(out, validateRedis(root, &c.Redis)...)

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

	// event_stream ApplyRequest send-limit. 0 = "unset" (default
	// DefaultMaxApplySizeMB); a set value must be ≥ MinMaxApplySizeMB.
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

	// plugins.work_root — root of the resolver's working git clones (ADR-026
	// F-fetch). Absolute path, same pattern as cache_root. STRICTLY outside
	// cache_root: if both are set and work_root is inside cache_root, .git/
	// checkout would land in the cache read by Discover/ReadSlot (breaks the
	// layout invariant).
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

	// plugins.max_artifact_size_mb / max_clone_size_mb — git-egress hardening
	// size limits (ADR-026(g)). 0 = "unset" (default in Resolved*); a set value
	// must be ≥ MinPluginSizeMB — a sub-megabyte ceiling would reject any real
	// plugin Go binary, turning hardening into a permanent fail-closed.
	if c.Plugins != nil {
		out = append(out, validatePluginSizeMB(root,
			"$.plugins.max_artifact_size_mb", c.Plugins.MaxArtifactSizeMB)...)
		out = append(out, validatePluginSizeMB(root,
			"$.plugins.max_clone_size_mb", c.Plugins.MaxCloneSizeMB)...)
		out = append(out, validatePluginCatalog(root, "$.plugins.ssh_providers", c.Plugins.SSHProviders)...)
		out = append(out, validatePluginCatalog(root, "$.plugins.soul_modules", c.Plugins.SoulModules)...)
	}

	if c.Audit != nil && c.Audit.RetentionDays != 0 && c.Audit.RetentionDays < 1 {
		out = append(out, atPath(root, "$.audit.retention_days", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("audit.retention_days must be >= 1, got %d", c.Audit.RetentionDays),
		}))
	}

	// reaper.batch_size: 0 = "unset" (runner default applies). Any set value must
	// be >= 1: an explicit zero or a negative is a meaningless chunk size that
	// the runner would silently treat as the default or loop the query on.
	if c.Reaper != nil && c.Reaper.BatchSize < 0 {
		out = append(out, atPath(root, "$.reaper.batch_size", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("reaper.batch_size must be >= 1, got %d", c.Reaper.BatchSize),
		}))
	}

	// keeper.acolytes (ADR-027): feature-flag for the execution pool's worker
	// count. 0 = "unset" → the pool is not started (the prior run-goroutine
	// path). Negative is a meaningless pool size.
	if c.Acolytes < 0 {
		out = append(out, atPath(root, "$.acolytes", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("acolytes must be >= 0, got %d", c.Acolytes),
		}))
	}

	// keeper.acolyte_batch (ADR-027(d)): batch size of one claim tick. 0 = "unset"
	// → default DefaultAcolyteBatch. Negative is a meaningless LIMIT (symmetric
	// with reaper.batch_size).
	if c.AcolyteBatch < 0 {
		out = append(out, atPath(root, "$.acolyte_batch", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("acolyte_batch must be >= 0, got %d", c.AcolyteBatch),
		}))
	}

	// keeper.oracle_circuit_max_fires (ADR-030(a), beacons S4): auto-disable
	// threshold for the Decree. nil (omitted) → default
	// DefaultOracleCircuitMaxFires in the daemon; explicit 0 = breaker OFF
	// (escape-hatch); negative is a meaningless threshold.
	if c.OracleCircuitMaxFires != nil && *c.OracleCircuitMaxFires < 0 {
		out = append(out, atPath(root, "$.oracle_circuit_max_fires", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("oracle_circuit_max_fires must be >= 0, got %d", *c.OracleCircuitMaxFires),
		}))
	}

	// keeper.watchman_fail_threshold (soul-shedding S2): number of consecutive
	// probe failures before shedding. 0 = "unset" → default
	// DefaultWatchmanFailThreshold. Negative is a meaningless threshold
	// (symmetric with acolyte_batch).
	if c.WatchmanFailThreshold < 0 {
		out = append(out, atPath(root, "$.watchman_fail_threshold", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("watchman_fail_threshold must be >= 0, got %d", c.WatchmanFailThreshold),
		}))
	}

	// keeper.toll.threshold (Toll cluster detector, ADR-038): fraction of the
	// `souls.status='connected'` baseline. 0 (unset) → default
	// DefaultTollThreshold; an explicit 0 is treated as "unset" (a zero threshold
	// is meaningless — it would fire on the first disconnect). Negative and > 1
	// are out of range.
	if c.Toll != nil {
		if c.Toll.Threshold < 0 || c.Toll.Threshold > 1 {
			out = append(out, atPath(root, "$.toll.threshold", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("toll.threshold must be in (0, 1], got %v", c.Toll.Threshold),
			}))
		}

		// keeper.toll.per_coven_thresholds[*]: symmetric with the top-level
		// threshold — range (0, 1]; an empty coven key is rejected (the
		// sorted-set member value carries coven=" "; an empty key === global,
		// which the top-level threshold already covers).
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

		// keeper.toll.webhook (ADR-038 amendment, extensions): enabled → url_ref +
		// a valid format (closed enum) are required. When enabled=false (or nil)
		// there are no validations (the notifier won't start, at the daemon gate).
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
	// (Tempo rate-limiter, ADR-050 + amendment 2026-06-17 — a separate preview
	// bucket): rate (rps, refill rate) and burst (bucket depth, capacity), when
	// set EXPLICITLY, cannot be negative (refill rate / capacity ≥ 0); 0 or an
	// omitted field resolves to the default in Resolved*, so the validator only
	// rejects < 0 (0 passes and is replaced by the default). enabled (any bool)
	// is valid; a zero block → defaults.
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

	// Top-level required: a `keeper:` block with a non-empty `endpoints[]`.
	// docs/soul/config.md → keeper.endpoints is mandatory.
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
		// host is required (shared by both phases; docs/soul/config.md →
		// keeper.endpoints[].host).
		if ep.Host == "" {
			out = append(out, atPath(root, fmt.Sprintf("$.keeper.endpoints[%d].host", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: fmt.Sprintf("keeper.endpoints[%d].host is required", i),
				Hint:    "Keeper-cluster host shared by both bootstrap and event-stream phases; see docs/soul/config.md → keeper.endpoints",
			}))
		}
		// Both ports are mandatory by architect decision (ADR-012 — two
		// listeners; "security first": explicitness > brevity, no silent fallback
		// of bootstrap onto the event_stream port).
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
		// priority: 0 = "unset" (normalizedPriority maps 0→1, default). Only a
		// negative is rejected; see docs/soul/connection.md.
		if ep.Priority < 0 {
			out = append(out, atPath(root, fmt.Sprintf("$.keeper.endpoints[%d].priority", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("keeper.endpoints[%d].priority must not be negative (0 = not set -> default 1), got %d", i, ep.Priority),
			}))
		}
	}
	// keeper.retry.max_attempts: 0 = "unset" (resolves to the connection runner's
	// default). Only a negative is rejected — an impossible attempt count; it
	// would silently read as the default or as "never connect".
	if c.Keeper.Retry != nil && c.Keeper.Retry.MaxAttempts < 0 {
		out = append(out, atPath(root, "$.keeper.retry.max_attempts", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("keeper.retry.max_attempts must not be negative (0 = not set -> default), got %d", c.Keeper.Retry.MaxAttempts),
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
	// keeper.max_apply_size_mb — recv-limit for an incoming ApplyRequest. 0 =
	// "unset" (default DefaultMaxApplySizeMB); a set value must be ≥
	// MinMaxApplySizeMB.
	out = append(out, validateMaxApplySize(root,
		"$.keeper.max_apply_size_mb", c.Keeper.MaxApplySizeMB)...)

	out = append(out, validateLogging(root, "$.logging", c.Logging.Level, c.Logging.Format, c.Logging.File, c.Logging.Rotation)...)
	if c.PluginRuntime != nil {
		out = append(out, validatePluginRuntime(root, "$.plugin_runtime", c.PluginRuntime)...)
	}
	return out
}

// validateMaxApplySize is the shared range check for the ApplyRequest limit on
// both sides (Keeper-send / Soul-recv). 0 = "unset" (default
// DefaultMaxApplySizeMB applies in Resolved*); any set value must be ≥
// MinMaxApplySizeMB — otherwise a real Destiny would hit fail-fast (Keeper) /
// rejection (Soul) at a sub-megabyte ceiling.
func validateMaxApplySize(root *ast.MappingNode, yamlPath string, mb int) []diag.Diagnostic {
	if mb == 0 {
		return nil
	}
	if mb < MinMaxApplySizeMB {
		return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("%s must be >= %d (MiB), got %d", yamlPath, MinMaxApplySizeMB, mb),
			Hint:    "Keeper-send-limit must be <= Soul-recv-limit; default for both is 8 MiB",
		})}
	}
	return nil
}

// validatePluginSizeMB is the range check for plugin-resolve size limits
// (`plugins.max_artifact_size_mb` / `max_clone_size_mb`, ADR-026(g)). 0 = "unset"
// (default applies in the Resolved* methods); any set value must be ≥
// MinPluginSizeMB.
func validatePluginSizeMB(root *ast.MappingNode, yamlPath string, mb int) []diag.Diagnostic {
	if mb == 0 {
		return nil
	}
	if mb < MinPluginSizeMB {
		return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "value_out_of_range",
			Message: fmt.Sprintf("%s must be >= %d (MiB), got %d", yamlPath, MinPluginSizeMB, mb),
			Hint:    "a sub-megabyte ceiling would reject any real plugin binary; defaults are 256 MiB (artifact) / 1024 MiB (clone)",
		})}
	}
	return nil
}

// reCatalogArtifactSHA256 is the digest form a catalog artifact row carries: the same
// 64 lowercase hex the grant, the DB CHECK and the signed block all use.
var reCatalogArtifactSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// rePlatformToken mirrors the shape [pluginhost.CanonicalArtifacts] enforces before a
// signature can exist over a row. Repeated here so the operator is told at the yaml
// path rather than by a resolve-time refusal with no line number.
var rePlatformToken = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// validatePluginCatalog checks one `plugins.<list>` against the shape its `kind:`
// picks (NIM-793). Every finding is an ERROR: this catalog decides which executable
// bytes a Keeper will fetch and sign for, so a malformed entry must stop the config
// rather than be resolved into a warning and skipped at runtime.
//
// The fields of the OTHER kind are refused rather than ignored. A `base_url` under a
// git entry, or a `source` under an artifact entry, is an operator believing the
// Keeper reads something it does not — and for a URL that belief is about where
// executable code comes from.
//
// Duplicate `(os, arch)` rows are refused here and again in
// [pluginhost.CanonicalArtifacts] before signing. Not redundant: this one gives the
// operator the yaml path, and that one makes it impossible for a signature to exist
// over an ambiguous list however the rows got there.
func validatePluginCatalog(root *ast.MappingNode, prefix string, entries []PluginCatalogEntry) []diag.Diagnostic {
	var out []diag.Diagnostic
	for i, e := range entries {
		at := fmt.Sprintf("%s[%d]", prefix, i)

		if e.Kind != "" && !plugin.ValidSourceKind(e.Kind) {
			out = append(out, atPath(root, at+".kind", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "enum_invalid",
				Message: fmt.Sprintf("%q is not in allowed set %v", e.Kind, plugin.SourceKinds()),
				Hint:    "omit `kind:` for the git default",
			}))
			continue
		}

		switch e.ResolvedKind() {
		case plugin.SourceKindGit:
			out = append(out, validateGitCatalogEntry(root, at, e)...)
		case plugin.SourceKindArtifact:
			out = append(out, validateArtifactCatalogEntry(root, at, e)...)
		}
	}
	return out
}

// validateGitCatalogEntry checks the historical shape: `source` + `ref`, and nothing
// belonging to the artifact kind.
func validateGitCatalogEntry(root *ast.MappingNode, at string, e PluginCatalogEntry) []diag.Diagnostic {
	var out []diag.Diagnostic
	if e.Source == "" {
		out = append(out, atPath(root, at+".source", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "field_required",
			Message: "a git plugin entry requires `source` (the module repository remote)",
		}))
	}
	if e.BaseURL != "" {
		out = append(out, atPath(root, at+".base_url", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "field_not_allowed",
			Message: "`base_url` belongs to `kind: artifact`; a git entry is resolved from `source`",
		}))
	}
	if len(e.Artifacts) > 0 {
		out = append(out, atPath(root, at+".artifacts", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "field_not_allowed",
			Message: "`artifacts` belongs to `kind: artifact`; a git entry takes the single executable in dist/",
		}))
	}
	return out
}

// validateArtifactCatalogEntry checks the published-release shape: `base_url` + a
// non-empty `artifacts` list, each row a complete (os, arch, path, sha256).
func validateArtifactCatalogEntry(root *ast.MappingNode, at string, e PluginCatalogEntry) []diag.Diagnostic {
	var out []diag.Diagnostic
	if e.BaseURL == "" {
		out = append(out, atPath(root, at+".base_url", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "field_required",
			Message: "an artifact plugin entry requires `base_url` (the publication root the files hang off)",
		}))
	}
	if e.Source != "" {
		out = append(out, atPath(root, at+".source", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "field_not_allowed",
			Message: "`source` belongs to `kind: git`; an artifact entry publishes under `base_url`",
		}))
	}
	if len(e.Artifacts) == 0 {
		out = append(out, atPath(root, at+".artifacts", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "field_required",
			Message: "an artifact plugin entry requires at least one `artifacts` row",
			Hint:    "there is no path template: every published file is listed with its own sha256",
		}))
		return out
	}

	seen := make(map[string]int, len(e.Artifacts))
	for j, a := range e.Artifacts {
		aat := fmt.Sprintf("%s.artifacts[%d]", at, j)
		// Fixed order, not a map: diagnostics are compared byte for byte by the
		// config tests and read in order by an operator.
		for _, f := range []struct{ name, val string }{
			{"os", a.OS}, {"arch", a.Arch}, {"path", a.Path}, {"sha256", a.SHA256},
		} {
			field, val := f.name, f.val
			if val == "" {
				out = append(out, atPath(root, aat+"."+field, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "field_required",
					Message: fmt.Sprintf("artifact row requires `%s`", field),
				}))
			}
		}
		if a.SHA256 != "" && !reCatalogArtifactSHA256.MatchString(a.SHA256) {
			out = append(out, atPath(root, aat+".sha256", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_invalid",
				Message: fmt.Sprintf("sha256 %q must be 64 lower-hex chars", a.SHA256),
			}))
		}
		if a.Path != "" && !ValidPluginArtifactPath(a.Path) {
			out = append(out, atPath(root, aat+".path", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_invalid",
				Message: fmt.Sprintf("path %q must name a file under base_url: no leading '/', no '.' or '..' segment, no scheme, no '?', '#', '%%' or '\\'", a.Path),
				Hint:    "the path is signed AND concatenated onto base_url — anything a URL parser reads differently would sign one address and fetch another",
			}))
		}
		for _, f := range []struct{ name, val string }{{"os", a.OS}, {"arch", a.Arch}} {
			if f.val != "" && !rePlatformToken.MatchString(f.val) {
				out = append(out, atPath(root, aat+"."+f.name, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "value_invalid",
					Message: fmt.Sprintf("%s %q must be a lower-alphanumeric GOOS/GOARCH token", f.name, f.val),
					Hint:    "spell the platform the way Go does: os: linux, arch: amd64",
				}))
			}
		}
		if a.OS != "" && a.Arch != "" {
			key := a.OS + "/" + a.Arch
			if first, dup := seen[key]; dup {
				out = append(out, atPath(root, aat, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "duplicate_entry",
					Message: fmt.Sprintf("platform %s is already declared by artifacts[%d]", key, first),
					Hint:    "one platform has one file; two rows would make the approved bytes depend on list order",
				}))
				continue
			}
			seen[key] = j
		}
	}
	return out
}

// ValidPluginArtifactPath reports whether a catalog `path` names a file UNDER the
// entry's base_url.
//
// The rule is stricter than "no traversal", and the extra strictness is the point: the
// path is SIGNED into the grant and separately CONCATENATED onto the base URL to fetch
// with, so anything a URL parser reads differently from a path reader would make the
// signed "where these bytes came from" a different address than the one fetched.
// `pkg#v2` fetches `…/pkg` and records `pkg#v2`; `a%2e%2e%2fb` records one path and
// resolves to another.
//
// Rejected, therefore:
//   - an empty path, an absolute one (`/x`), any `.` or `..` segment, and anything
//     carrying a scheme or network-path prefix (`https://x`, `//host/x`). `..` reaches
//     an address the operator did not approve; a bare `.` reaches the same file by a
//     different spelling, which is worse than useless here — the Soul-side check
//     refuses it, so a grant Keeper happily signed would be uninstallable on every host
//     while showing as active;
//   - `?` and `#` — a query or fragment is not part of a file name, and both are cut
//     off by the fetcher while staying in the signature;
//   - `%` — percent-encoding means the byte sequence signed and the byte sequence
//     requested can differ. Refusing it outright is honest; decoding it here would put
//     a second URL parser in the trust path.
//
// A backslash is refused for the same reason: some servers normalize it to `/`, which
// would smuggle a segment past the `..` check.
//
// Exported because the rule has two enforcement points and must not have two
// definitions: the schema phase, which can name the yaml path in the message, and the
// resolver, which must refuse the same thing when it is handed a catalog that never
// went through the schema phase.
func ValidPluginArtifactPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "://") {
		return false
	}
	if strings.ContainsAny(p, "?#%\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func validateLogging(root *ast.MappingNode, prefix, level, format, file string, rotation *LoggingRotation) []diag.Diagnostic {
	var out []diag.Diagnostic
	if level != "" {
		out = append(out, checkEnum(root, prefix+".level", level, enumLogLevel)...)
	}
	if format != "" {
		out = append(out, checkEnum(root, prefix+".format", format, enumLogFormat)...)
	}
	// `file` — path to the log file; empty = stderr. If set, must be absolute
	// (following plugins.cache_root → path_not_absolute).
	if file != "" && !filepath.IsAbs(file) {
		out = append(out, atPath(root, prefix+".file", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "path_not_absolute",
			Message: fmt.Sprintf("%s.file must be an absolute path, got %q", prefix, file),
			Hint:    "use e.g. /var/log/soul/soul.log",
		}))
	}
	if rotation != nil {
		// 0 = "unset" (the builder default in shared/log applies) for all three
		// numeric rotation fields; a negative is an impossible size/count that
		// would otherwise go to lumberjack as garbage.
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

// validateMetricsBasicAuth checks the `metrics.auth.basic` block (ADR-024): with
// `enabled: true`, `username` and `password_ref` are required, and password_ref
// must be a vault-ref (`vault:<mount>/<path>`) — a plaintext password in the
// config is forbidden ("security first").
//
// With `enabled: false` the fields aren't required (a stub with empty values is
// fine), but if password_ref is set it is still validated as a vault-ref, to
// catch a typo before it's enabled.
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
			Message: vaultRefMessage("metrics.auth.basic.password_ref", vaultRefFormMount),
			Hint:    "plaintext passwords are not allowed; use vault:secret/keeper/metrics-password",
		}))
	}
	return out
}

// validateSoulMetricsBasicAuth checks the soul `metrics.basic_auth` block
// (ADR-024): with `enabled: true`, `username` and `password_file` are required.
//
// Mirror of keeper `metrics.auth.basic`, but the password source is a file path,
// not a vault-ref (Soul has no vault client, ADR-012). File existence/readability
// is NOT checked here (a runtime boundary: the file may appear between validation
// and start, and checking in the schema = false positives in offline
// `soul-lint`); fail-fast on absence is in main when resolving credentials.
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

// validatePush checks the `push` block (pilot wire-up of SshDispatcher, ADR-032
// amendment 2026-05-26). The whole block is optional: empty/absent → the push
// orchestrator is not started (see setupPushDispatchers).
//
// Pilot-schema rules:
//   - `host_ca_ref` (singular, deprecated S7-3) AND `host_ca_refs[]` (multi-CA)
//     are mutually exclusive — both present is rejected with
//     `mutually_exclusive_keys`. The singular stays under a 1-release WARN
//     deprecation window (the daemon auto-adapts it into the singleton).
//   - `host_ca_ref` (if the single singular is set) must be a vault-ref
//     (`vault:<mount>/<path>`). Plaintext inline PEM is rejected as a security
//     policy violation (symmetric with `sigil.signing_key_ref` /
//     `auth.jwt.signing_key_ref`).
//   - `host_ca_refs[]` — each element must have a vault-ref `ref` and a unique
//     kebab-case `name` (used as a label value in
//     `keeper_push_host_ca_used_total{ca_name=...}` and in logs).
//   - `targets[]` — each element must have a non-empty `sid`; duplicate sids are
//     forbidden (ConfigTargetResolver indexes by SID).
//   - `ssh_port` (if set) — in range 1..65535. 0/omitted → default 22 resolved
//     in push.ConfigTargetResolver.
//   - `providers[]` — each element must have a non-empty `name`; duplicate
//     `name`s are forbidden (env-payload-injection lookup by name).
func validatePush(root *ast.MappingNode, p *KeeperPush) []diag.Diagnostic {
	var out []diag.Diagnostic
	// Mutually exclusive singular + plural. The daemon auto-adapts the singular
	// into the singleton; the schema phase only rejects the ambiguity.
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
			Message: vaultRefMessage("push.host_ca_ref", vaultRefFormMount),
			Hint:    "host-CA public key lives in Vault; use e.g. vault:secret/keeper/ssh-host-ca",
		}))
	}
	out = append(out, validatePushHostCARefs(root, p.HostCARefs)...)
	out = append(out, validatePushTransport(root, p)...)
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

// validatePushTransport checks `push.transport` + the `push.teleport` block
// (ADR-063 amendment "Teleport by-name transport"):
//
//   - `transport` (if set) — one of `direct` / `teleport`; empty = `direct`.
//   - with `transport: teleport` the `teleport` block is required and all three
//     fields (`proxy_addr` / `identity_file` / `cluster`) are non-empty —
//     transport+auth+host-verify all go through the identity file, without which
//     no connection is possible.
//   - `teleport.*` with `transport != teleport` is not an error (creds can be
//     kept ahead of time) but is unused.
//
// `teleport.use_system_trust` / `teleport.alpn_upgrade` — optional bools
// (default false), no validation needed (proxy behind an L7 LB, ADR-063
// amendment).
func validatePushTransport(root *ast.MappingNode, p *KeeperPush) []diag.Diagnostic {
	var out []diag.Diagnostic
	switch p.Transport {
	case "", PushTransportDirect, PushTransportTeleport:
		// ok
	default:
		out = append(out, atPath(root, "$.push.transport", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "invalid_enum_value",
			Message: fmt.Sprintf("push.transport must be one of [%s, %s], got %q", PushTransportDirect, PushTransportTeleport, p.Transport),
			Hint:    "omit for default 'direct' (generic SSH by IP); 'teleport' delivers by node-name via Teleport Proxy",
		}))
	}
	if p.Transport != PushTransportTeleport {
		return out
	}
	if p.Teleport == nil {
		out = append(out, atPath(root, "$.push.teleport", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "push.teleport is required when push.transport: teleport",
			Hint:    "set push.teleport.{proxy_addr, identity_file, cluster}",
		}))
		return out
	}
	if p.Teleport.ProxyAddr == "" {
		out = append(out, atPath(root, "$.push.teleport.proxy_addr", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "push.teleport.proxy_addr is required when push.transport: teleport",
			Hint:    "host:port of the Teleport Proxy gRPC listener, e.g. proxy.example.com:443",
		}))
	}
	if p.Teleport.IdentityFile == "" {
		out = append(out, atPath(root, "$.push.teleport.identity_file", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "push.teleport.identity_file is required when push.transport: teleport",
			Hint:    "path to a Teleport identity file (tctl auth sign) with access to target nodes",
		}))
	}
	if p.Teleport.Cluster == "" {
		out = append(out, atPath(root, "$.push.teleport.cluster", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "push.teleport.cluster is required when push.transport: teleport",
			Hint:    "Teleport cluster name in which node-names (SIDs) are resolved",
		}))
	}
	return out
}

// rePushCAName — kebab-case CA name in `push.host_ca_refs[].name` (S7-3). Dash
// only between alphanumerics, no trailing/leading/double dash. Used as a label
// value in `keeper_push_host_ca_used_total{ca_name=...}` — a cardinality-safe
// form (a short closed set of operator-defined names).
var rePushCAName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// validatePushHostCARefs checks the multi-CA list `push.host_ca_refs[]` (S7-3,
// ADR-032 amendment 2026-05-26):
//
//   - each `ref` is a non-empty vault-ref (`vault:<mount>/<path>`); plaintext PEM
//     is forbidden, symmetric with the singular `host_ca_ref`;
//   - each `name` is a non-empty kebab-case (used as a label value in metrics, so
//     the form is enforced in the schema);
//   - names in the set are unique (lookup by name in logs/metrics without
//     ambiguity).
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
				Message: vaultRefMessage(fmt.Sprintf("push.host_ca_refs[%d].ref", i), vaultRefFormMount),
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

// validateSigil checks the `sigil` block (ADR-026, the Sigil seal of trust).
//
// The block is optional: without it plugin signing is unavailable (the S4 allow
// operation returns an error), the keeper starts normally. If signing_key_ref is
// set it must be a vault-ref (`vault:<mount>/<path>`): the signing private key
// lives in Vault, a plaintext key in the config is forbidden ("security first"),
// symmetric with auth.jwt.signing_key_ref / metrics.auth.basic.password_ref.
func validateSigil(root *ast.MappingNode, s *KeeperSigil) []diag.Diagnostic {
	var out []diag.Diagnostic
	if s.SigningKeyRef != "" && !isVaultRef(s.SigningKeyRef) {
		out = append(out, atPath(root, "$.sigil.signing_key_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_invalid",
			Message: vaultRefMessage("sigil.signing_key_ref", vaultRefFormMount),
			Hint:    "the sigil signing key lives in Vault; use e.g. vault:secret/keeper/sigil-signing-key",
		}))
	}
	return out
}

// validateRedis checks the `redis` block (ADR-006 amendment, modes
// standalone/sentinel/cluster). Symmetric with [validateVaultAuth]: enum `mode`
// + per-mode required fields + a diagnostic for stray fields of another mode.
//
// Rules:
//   - `mode` (if set) — from [enumRedisMode]; empty = standalone (forward-compat
//     for configs without `mode`).
//   - standalone: requires `addr` (host:port); `sentinels`/`nodes`/`master_name`
//     are stray for this mode (diagnostic, not fatal for other fields).
//   - sentinel: requires a non-empty `master_name` AND a non-empty `sentinels`
//     (each element host:port); `addr`/`nodes` are stray.
//   - cluster: requires a non-empty `nodes` (each element host:port);
//     `addr`/`master_name`/`sentinels` are stray.
//   - `password_ref`/`sentinel_password_ref` — vault-ref format (like
//     metrics.auth.basic.password_ref); plaintext is invalid here.
//
// host:port validity of addr/elements is resolved via [checkHostPort] (empty is
// skipped — the required check catches absence with its own diagnostic).
func validateRedis(root *ast.MappingNode, r *KeeperRedis) []diag.Diagnostic {
	var out []diag.Diagnostic

	if r.Mode != "" {
		out = append(out, checkEnum(root, "$.redis.mode", r.Mode, enumRedisMode)...)
		// Invalid mode → per-mode checks are meaningless (like vault.auth.method).
		if !contains(enumRedisMode, r.Mode) {
			return out
		}
	}

	mode := r.Mode
	if mode == "" {
		mode = "standalone"
	}

	// The vault-ref format of password_ref/sentinel_password_ref is checked by the
	// semantic phase (`$.redis.password_ref` / `$.redis.sentinel_password_ref`,
	// `#field`-aware reVaultRef) — here only the topology structure.

	switch mode {
	case "standalone":
		if r.Addr == "" {
			out = append(out, atPath(root, "$.redis.addr", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: "redis.addr is required when redis.mode=standalone",
				Hint:    "set redis.addr: host:port, or pick mode sentinel/cluster",
			}))
		}
		out = append(out, checkHostPort(root, "$.redis.addr", r.Addr)...)
		out = append(out, redisUnusedFieldsDiag(root, mode,
			r.MasterName != "", len(r.Sentinels) > 0, len(r.Nodes) > 0)...)

	case "sentinel":
		if r.MasterName == "" {
			out = append(out, atPath(root, "$.redis.master_name", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: "redis.master_name is required when redis.mode=sentinel",
				Hint:    "set redis.master_name to the monitored master group name",
			}))
		}
		if len(r.Sentinels) == 0 {
			out = append(out, atPath(root, "$.redis.sentinels", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: "redis.sentinels is required when redis.mode=sentinel",
				Hint:    "list sentinel node addresses as host:port",
			}))
		}
		for i, s := range r.Sentinels {
			yamlPath := fmt.Sprintf("$.redis.sentinels[%d]", i)
			out = append(out, checkListEntryHostPort(root, yamlPath, s)...)
		}
		out = append(out, redisUnusedFieldsDiag(root, mode,
			false, false, len(r.Nodes) > 0)...)
		if r.Addr != "" {
			out = append(out, atPath(root, "$.redis.addr", diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:    "redis_unused_field",
				Message: "redis.addr is ignored when redis.mode=sentinel (use sentinels)",
			}))
		}

	case "cluster":
		if len(r.Nodes) == 0 {
			out = append(out, atPath(root, "$.redis.nodes", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "missing_required_field",
				Message: "redis.nodes is required when redis.mode=cluster",
				Hint:    "list cluster node addresses as host:port",
			}))
		}
		for i, n := range r.Nodes {
			yamlPath := fmt.Sprintf("$.redis.nodes[%d]", i)
			out = append(out, checkListEntryHostPort(root, yamlPath, n)...)
		}
		out = append(out, redisUnusedFieldsDiag(root, mode,
			r.MasterName != "", len(r.Sentinels) > 0, false)...)
		if r.Addr != "" {
			out = append(out, atPath(root, "$.redis.addr", diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:    "redis_unused_field",
				Message: "redis.addr is ignored when redis.mode=cluster (use nodes)",
			}))
		}
	}

	return out
}

// redisUnusedFieldsDiag emits a warn diagnostic for fields of another mode. Each
// flag = "field set but not relevant to the current mode". Not fatal: helps catch
// a typo in `mode` / a leftover field from a prior topology.
func redisUnusedFieldsDiag(root *ast.MappingNode, mode string, masterName, sentinels, nodes bool) []diag.Diagnostic {
	var out []diag.Diagnostic
	warn := func(yamlPath, field string) {
		out = append(out, atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:    "redis_unused_field",
			Message: fmt.Sprintf("redis.%s is ignored when redis.mode=%s", field, mode),
		}))
	}
	if masterName {
		warn("$.redis.master_name", "master_name")
	}
	if sentinels {
		warn("$.redis.sentinels", "sentinels")
	}
	if nodes {
		warn("$.redis.nodes", "nodes")
	}
	return out
}

// validateVaultAuth checks the `vault.auth` block (ADR-014, AppRole).
//
// vaultPresent — whether the top-level `vault` key is present in the YAML; if
// absent the block is not validated (the top-level required check already emitted
// missing_required_field for `vault` itself, no need to duplicate).
//
// Rules:
//   - `method` (if set) — from enumVaultAuth (token | approle); empty = token.
//   - method=token: approle fields are ignored (if set — a warn about unused
//     fields, not an error: to catch a typo in method).
//   - method=approle: `role_id` is required; exactly one secret_id source —
//     `secret_id_file` OR `secret_id_env` (neither zero nor both); `secret_id_file`
//     must be an absolute path; `token` must not be set with approle (mutually
//     exclusive authentication sources).
//
// role_id is not a secret, plaintext in the config is fine. A plaintext secret_id
// is not allowed by the schema (no such field) — only file/env.
func validateVaultAuth(root *ast.MappingNode, v *KeeperVault, vaultPresent bool) []diag.Diagnostic {
	if !vaultPresent {
		return nil
	}
	var out []diag.Diagnostic

	// `kv_version` — escape-hatch for the KV mount version probe: empty → auto,
	// otherwise strictly "1"/"2". An invalid value is caught here to avoid a
	// runtime failure on the first ReadKV (the override beats the probe).
	if v.KVVersion != "" && v.KVVersion != "1" && v.KVVersion != "2" {
		out = append(out, atPath(root, "$.vault.kv_version", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_kv_version_invalid",
			Message: fmt.Sprintf("vault.kv_version must be \"1\" or \"2\", got %q", v.KVVersion),
			Hint:    "leave empty for auto-detect via sys/internal/ui/mounts, or set \"1\"/\"2\"",
		}))
	}

	// `kv_mount` — one plain path segment, or empty for the default. A mount is
	// not merely a prefix here: [ADR-0083] §1 derives every declared secret's path
	// as <mount>/<service>/<incarnation>/…, and the §7 fence recognises a service's
	// own namespace by finding the service in the FIRST TWO segments. A two-segment
	// mount ("apps/kv") pushes the service to the third and switches the fence off
	// silently, which is the one failure this whole feature exists to prevent.
	if m := v.KVMount; m != "" && !ValidVaultPathSegment(m) {
		out = append(out, atPath(root, "$.vault.kv_mount", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_kv_mount_invalid",
			Message: fmt.Sprintf("vault.kv_mount must be a single path segment (letters, digits, %q, %q), got %q", "_", "-", m),
			Hint:    "leave empty for the default \"secret\" mount, or name one segment without slashes",
		}))
	}

	a := &v.Auth

	if a.Method != "" {
		out = append(out, checkEnum(root, "$.vault.auth.method", a.Method, enumVaultAuth)...)
		// Invalid method → further per-method checks are meaningless.
		if !contains(enumVaultAuth, a.Method) {
			return out
		}
	}

	switch a.ResolvedAuthMethod() {
	case AuthMethodToken:
		// approle fields are unused with the token method — a soft signal.
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

// isVaultRef checks the `vault:<mount>/<path>` form (minimally: `vault:` prefix,
// non-empty body, a `/` separator between mount and path, not at the edges).
// Mirrors keeper/internal/vault.ParseRef but without importing the keeper package
// into shared/ (ADR-011). Full ref resolution is done by keeper-vault at runtime.
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

// checkListEntryHostPort is the [checkHostPort] variant for list elements
// (`sentinels[]`/`nodes[]`), where an empty element is not "omitted" but an
// erroneously empty entry (`["", "s:26379"]`): the required check catches only a
// fully empty list, so a single empty element must be rejected explicitly with
// the same host_port_invalid code.
func checkListEntryHostPort(root *ast.MappingNode, yamlPath, val string) []diag.Diagnostic {
	if val == "" {
		return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "host_port_invalid",
			Message: "empty list entry: expected host:port",
		})}
	}
	return checkHostPort(root, yamlPath, val)
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

// checkPort validates an integer port in range 1..65535. Port 0 is treated as
// "unset" by the caller (a separate *_required diag); only non-zero values reach
// here.
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

// atPath enriches a diagnostic with line/column by resolving the yaml path in the
// AST.
//
// If the path isn't found, the diag is returned as is (without a position).
func atPath(root *ast.MappingNode, yamlPath string, d diag.Diagnostic) diag.Diagnostic {
	d.YAMLPath = yamlPath
	if line, col, ok := lookupPath(root, yamlPath); ok {
		d.Line = line
		d.Column = col
	}
	return d
}

// topLevelKeys returns the set of top-level key names in the mapping. Used to
// check presence of required blocks: a zero-value Go struct can't tell "key
// absent" from "key present with an empty body", and for `missing_required_field`
// we care about presence itself.
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

// pathWithin reports whether sub is inside (or equal to) base. Both are expected
// absolute (called after a filepath.IsAbs check). Compares cleaned paths with a
// boundary separator so `/a/bc` isn't considered inside `/a/b`.
func pathWithin(sub, base string) bool {
	b := filepath.Clean(base)
	s := filepath.Clean(sub)
	if s == b {
		return true
	}
	return strings.HasPrefix(s, b+string(filepath.Separator))
}
