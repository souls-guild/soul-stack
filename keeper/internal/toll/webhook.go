package toll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// VaultReader — narrow interface for reading Vault KV to resolve webhook URLRef
// (ADR-038 amendment, extensions). Implementation — *vault.Client.ReadKV in daemon
// wire-up; fake in unit-tests. Contract: returns `url` field (or path-
// specific field — operator puts url in KV under this key, symmetric to
// signing_key/public_key conventions of existing vault-refs).
type VaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// urlFieldName — name of field in Vault KV where webhook-URL lives. Convention
// «secret = one field in KV under known-name»; symmetry with
// `signing_key`/`public_key`/`password` of other vault-refs.
const urlFieldName = "url"

// vaultRefPrefix — prefix of `url_ref` by which WebhookNotifier
// distinguishes vault-ref (resolution via VaultReader) from inline-URL.
const vaultRefPrefix = "vault:"

// WebhookNotifier — implementation of [Notifier] for HTTP-POST alert channel. One
// instance per keeper process; thread-safe (HTTP-client + immutable cfg/vault).
//
// SECURITY: webhook-URL is a secret (reveals pager-integration). For prod
// use vault-ref; inline-URL allowed but not recommended.
//
// Lifecycle: Notify called only under leader-lease (single-winner,
// ADR-038 invariant). No retries — best-effort: timeout / non-2xx / DNS-error
// logged, error not returned to caller ([Notifier] contract).
type WebhookNotifier struct {
	cfg    WebhookConfig
	client *http.Client
	vault  VaultReader
	logger *slog.Logger
}

// WebhookConfig — [WebhookNotifier] parameters. Resolved by daemon from
// config.KeeperTollWebhook + defaults.
type WebhookConfig struct {
	// URLRef — `vault:<mount>/<path>` (`url` field in KV) or inline URL
	// (`http(s)://...`). Distinction — by `vault:` prefix.
	URLRef string
	// Format — one of [TollWebhookFormat*] constants (config package).
	// Resolved by daemon: empty → generic.
	Format string
	// Timeout — ceiling for one POST call. <=0 → default
	// [DefaultWebhookTimeout] (10s).
	Timeout time.Duration
}

const (
	// DefaultWebhookTimeout — default for WebhookConfig.Timeout. Matches
	// shared/config.DefaultTollWebhookTimeout; duplicated here so
	// toll package doesn't pull shared/config (dependency direction
	// daemon → toll, not reversed).
	DefaultWebhookTimeout = 10 * time.Second
)

// NewWebhookNotifier validates cfg and builds notifier. vault may be nil
// for inline-URL; required for URLRef with `vault:` prefix — otherwise resolution
// fails in Notify.
func NewWebhookNotifier(cfg WebhookConfig, vault VaultReader, logger *slog.Logger) (*WebhookNotifier, error) {
	if strings.TrimSpace(cfg.URLRef) == "" {
		return nil, errors.New("toll.NewWebhookNotifier: empty URLRef")
	}
	if logger == nil {
		return nil, errors.New("toll.NewWebhookNotifier: nil logger")
	}
	if cfg.Format == "" {
		cfg.Format = "generic"
	}
	switch cfg.Format {
	case "generic", "pagerduty_v2", "slack":
	default:
		return nil, fmt.Errorf("toll.NewWebhookNotifier: unsupported format %q", cfg.Format)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultWebhookTimeout
	}
	if strings.HasPrefix(cfg.URLRef, vaultRefPrefix) && vault == nil {
		return nil, errors.New("toll.NewWebhookNotifier: URLRef has vault: prefix but VaultReader is nil")
	}
	return &WebhookNotifier{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		vault:  vault,
		logger: logger,
	}, nil
}

// Notify — best-effort POST. Errors logged, not returned to caller.
//
// Steps:
//  1. URL resolution (Vault or inline).
//  2. Payload assembly per [WebhookConfig.Format] (including routing_key which
//     for PagerDuty pulled from same KV under `routing_key` field).
//  3. POST with cfg.Timeout.
//  4. Non-2xx → error log (with body trimmed to 256 bytes for diagnostics).
func (n *WebhookNotifier) Notify(ctx context.Context, event TollEvent) {
	if n == nil {
		return
	}
	url, extra, err := n.resolveSecret(ctx)
	if err != nil {
		n.logger.Error("toll.webhook: resolve URL failed",
			slog.Any("error", err),
			slog.String("event_type", event.Type))
		return
	}
	payload, err := buildPayload(n.cfg.Format, event, extra)
	if err != nil {
		n.logger.Error("toll.webhook: build payload failed",
			slog.Any("error", err),
			slog.String("format", n.cfg.Format))
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		n.logger.Error("toll.webhook: build request failed",
			slog.Any("error", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "soul-stack-keeper/toll")

	resp, err := n.client.Do(req)
	if err != nil {
		// Transport / DNS / timeout — Warn log (not Error: webhook temporarily
		// unavailable — not reason to page self-monitoring).
		n.logger.Warn("toll.webhook: POST failed",
			slog.Any("error", err),
			slog.String("format", n.cfg.Format),
			slog.String("event_type", event.Type))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		n.logger.Warn("toll.webhook: non-2xx response",
			slog.Int("status", resp.StatusCode),
			slog.String("body", string(body)),
			slog.String("format", n.cfg.Format))
		return
	}
	// Drain remaining body for connection-reuse.
	_, _ = io.Copy(io.Discard, resp.Body)

	n.logger.Debug("toll.webhook: delivered",
		slog.String("event_type", event.Type),
		slog.String("format", n.cfg.Format),
		slog.Int("status", resp.StatusCode))
}

// resolveSecret returns (url, extra-fields, err). For inline URL — extra
// nil. For vault-ref — KV-map (caller extracts routing_key for PagerDuty).
func (n *WebhookNotifier) resolveSecret(ctx context.Context) (string, map[string]any, error) {
	if !strings.HasPrefix(n.cfg.URLRef, vaultRefPrefix) {
		return n.cfg.URLRef, nil, nil
	}
	path := strings.TrimPrefix(n.cfg.URLRef, vaultRefPrefix)
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", nil, fmt.Errorf("empty path in URLRef %q", n.cfg.URLRef)
	}
	kv, err := n.vault.ReadKV(ctx, path)
	if err != nil {
		return "", nil, fmt.Errorf("vault read %q: %w", path, err)
	}
	rawURL, ok := kv[urlFieldName].(string)
	if !ok || rawURL == "" {
		return "", nil, fmt.Errorf("vault KV %q: missing or empty field %q", path, urlFieldName)
	}
	return rawURL, kv, nil
}

// buildPayload serializes event to JSON per format.
//
// Generic — flat snake_case (event_type/leader_kid/rate/...) + opt. coven_name.
// PagerDuty v2 — format `https://developer.pagerduty.com/docs/events-api-v2`:
// dedup_key=`cluster:degraded` (same for set/clear → trigger+resolve
// in one incident), event_action=trigger/resolve.
// Slack — incoming webhook: text + attachment with color (red/green) and fields.
func buildPayload(format string, ev TollEvent, vaultKV map[string]any) ([]byte, error) {
	switch format {
	case "generic":
		return marshalGeneric(ev)
	case "pagerduty_v2":
		return marshalPagerDuty(ev, vaultKV)
	case "slack":
		return marshalSlack(ev)
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

func marshalGeneric(ev TollEvent) ([]byte, error) {
	body := map[string]any{
		"event_type":         ev.Type,
		"leader_kid":         ev.LeaderKID,
		"rate":               ev.Rate,
		"baseline_connected": ev.BaselineConnected,
		"threshold":          ev.Threshold,
		"window_seconds":     ev.WindowSeconds,
		"timestamp":          ev.Timestamp.Format(time.RFC3339),
	}
	if ev.CovenName != "" {
		body["coven_name"] = ev.CovenName
	}
	return json.Marshal(body)
}

// marshalPagerDuty — Events API v2 enqueue schema. `routing_key` is
// integration-key, read from vault KV (`routing_key` field); for inline-URL
// or missing field — empty string (PagerDuty rejects with 400, operator learns
// from non-2xx log — better to fail loudly than silently drop
// event to /dev/null).
func marshalPagerDuty(ev TollEvent, vaultKV map[string]any) ([]byte, error) {
	routingKey := ""
	if vaultKV != nil {
		if rk, ok := vaultKV["routing_key"].(string); ok {
			routingKey = rk
		}
	}
	action := "trigger"
	severity := "error"
	summary := fmt.Sprintf("Soul Stack cluster degraded (leader=%s, rate=%.2f, threshold=%.2f)",
		ev.LeaderKID, ev.Rate, ev.Threshold)
	if ev.Type == EventTypeDegradedCleared {
		action = "resolve"
		severity = "info"
		summary = fmt.Sprintf("Soul Stack cluster recovered (leader=%s, rate=%.2f)", ev.LeaderKID, ev.Rate)
	}
	customDetails := map[string]any{
		"leader_kid":         ev.LeaderKID,
		"rate":               ev.Rate,
		"baseline_connected": ev.BaselineConnected,
		"threshold":          ev.Threshold,
		"window_seconds":     ev.WindowSeconds,
	}
	if ev.CovenName != "" {
		customDetails["coven_name"] = ev.CovenName
	}
	body := map[string]any{
		"routing_key":  routingKey,
		"event_action": action,
		"dedup_key":    "soul-stack/cluster:degraded",
		"payload": map[string]any{
			"summary":        summary,
			"source":         ev.LeaderKID,
			"severity":       severity,
			"timestamp":      ev.Timestamp.Format(time.RFC3339),
			"component":      "toll",
			"group":          "soul-stack",
			"class":          "cluster-availability",
			"custom_details": customDetails,
		},
	}
	return json.Marshal(body)
}

// marshalSlack — incoming webhook schema (`https://api.slack.com/messaging/webhooks`).
// Color attachment: red for set, green for cleared.
func marshalSlack(ev TollEvent) ([]byte, error) {
	color := "danger"
	text := fmt.Sprintf(":rotating_light: *Soul Stack cluster degraded* (leader=`%s`)", ev.LeaderKID)
	if ev.Type == EventTypeDegradedCleared {
		color = "good"
		text = fmt.Sprintf(":white_check_mark: *Soul Stack cluster recovered* (leader=`%s`)", ev.LeaderKID)
	}
	fields := []map[string]any{
		{"title": "Rate", "value": fmt.Sprintf("%.2f", ev.Rate), "short": true},
		{"title": "Threshold", "value": fmt.Sprintf("%.2f", ev.Threshold), "short": true},
		{"title": "Baseline", "value": fmt.Sprintf("%d", ev.BaselineConnected), "short": true},
		{"title": "Window", "value": fmt.Sprintf("%ds", ev.WindowSeconds), "short": true},
	}
	if ev.CovenName != "" {
		fields = append(fields, map[string]any{
			"title": "Coven",
			"value": ev.CovenName,
			"short": true,
		})
	}
	body := map[string]any{
		"text": text,
		"attachments": []map[string]any{
			{
				"color":  color,
				"fields": fields,
				"ts":     ev.Timestamp.Unix(),
			},
		},
	}
	return json.Marshal(body)
}
