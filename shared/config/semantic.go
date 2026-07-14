package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Regex catalog per naming-rules.md and docs/keeper|soul/config.md.
//
// KID, namespace and plugin-name share the form `^[a-z][a-z0-9-]{0,N}$`. FQDN
// follows RFC 1035/1123, label without trailing/leading dash. Vault-ref is
// `vault:<path>` or `vault:<path>#<field>`.
var (
	reKID      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	reFQDNLab  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reVaultRef = regexp.MustCompile(`^vault:[A-Za-z0-9_./\-]+(#[A-Za-z0-9_.\-]+)?$`)
)

// semanticValidateKeeper runs post-decode checks on KeeperConfig.
// Uses root (AST) only to resolve yaml-paths to positions.
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
	if c.Redis.SentinelPasswordRef != "" {
		out = append(out, checkVaultRef(root, "$.redis.sentinel_password_ref", c.Redis.SentinelPasswordRef)...)
	}
	if c.Auth != nil && c.Auth.JWT != nil && c.Auth.JWT.SigningKeyRef != "" {
		out = append(out, checkVaultRef(root, "$.auth.jwt.signing_key_ref", c.Auth.JWT.SigningKeyRef)...)
	}
	// cloud_init.tls_ca_ref (ADR-017(h) amendment 2026-05-27, B-flat): vault-ref
	// to the Keeper's PEM CA for cloud-init userdata. Resolved on the
	// GenerateUserdata call via the keeper-vault client; format is checked here
	// (jwt.signing_key_ref style).
	if c.CloudInit != nil && c.CloudInit.TLSCARef != "" {
		out = append(out, checkVaultRef(root, "$.cloud_init.tls_ca_ref", c.CloudInit.TLSCARef)...)
	}

	// duration fields.
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
	// cadence_scheduler (Conductor, ADR-048): duration format for tick-interval
	// and lock_ttl. Range (>0) is resolved by default in
	// ResolvedInterval/ResolvedLockTTL (reaper / acolyte_* style); here only format.
	if c.CadenceScheduler != nil {
		out = append(out, checkDuration(root, "$.cadence_scheduler.interval", c.CadenceScheduler.Interval)...)
		out = append(out, checkDuration(root, "$.cadence_scheduler.lock_ttl", c.CadenceScheduler.LockTTL)...)
		out = append(out, checkDuration(root, "$.cadence_scheduler.poll_floor", c.CadenceScheduler.PollFloor)...)
		out = append(out, checkDuration(root, "$.cadence_scheduler.poll_ceiling", c.CadenceScheduler.PollCeiling)...)
		out = append(out, checkDuration(root, "$.cadence_scheduler.poll_idle", c.CadenceScheduler.PollIdle)...)
		out = append(out, checkCadencePollProfile(root, c.CadenceScheduler)...)
		out = append(out, cadenceIntervalBelowFloorWarn(root, c.CadenceScheduler)...)
	}
	// Acolyte pool (ADR-027): duration format for lease/poll/drain. Range (>0) is
	// enforced after parsing in the daemon, like other duration fields; here only format.
	out = append(out, checkDuration(root, "$.acolyte_lease", c.AcolyteLease)...)
	out = append(out, checkDuration(root, "$.acolyte_poll_interval", c.AcolytePollInterval)...)
	out = append(out, checkDuration(root, "$.acolyte_drain_grace", c.AcolyteDrainGrace)...)

	// Oracle circuit-breaker (ADR-030(a), beacons S4): duration format for the
	// fixed-window. Range (>0) is resolved by default in the daemon; here only
	// format (acolyte_* style). max_fires is int, validated in the schema phase.
	out = append(out, checkDuration(root, "$.oracle_circuit_window", c.OracleCircuitWindow)...)

	// plugins.fetch_timeout (ADR-026 F-fetch): duration format for the plugin
	// git-resolve timeout. Range (>0) is resolved by default in
	// ResolvedFetchTimeout; here only format (acolyte_* style).
	if c.Plugins != nil {
		out = append(out, checkDuration(root, "$.plugins.fetch_timeout", c.Plugins.FetchTimeout)...)
	}

	// sigil_anchors_reload_interval (ADR-026(h), R3 known-gap): duration format
	// for the TTL-fallback reread period of the signature trust-anchor key set.
	// Range (>0) is resolved by default in the daemon; here only format (acolyte_* style).
	out = append(out, checkDuration(root, "$.sigil_anchors_reload_interval", c.SigilAnchorsReloadInterval)...)

	// watchman_interval (soul-shedding S2): duration format for the Watchman
	// probe-tick period (isolation detection). Range (>0) is resolved by default
	// in the daemon; here only format (acolyte_* / oracle_circuit_window style).
	out = append(out, checkDuration(root, "$.watchman_interval", c.WatchmanInterval)...)

	// max_await_timeout (ADR-061): duration format for the onboarding-barrier
	// ceiling of `core.soul.registered`. Range (>0) is resolved by default in
	// ResolvedMaxAwaitTimeout; here only format (acolyte_* / watchman_interval style).
	out = append(out, checkDuration(root, "$.max_await_timeout", c.MaxAwaitTimeout)...)

	// toll.* (cluster-wide mass-churn detector, ADR-038): duration formats for
	// sub-fields. Ranges (>0) are resolved by default in the daemon; here only
	// format. Threshold is float, range checked in the schema phase.
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

	// auth.ldap (ADR-058): TLS-required + mutually-exclusive transport +
	// search-bind ⇒ bind_dn+bind_password_ref + vault-ref form + insecure WARN.
	if c.Auth != nil && c.Auth.LDAP != nil {
		out = append(out, checkAuthLDAP(root, c.Auth.LDAP)...)
	}

	// auth.oidc (ADR-058 stage 2): issuer HTTPS-only + required
	// client_id/redirect_url + vault-ref form for client_secret_ref/ca_ref.
	if c.Auth != nil && c.Auth.OIDC != nil {
		out = append(out, checkAuthOIDC(root, c.Auth.OIDC)...)
	}

	// auth.rate_limit (ADR-058(g), HIGH-3): duration format for lockout window/backoff.
	if c.Auth != nil && c.Auth.RateLimit != nil {
		out = append(out, checkDuration(root, "$.auth.rate_limit.lockout_window", c.Auth.RateLimit.LockoutWindow)...)
		out = append(out, checkDuration(root, "$.auth.rate_limit.lockout_backoff", c.Auth.RateLimit.LockoutBackoff)...)
	}

	// Cross-field: audit.retention_days aliases reaper.rules.purge_audit_old.max_age.
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

// semanticValidateSoul runs post-decode checks on SoulConfig.
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

// checkAuthLDAP runs semantic checks on the auth.ldap block (ADR-058, search-bind).
//
// Safe perimeter (ADR-058(g), "security first"):
//   - TLS required: url starts with `ldaps://` OR start_tls: true; otherwise
//     plaintext LDAP is forbidden (ERROR) — operator passwords must not travel
//     in the clear;
//   - `ldaps://` and start_tls are mutually exclusive (ERROR) — StartTLS over an
//     already-encrypted channel is pointless and wrong;
//   - bind_mode empty or `search` (default) ⇒ requires bind_dn+bind_password_ref;
//   - vault-ref form for bind_password_ref and tls.ca_ref;
//   - insecure_skip_verify: true → WARN (dev-only, not for prod).
//
// *_ref resolution + *tls.Config assembly happen at load-time in the daemon
// (ADR-058(e), redis.password_ref style); here only static form/invariants.
func checkAuthLDAP(root *ast.MappingNode, l *KeeperAuthLDAP) []diag.Diagnostic {
	var out []diag.Diagnostic

	isLDAPS := strings.HasPrefix(l.URL, "ldaps://")
	isPlainLDAP := strings.HasPrefix(l.URL, "ldap://")

	// TLS-required: either ldaps://, or ldap://+start_tls.
	if !isLDAPS && !(isPlainLDAP && l.StartTLS) {
		out = append(out, atPath(root, "$.auth.ldap.url", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "ldap_plaintext_forbidden",
			Message: fmt.Sprintf("plaintext LDAP forbidden: url %q must be ldaps:// or ldap:// with start_tls: true", l.URL),
			Hint:    "use ldaps://host:636, or ldap://host:389 with start_tls: true",
		}))
	}

	// ldaps:// and start_tls are mutually exclusive.
	if isLDAPS && l.StartTLS {
		out = append(out, atPath(root, "$.auth.ldap.start_tls", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "ldap_tls_conflict",
			Message: "ldaps:// url and start_tls: true are mutually exclusive",
			Hint:    "ldaps:// already encrypts the channel; drop start_tls",
		}))
	}

	// bind_mode outside {"", "search"} → ERROR at load (not runtime in ldap.New):
	// stage 1 of ADR-058(c) supports only search-bind; direct-bind is deferred
	// without a breaking change. `user_dn_template` stays in the config as a
	// placeholder for a future direct mode, but is NOT activated by bind_mode.
	if l.BindMode != "" && l.BindMode != "search" {
		out = append(out, atPath(root, "$.auth.ldap.bind_mode", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "ldap_bind_mode_unsupported",
			Message: fmt.Sprintf("auth.ldap.bind_mode %q is unsupported: only \"search\" (or empty=default) is implemented in stage 1", l.BindMode),
			Hint:    "set bind_mode: search (direct-bind is deferred)",
		}))
	}

	// bind_mode empty (=search default) or search ⇒ a service-account is required.
	if l.BindMode == "" || l.BindMode == "search" {
		if l.BindDN == "" {
			out = append(out, atPath(root, "$.auth.ldap.bind_dn", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "ldap_search_requires_bind_dn",
				Message: "bind_mode=search requires bind_dn (service-account DN)",
			}))
		}
		if l.BindPasswordRef == "" {
			out = append(out, atPath(root, "$.auth.ldap.bind_password_ref", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "ldap_search_requires_bind_password",
				Message: "bind_mode=search requires bind_password_ref (vault-ref)",
			}))
		}
	}

	if l.BindPasswordRef != "" {
		out = append(out, checkVaultRef(root, "$.auth.ldap.bind_password_ref", l.BindPasswordRef)...)
	}
	if l.TLS.CARef != "" {
		out = append(out, checkVaultRef(root, "$.auth.ldap.tls.ca_ref", l.TLS.CARef)...)
	}
	out = append(out, checkDuration(root, "$.auth.ldap.timeout", l.Timeout)...)

	if l.TLS.InsecureSkipVerify {
		out = append(out, atPath(root, "$.auth.ldap.tls.insecure_skip_verify", diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSemanticValidate,
			Code:    "ldap_insecure_skip_verify",
			Message: "auth.ldap.tls.insecure_skip_verify: true disables LDAPS certificate verification (dev-only)",
			Hint:    "production must verify the server certificate; set a tls.ca_ref instead",
		}))
	}

	return out
}

// checkAuthOIDC runs semantic checks on the auth.oidc block (ADR-058(b)/(e), stage 2).
//
// Safe perimeter (ADR-058(g), "security first"):
//   - issuer — HTTPS only (ERROR otherwise): discovery/JWKS/token-exchange reach
//     the IdP, plaintext is unacceptable (MITM on id_token);
//   - client_id and redirect_url are required (ERROR);
//   - client_secret_ref (optional, a public-client may lack it) and tls.ca_ref
//     are vault-ref forms.
//
// PKCE is mandatory at the implementation level (auth/oidc always sends an
// S256-challenge), so there is NO use_pkce config flag — it is not left to the
// operator (ADR-058 fork №6 resolved in favor of "mandatory").
//
// *_ref resolution + discovery → auth/oidc.Config happen at load-time in the
// daemon (setupOIDCAuth); here only static form/invariants.
func checkAuthOIDC(root *ast.MappingNode, o *KeeperAuthOIDC) []diag.Diagnostic {
	var out []diag.Diagnostic

	if !strings.HasPrefix(o.Issuer, "https://") {
		out = append(out, atPath(root, "$.auth.oidc.issuer", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "oidc_issuer_not_https",
			Message: fmt.Sprintf("auth.oidc.issuer %q must be https:// (OIDC discovery/JWKS over TLS)", o.Issuer),
			Hint:    "use https://idp.example.com/realms/...",
		}))
	}
	if o.ClientID == "" {
		out = append(out, atPath(root, "$.auth.oidc.client_id", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "oidc_client_id_required",
			Message: "auth.oidc.client_id is required",
		}))
	}
	if o.RedirectURL == "" {
		out = append(out, atPath(root, "$.auth.oidc.redirect_url", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "oidc_redirect_url_required",
			Message: "auth.oidc.redirect_url is required (e.g. https://keeper.example.com/auth/oidc/callback)",
		}))
	}
	if o.ClientSecretRef != "" {
		out = append(out, checkVaultRef(root, "$.auth.oidc.client_secret_ref", o.ClientSecretRef)...)
	}
	if o.TLS.CARef != "" {
		out = append(out, checkVaultRef(root, "$.auth.oidc.tls.ca_ref", o.TLS.CARef)...)
	}

	// aid_claim from a user-mutable claim → identity-spoofing risk (WARN). `sub`
	// (default, MED-fix) is the immutable IdP subject; email/preferred_username
	// can be reassigned by the user/IdP admin, and then a different person who
	// obtains that email/username at the IdP logs in under an existing AID.
	switch o.AIDClaim {
	case "email", "preferred_username":
		out = append(out, atPath(root, "$.auth.oidc.aid_claim", diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSemanticValidate,
			Code:    "oidc_aid_claim_mutable",
			Message: fmt.Sprintf("auth.oidc.aid_claim=%q is user-mutable (identity-spoofing risk): a reassigned %s lets a different person log in as the same AID", o.AIDClaim, o.AIDClaim),
			Hint:    "prefer the immutable subject identifier: aid_claim: sub (default)",
		}))
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

// checkDuration accepts a value per the Soul Stack `duration` convention
// (see docs/keeper/config.md → "Type conventions" and docs/naming-rules.md →
// "DSL syntax"): Go-`time.ParseDuration` or the `<N>d` suffix.
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

// minCadencePollFloor is the absolute minimum poll_floor (ADR-048 "Adaptive
// interval"): the Conductor poll step never drops below 30s. Matches the DB-CHECK
// floor interval_seconds ≥ 30 (Pass B / ADR-046).
const minCadencePollFloor = 30 * time.Second

// checkCadencePollProfile checks the range/ordering of the adaptive-poll
// corridor (ADR-048): poll_floor ≥ 30s, poll_floor ≤ poll_ceiling,
// poll_idle ≥ poll_ceiling. Works on resolved values (defaults +
// backcompat-alias interval→ceiling), so it catches implicit violations too. The
// duration format is checked separately by [checkDuration]; an invalid format
// here resolves to a default and yields no false range diagnostic.
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

// cadenceIntervalBelowFloorWarn emits a WARNING (not an error) when the
// backcompat-alias `interval` is set, `poll_ceiling` is unset, and
// `interval < poll_floor`. An old small interval (dev configs set sub-30s, e.g.
// 5s) does NOT fail the config — [KeeperCadenceScheduler.ResolvedPollCeiling]
// raises ceiling up to floor (clamp up). We tell the operator that the actual
// poll won't drop below floor, and that sub-30s reactions need Beacons (ADR-030).
// No error is emitted: the alias-clamp guarantees ceiling ≥ floor; a real
// floor > ceiling config error is possible only with an EXPLICIT poll_ceiling
// (caught by [checkCadencePollProfile]).
func cadenceIntervalBelowFloorWarn(root *ast.MappingNode, cs *KeeperCadenceScheduler) []diag.Diagnostic {
	if cs.PollCeiling != "" || cs.Interval == "" {
		return nil
	}
	interval, err := ParseDuration(cs.Interval)
	if err != nil || interval <= 0 {
		// A broken interval format already yields duration_invalid; non-positive
		// resolves to the 60s default (> floor) — no raise.
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

// isFQDN — RFC 1035/1123 label set joined by dots. Accepts 1+ labels;
// each label is ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$, no trailing dot.
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
