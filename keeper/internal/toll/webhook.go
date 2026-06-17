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

// VaultReader — узкая поверхность чтения Vault KV для резолва URLRef webhook-а
// (ADR-038 amendment, extensions). Реализация — *vault.Client.ReadKV в daemon-
// wire-up; fake в unit-тестах. Контракт: возвращает поле `url` (или path-
// специфичное поле — оператор сам кладёт url в KV под этим ключом, симметрично
// signing_key/public_key конвенциям существующих vault-ref-ов).
type VaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// urlFieldName — имя поля в Vault KV, где лежит webhook-URL. Конвенция
// «секрет = одно поле в KV под known-name»; симметрия с
// `signing_key`/`public_key`/`password` other vault-ref-ов.
const urlFieldName = "url"

// vaultRefPrefix — префикс `url_ref`-а, по которому WebhookNotifier
// различает vault-ref (резолв через VaultReader) от inline-URL.
const vaultRefPrefix = "vault:"

// WebhookNotifier — реализация [Notifier] для HTTP-POST alert-канала. Один
// экземпляр на keeper-процесс; thread-safe (HTTP-client + immutable cfg/vault).
//
// БЕЗОПАСНОСТЬ: webhook-URL — секрет (раскрывает pager-integration). Для prod
// — vault-ref; inline-URL допустим, но не рекомендуется.
//
// Lifecycle: Notify вызывается только под leader-lease-ом (single-winner,
// инвариант ADR-038). Нет ретраев — best-effort: тайм-аут / non-2xx / DNS-error
// логируется, наружу ошибка не возвращается ([Notifier]-контракт).
type WebhookNotifier struct {
	cfg    WebhookConfig
	client *http.Client
	vault  VaultReader
	logger *slog.Logger
}

// WebhookConfig — параметры [WebhookNotifier]. Резолвится daemon-ом из
// config.KeeperTollWebhook + дефолтов.
type WebhookConfig struct {
	// URLRef — `vault:<mount>/<path>` (поле `url` в KV) либо inline URL
	// (`http(s)://...`). Различение — по префиксу `vault:`.
	URLRef string
	// Format — один из [TollWebhookFormat*]-констант (config-пакет).
	// Резолвится daemon-ом: пустое → generic.
	Format string
	// Timeout — потолок одного POST-вызова. <=0 → дефолт
	// [DefaultWebhookTimeout] (10s).
	Timeout time.Duration
}

const (
	// DefaultWebhookTimeout — дефолт WebhookConfig.Timeout. Совпадает с
	// shared/config.DefaultTollWebhookTimeout; дублируется здесь, чтобы
	// пакет toll не тянул shared/config (направление зависимости
	// daemon → toll, не наоборот).
	DefaultWebhookTimeout = 10 * time.Second
)

// NewWebhookNotifier валидирует cfg и собирает notifier. vault может быть nil
// при inline-URL; обязателен при URLRef с префиксом `vault:` — иначе резолв
// упадёт в Notify.
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

// Notify — best-effort POST. Ошибки логируются, наружу не возвращаются.
//
// Шаги:
//  1. Резолв URL (Vault либо inline).
//  2. Сборка payload-а по [WebhookConfig.Format] (включая routing_key, который
//     при PagerDuty тянется из того же KV под полем `routing_key`).
//  3. POST с тайм-аутом cfg.Timeout.
//  4. Non-2xx → лог error (с body, обрезанным до 256 байт для диагностики).
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
		// Транспорт / DNS / тайм-аут — лог Warn (не Error: webhook временно
		// недоступен — не повод поднимать pager у self-monitoring-а).
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
	// Drain remaining body для connection-reuse.
	_, _ = io.Copy(io.Discard, resp.Body)

	n.logger.Debug("toll.webhook: delivered",
		slog.String("event_type", event.Type),
		slog.String("format", n.cfg.Format),
		slog.Int("status", resp.StatusCode))
}

// resolveSecret возвращает (url, extra-fields, err). Для inline URL — extra
// nil. Для vault-ref — KV-map (caller достанет routing_key для PagerDuty).
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

// buildPayload сериализует event в JSON в соответствии с format.
//
// Generic — плоский snake_case (event_type/leader_kid/rate/...) + опц. coven_name.
// PagerDuty v2 — формат `https://developer.pagerduty.com/docs/events-api-v2`:
// dedup_key=`cluster:degraded` (одинаковый для set/clear → trigger+resolve
// в одной incident), event_action=trigger/resolve.
// Slack — incoming webhook: text + attachment с цветом (red/green) и полями.
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

// marshalPagerDuty — Events API v2 enqueue schema. `routing_key` —
// integration-key, читается из vault KV (поле `routing_key`); при inline-URL
// или отсутствии поля — пустая строка (PagerDuty отвергнет 400, об этом
// узнает оператор по non-2xx-логу — лучше шумно отказать, чем тихо отдать
// событие на /dev/null).
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
// Color attachment: red для set, green для cleared.
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
