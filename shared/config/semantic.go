package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Regex-каталог по naming-rules.md и docs/keeper|soul/config.md.
//
// KID, namespace и plugin-name делят форму `^[a-z][a-z0-9-]{0,N}$`. FQDN —
// по RFC 1035/1123, label без trailing/leading dash. Vault-ref —
// `vault:<path>` либо `vault:<path>#<field>`.
var (
	reKID      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	reFQDNLab  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reVaultRef = regexp.MustCompile(`^vault:[A-Za-z0-9_./\-]+(#[A-Za-z0-9_.\-]+)?$`)
)

// semanticValidateKeeper — пост-decode проверки KeeperConfig.
// Использует root (AST) только для резолва yaml-paths к позициям.
func semanticValidateKeeper(c *KeeperConfig, root *ast.MappingNode) []diag.Diagnostic {
	var out []diag.Diagnostic

	if c.KID != "" && !reKID.MatchString(c.KID) {
		out = append(out, atPath(root, "$.kid", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "kid_invalid_format",
			Message: fmt.Sprintf("kid %q does not match %s", c.KID, reKID),
		}))
	}

	// vault-refs.
	if c.Postgres.DSNRef != "" {
		out = append(out, checkVaultRef(root, "$.postgres.dsn_ref", c.Postgres.DSNRef)...)
	}
	if c.Redis.PasswordRef != "" {
		out = append(out, checkVaultRef(root, "$.redis.password_ref", c.Redis.PasswordRef)...)
	}
	if c.Auth != nil && c.Auth.JWT != nil && c.Auth.JWT.SigningKeyRef != "" {
		out = append(out, checkVaultRef(root, "$.auth.jwt.signing_key_ref", c.Auth.JWT.SigningKeyRef)...)
	}
	// cloud_init.tls_ca_ref (ADR-017(h) amendment 2026-05-27, B-flat): vault-ref
	// на PEM CA Keeper-а для cloud-init userdata. Резолв — на GenerateUserdata-
	// вызове через keeper-vault-клиент; формат проверяется тут (стиль jwt.signing_key_ref).
	if c.CloudInit != nil && c.CloudInit.TLSCARef != "" {
		out = append(out, checkVaultRef(root, "$.cloud_init.tls_ca_ref", c.CloudInit.TLSCARef)...)
	}

	// duration-поля.
	if c.Auth != nil && c.Auth.JWT != nil {
		out = append(out, checkDuration(root, "$.auth.jwt.ttl_default", c.Auth.JWT.TTLDefault)...)
		out = append(out, checkDuration(root, "$.auth.jwt.ttl_bootstrap", c.Auth.JWT.TTLBootstrap)...)
	}
	if c.PluginRuntime != nil {
		out = append(out, checkDuration(root, "$.plugin_runtime.startup_timeout", c.PluginRuntime.StartupTimeout)...)
		out = append(out, checkDuration(root, "$.plugin_runtime.shutdown_grace", c.PluginRuntime.ShutdownGrace)...)
	}
	if c.Reaper != nil {
		out = append(out, checkDuration(root, "$.reaper.interval", c.Reaper.Interval)...)
		out = append(out, checkDuration(root, "$.reaper.lock_ttl", c.Reaper.LockTTL)...)
		for name, rule := range c.Reaper.Rules {
			prefix := "$.reaper.rules." + name
			out = append(out, checkDuration(root, prefix+".max_age", rule.MaxAge)...)
			out = append(out, checkDuration(root, prefix+".stale_after", rule.StaleAfter)...)
		}
	}
	// cadence_scheduler (Conductor, ADR-048): duration-формат tick-interval и
	// lock_ttl. Диапазон (>0) резолвится дефолтом в ResolvedInterval/ResolvedLockTTL
	// (стиль reaper / acolyte_*); здесь только формат.
	if c.CadenceScheduler != nil {
		out = append(out, checkDuration(root, "$.cadence_scheduler.interval", c.CadenceScheduler.Interval)...)
		out = append(out, checkDuration(root, "$.cadence_scheduler.lock_ttl", c.CadenceScheduler.LockTTL)...)
		out = append(out, checkDuration(root, "$.cadence_scheduler.poll_floor", c.CadenceScheduler.PollFloor)...)
		out = append(out, checkDuration(root, "$.cadence_scheduler.poll_ceiling", c.CadenceScheduler.PollCeiling)...)
		out = append(out, checkDuration(root, "$.cadence_scheduler.poll_idle", c.CadenceScheduler.PollIdle)...)
		out = append(out, checkCadencePollProfile(root, c.CadenceScheduler)...)
		out = append(out, cadenceIntervalBelowFloorWarn(root, c.CadenceScheduler)...)
	}
	// Acolyte-пул (ADR-027): duration-формат lease/poll/drain. Диапазон (>0) —
	// после парсинга в daemon, как у прочих duration-полей; здесь только формат.
	out = append(out, checkDuration(root, "$.acolyte_lease", c.AcolyteLease)...)
	out = append(out, checkDuration(root, "$.acolyte_poll_interval", c.AcolytePollInterval)...)
	out = append(out, checkDuration(root, "$.acolyte_drain_grace", c.AcolyteDrainGrace)...)

	// circuit-breaker Oracle (ADR-030(a), beacons S4): duration-формат окна
	// fixed-window. Диапазон (>0) резолвится дефолтом в daemon; здесь только
	// формат (стиль acolyte_*). max_fires — int, валидируется schema-фазой.
	out = append(out, checkDuration(root, "$.oracle_circuit_window", c.OracleCircuitWindow)...)

	// plugins.fetch_timeout (ADR-026 F-fetch): duration-формат таймаута git-
	// резолва плагина. Диапазон (>0) резолвится дефолтом в ResolvedFetchTimeout;
	// здесь только формат (стиль acolyte_*).
	if c.Plugins != nil {
		out = append(out, checkDuration(root, "$.plugins.fetch_timeout", c.Plugins.FetchTimeout)...)
	}

	// sigil_anchors_reload_interval (ADR-026(h), R3 known-gap): duration-формат
	// периода TTL-fallback-перечита набора trust-anchor-ключей подписи. Диапазон
	// (>0) резолвится дефолтом в daemon; здесь только формат (стиль acolyte_*).
	out = append(out, checkDuration(root, "$.sigil_anchors_reload_interval", c.SigilAnchorsReloadInterval)...)

	// watchman_interval (soul-shedding S2): duration-формат периода probe-тика
	// Watchman (изоляция-детект). Диапазон (>0) резолвится дефолтом в daemon;
	// здесь только формат (стиль acolyte_* / oracle_circuit_window).
	out = append(out, checkDuration(root, "$.watchman_interval", c.WatchmanInterval)...)

	// toll.* (cluster-wide detector массового оттока, ADR-038): duration-форматы
	// под-полей. Диапазоны (>0) резолвятся дефолтом в daemon; здесь только формат.
	// Threshold — float, диапазон проверяется schema-фазой.
	if c.Toll != nil {
		out = append(out, checkDuration(root, "$.toll.window_size", c.Toll.WindowSize)...)
		out = append(out, checkDuration(root, "$.toll.degraded_ttl", c.Toll.DegradedTTL)...)
		out = append(out, checkDuration(root, "$.toll.clear_grace", c.Toll.ClearGrace)...)
		out = append(out, checkDuration(root, "$.toll.lease_ttl", c.Toll.LeaseTTL)...)
		out = append(out, checkDuration(root, "$.toll.warmup_delay", c.Toll.WarmupDelay)...)
		if c.Toll.Webhook != nil {
			out = append(out, checkDuration(root, "$.toll.webhook.timeout", c.Toll.Webhook.Timeout)...)
		}
	}

	// Cross-field: audit.retention_days alias на reaper.rules.purge_audit_old.max_age.
	if c.Audit != nil && c.Reaper != nil {
		if rule, ok := c.Reaper.Rules["purge_audit_old"]; ok && rule.MaxAge != "" && c.Audit.RetentionDays != 0 {
			if maxAge, err := ParseDuration(rule.MaxAge); err == nil {
				expect := time.Duration(c.Audit.RetentionDays) * 24 * time.Hour
				if maxAge != expect {
					out = append(out, atPath(root, "$.audit.retention_days", diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
						Code: "audit_retention_mismatch",
						Message: fmt.Sprintf(
							"audit.retention_days (%dd) != reaper.rules.purge_audit_old.max_age (%s)",
							c.Audit.RetentionDays, rule.MaxAge),
						Hint: "one source of truth (ADR-022); make them equal",
					}))
				}
			}
		}
	}

	return out
}

// semanticValidateSoul — пост-decode проверки SoulConfig.
func semanticValidateSoul(c *SoulConfig, root *ast.MappingNode) []diag.Diagnostic {
	var out []diag.Diagnostic

	if c.SID != "" && !isFQDN(c.SID) {
		out = append(out, atPath(root, "$.sid", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "sid_invalid_format",
			Message: fmt.Sprintf("sid %q is not a valid FQDN", c.SID),
			Hint:    "labels through dots, each label ^[a-z0-9-]{1,63}$, no leading/trailing dash",
		}))
	}

	if c.Keeper.Retry != nil {
		b := c.Keeper.Retry.Backoff
		out = append(out, checkDuration(root, "$.keeper.retry.backoff.initial", b.Initial)...)
		out = append(out, checkDuration(root, "$.keeper.retry.backoff.max", b.Max)...)
		out = append(out, checkDuration(root, "$.keeper.retry.handshake_timeout", c.Keeper.Retry.HandshakeTimeout)...)
	}
	if c.Keeper.Failback != nil {
		out = append(out, checkDuration(root, "$.keeper.failback.interval", c.Keeper.Failback.Interval)...)
		out = append(out, checkDuration(root, "$.keeper.failback.spray", c.Keeper.Failback.Spray)...)
	}
	if c.Soulprint != nil {
		out = append(out, checkDuration(root, "$.soulprint.refresh_interval", c.Soulprint.RefreshInterval)...)
	}
	if c.Cleanup != nil {
		out = append(out, checkDuration(root, "$.cleanup.run_interval", c.Cleanup.RunInterval)...)
		if c.Cleanup.ModulesTTLDays < 0 {
			out = append(out, atPath(root, "$.cleanup.modules_ttl_days", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("cleanup.modules_ttl_days must be >= 0, got %d", c.Cleanup.ModulesTTLDays),
			}))
		}
	}
	if c.PluginRuntime != nil {
		out = append(out, checkDuration(root, "$.plugin_runtime.startup_timeout", c.PluginRuntime.StartupTimeout)...)
		out = append(out, checkDuration(root, "$.plugin_runtime.shutdown_grace", c.PluginRuntime.ShutdownGrace)...)
	}

	return out
}

func checkVaultRef(root *ast.MappingNode, yamlPath, val string) []diag.Diagnostic {
	if reVaultRef.MatchString(val) {
		return nil
	}
	return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		Code:    "vault_ref_invalid_format",
		Message: fmt.Sprintf("vault-ref %q must match vault:<path>[#<field>]", val),
	})}
}

// checkDuration принимает значение по convention `duration` Soul Stack
// (см. docs/keeper/config.md → «Конвенции типов» и docs/naming-rules.md →
// «DSL-синтаксис»): Go-`time.ParseDuration` либо суффикс `<N>d`.
func checkDuration(root *ast.MappingNode, yamlPath, val string) []diag.Diagnostic {
	if val == "" {
		return nil
	}
	_, err := ParseDuration(val)
	if err == nil {
		return nil
	}
	hint := "use Go-duration (e.g. 24h, 5m) or <N>d for days (composite forms like 1d2h are not supported)"
	if strings.Contains(err.Error(), "value too large") {
		hint = fmt.Sprintf("value too large; max is ~292 years (%d days)", MaxDurationDays)
	}
	return []diag.Diagnostic{atPath(root, yamlPath, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		Code:    "duration_invalid",
		Message: fmt.Sprintf("invalid duration %q: %v", val, err),
		Hint:    hint,
	})}
}

// minCadencePollFloor — абсолютный минимум poll_floor (ADR-048 «Adaptive
// interval»): шаг опроса Conductor не опускается ниже 30s. Совпадает с DB-CHECK
// floor interval_seconds ≥ 30 (Pass B / ADR-046).
const minCadencePollFloor = 30 * time.Second

// checkCadencePollProfile проверяет диапазон/взаимный порядок коридора
// адаптивного опроса (ADR-048): poll_floor ≥ 30s, poll_floor ≤ poll_ceiling,
// poll_idle ≥ poll_ceiling. Работает по резолвнутым значениям (дефолты +
// backcompat-alias interval→ceiling), поэтому ловит и неявные нарушения. Формат
// duration проверяется отдельно [checkDuration]-ом; невалидный формат тут
// резолвится дефолтом и не даёт ложного range-диагноза.
func checkCadencePollProfile(root *ast.MappingNode, cs *KeeperCadenceScheduler) []diag.Diagnostic {
	floor := cs.ResolvedPollFloor()
	ceiling := cs.ResolvedPollCeiling()
	idle := cs.ResolvedPollIdle()

	var out []diag.Diagnostic
	rangeErr := func(yamlPath, msg string) {
		out = append(out, atPath(root, yamlPath, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code: "value_out_of_range", Message: msg,
		}))
	}
	if floor < minCadencePollFloor {
		rangeErr("$.cadence_scheduler.poll_floor",
			fmt.Sprintf("poll_floor must be >= %s (абсолютный минимум шага опроса), got %s", minCadencePollFloor, floor))
	}
	if floor > ceiling {
		rangeErr("$.cadence_scheduler.poll_floor",
			fmt.Sprintf("poll_floor (%s) must be <= poll_ceiling (%s)", floor, ceiling))
	}
	if idle < ceiling {
		rangeErr("$.cadence_scheduler.poll_idle",
			fmt.Sprintf("poll_idle (%s) must be >= poll_ceiling (%s) — idle опрос не чаще обычного", idle, ceiling))
	}
	return out
}

// cadenceIntervalBelowFloorWarn эмитит WARNING (не error), когда backcompat-alias
// `interval` задан, `poll_ceiling` не задан, и `interval < poll_floor`. Старый
// малый interval (dev-конфиги ставили суб-30s, напр. 5s) НЕ роняет конфиг —
// [KeeperCadenceScheduler.ResolvedPollCeiling] поднимает ceiling до floor (clamp
// вверх). Оператору сообщаем, что фактический опрос не опустится ниже floor, и
// что для суб-30s реакции нужны Beacons (ADR-030). error НЕ эмитим: alias-clamp
// гарантирует ceiling ≥ floor, реальная конфиг-ошибка floor > ceiling возможна
// только при ЯВНОМ poll_ceiling (её ловит [checkCadencePollProfile]).
func cadenceIntervalBelowFloorWarn(root *ast.MappingNode, cs *KeeperCadenceScheduler) []diag.Diagnostic {
	if cs.PollCeiling != "" || cs.Interval == "" {
		return nil
	}
	interval, err := ParseDuration(cs.Interval)
	if err != nil || interval <= 0 {
		// Битый формат interval уже даст duration_invalid; non-positive резолвится
		// дефолтом 60s (> floor) — подъёма нет.
		return nil
	}
	floor := cs.ResolvedPollFloor()
	if interval >= floor {
		return nil
	}
	return []diag.Diagnostic{atPath(root, "$.cadence_scheduler.interval", diag.Diagnostic{
		Level: diag.LevelWarning, Phase: diag.PhaseSemanticValidate,
		Code: "value_clamped",
		Message: fmt.Sprintf(
			"cadence_scheduler.interval %s ниже poll_floor %s — поднято до %s; для суб-30s реакции используйте Beacons (ADR-030)",
			interval, floor, floor),
	})}
}

// isFQDN — RFC 1035/1123 label set через точки. Принимает 1+ labels;
// каждый label — ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$, без trailing dot.
func isFQDN(s string) bool {
	if s == "" || strings.HasSuffix(s, ".") || strings.HasPrefix(s, ".") {
		return false
	}
	for _, lab := range strings.Split(s, ".") {
		if !reFQDNLab.MatchString(lab) {
			return false
		}
	}
	return true
}
