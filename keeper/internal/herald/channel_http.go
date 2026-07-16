package herald

// HTTP-class channel drivers for Herald (ADR-052 amendment): webhook / telegram /
// slack / mattermost / discord / custom. Each implements [channelDriver]:
// validateConfig (per its [HeraldFieldSpec] descriptor + domain invariants),
// secretRequired (webhook only), resolveDelivery (ready [httpDelivery]).
//
// Messengers (telegram/slack/mattermost/discord) send HUMAN-READABLE text
// ([messageText] — event_type + herald/tiding + compact payload-digest), not
// raw webhookPayload. webhook/custom send structured webhookPayload
// (projection/annotations). Secret (bot_token/webhook_url) is vault-ref inside
// config, resolved from Vault at delivery ([resolveVaultString]); does not leak
// into error text.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// telegramAPIBase is fixed base URL for Bot API (public → SSRF-guard trivially passes).
	// Extracted as constant for testing the built URL.
	telegramAPIBase = "https://api.telegram.org"

	// discordContentLimit is hard limit for Discord webhook content field (2000 chars,
	// otherwise 400 from API). Text is truncated to limit.
	discordContentLimit = 2000

	// userAgent is the unified User-Agent for outgoing Herald HTTP deliveries.
	userAgent = "soul-stack-keeper/herald"
)

// --- webhook ------------------------------------------------------------------

// webhookChannel is the driver for webhook channel. BEHAVIOR is bit-for-bit previous
// (same [webhookTarget]-resolution, same [webhookPayload], same [SignatureHeader],
// same opt-out flags). Only type with top-level secret_ref (HMAC signature).
type webhookChannel struct{}

func (webhookChannel) fields() []HeraldFieldSpec {
	return []HeraldFieldSpec{
		{Name: "url", Label: "URL", Required: true, Kind: KindURL},
		{Name: "headers", Label: "HTTP-заголовки", Kind: KindMap},
		{Name: "http_allowed", Label: "Разрешить http://", Kind: KindBool},
		{Name: "allow_private", Label: "Разрешить приватные IP", Kind: KindBool},
	}
}

func (c webhookChannel) secretRequired() bool { return true }

func (c webhookChannel) validateConfig(config map[string]any) error {
	if err := validateBySpec(HeraldWebhook, c.fields(), config); err != nil {
		return err
	}
	return validateWebhookURL(config)
}

func (webhookChannel) resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error) {
	target, err := resolveWebhookTarget(ctx, h, kv)
	if err != nil {
		// Broken config / Vault failure for signing-token. Vault failure can be
		// transient — leaving retry (NOT terminal-no-retry).
		return nil, err
	}
	body, err := buildPayload(job)
	if err != nil {
		return nil, errTerminalNoRetry{err}
	}
	return &httpDelivery{
		url:          target.url,
		method:       http.MethodPost,
		contentType:  "application/json",
		body:         body,
		headers:      target.headers,
		httpAllowed:  target.httpAllowed,
		allowPrivate: target.allowPrivate,
		signingKey:   target.signingKey,
	}, nil
}

// --- telegram -----------------------------------------------------------------

// telegramChannel is the driver for telegram channel. URL is https://api.telegram.org/
// bot<token>/sendMessage (token from config.bot_token_ref via Vault), body is
// {chat_id, text[, parse_mode]}. Endpoint is public → opt-out flags false.
type telegramChannel struct{}

// telegramMessage is POST body for sendMessage. parse_mode is omitted for plain.
type telegramMessage struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

func (telegramChannel) fields() []HeraldFieldSpec {
	return []HeraldFieldSpec{
		{Name: "bot_token_ref", Label: "Vault-ref токена бота", Required: true, Secret: true, Kind: KindVaultRef},
		{Name: "chat_id", Label: "ID чата/канала", Required: true, Kind: KindString},
		{Name: "parse_mode", Label: "Формат текста", Kind: KindEnum, EnumValues: []string{"", "MarkdownV2", "HTML"}},
	}
}

func (telegramChannel) secretRequired() bool { return false }

func (c telegramChannel) validateConfig(config map[string]any) error {
	if err := validateBySpec(HeraldTelegram, c.fields(), config); err != nil {
		return err
	}
	// chat_id непустой после трима: пробельный chat_id — мёртвый адрес.
	if chatID, _ := config["chat_id"].(string); strings.TrimSpace(chatID) == "" {
		return fmt.Errorf("herald: telegram config %q must be a non-empty chat id", "chat_id")
	}
	return nil
}

func (telegramChannel) resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error) {
	chatID, ok := configString(h.Config, "chat_id")
	if !ok {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: telegram channel %q has no chat_id", h.Name)}
	}
	tokenRef, ok := configString(h.Config, "bot_token_ref")
	if !ok {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: telegram channel %q has no bot_token_ref", h.Name)}
	}
	parseMode, _ := h.Config["parse_mode"].(string)

	// Bot token is secret via Vault; Vault failure is transient (retry).
	token, err := resolveVaultString(ctx, kv, tokenRef)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(telegramMessage{ChatID: chatID, Text: messageText(job), ParseMode: parseMode})
	if err != nil {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: marshal telegram message: %w", err)}
	}
	url := telegramAPIBase + "/bot" + token + "/sendMessage"
	return &httpDelivery{url: url, method: http.MethodPost, contentType: "application/json", body: body}, nil
}

// --- slack --------------------------------------------------------------------

// slackChannel is the driver for Slack incoming-webhook. URL is entirely secret
// (contains token) → config.webhook_url_ref from Vault. Body is {text}.
type slackChannel struct{}

func (slackChannel) fields() []HeraldFieldSpec {
	return []HeraldFieldSpec{
		{Name: "webhook_url_ref", Label: "Vault-ref URL incoming-webhook", Required: true, Secret: true, Kind: KindVaultRef},
	}
}

func (slackChannel) secretRequired() bool { return false }

func (c slackChannel) validateConfig(config map[string]any) error {
	return validateBySpec(HeraldSlack, c.fields(), config)
}

func (slackChannel) resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error) {
	url, err := resolveWebhookURLRef(ctx, h, kv)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"text": messageText(job)})
	if err != nil {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: marshal slack message: %w", err)}
	}
	return &httpDelivery{url: url, method: http.MethodPost, contentType: "application/json", body: body}, nil
}

// --- mattermost ---------------------------------------------------------------

// mattermostChannel is the driver for Mattermost incoming-webhook (Slack-compatible
// body). URL is secret (config.webhook_url_ref). Optional channel/username override
// destination/sender name.
type mattermostChannel struct{}

// mattermostMessage is POST body for Mattermost incoming-webhook. channel/username
// are omitted when empty (omitempty).
type mattermostMessage struct {
	Text     string `json:"text"`
	Channel  string `json:"channel,omitempty"`
	Username string `json:"username,omitempty"`
}

func (mattermostChannel) fields() []HeraldFieldSpec {
	return []HeraldFieldSpec{
		{Name: "webhook_url_ref", Label: "Vault-ref URL incoming-webhook", Required: true, Secret: true, Kind: KindVaultRef},
		{Name: "channel", Label: "Канал (override)", Kind: KindString},
		{Name: "username", Label: "Имя отправителя (override)", Kind: KindString},
	}
}

func (mattermostChannel) secretRequired() bool { return false }

func (c mattermostChannel) validateConfig(config map[string]any) error {
	return validateBySpec(HeraldMattermost, c.fields(), config)
}

func (mattermostChannel) resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error) {
	url, err := resolveWebhookURLRef(ctx, h, kv)
	if err != nil {
		return nil, err
	}
	channel, _ := h.Config["channel"].(string)
	username, _ := h.Config["username"].(string)
	body, err := json.Marshal(mattermostMessage{Text: messageText(job), Channel: channel, Username: username})
	if err != nil {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: marshal mattermost message: %w", err)}
	}
	return &httpDelivery{url: url, method: http.MethodPost, contentType: "application/json", body: body}, nil
}

// --- discord ------------------------------------------------------------------

// discordChannel is the driver for Discord webhook. URL is secret (config.webhook_url_ref).
// Body is {content[, username]}; content <= 2000 chars (truncated).
type discordChannel struct{}

// discordMessage is POST body for Discord webhook. username is omitted when empty.
type discordMessage struct {
	Content  string `json:"content"`
	Username string `json:"username,omitempty"`
}

func (discordChannel) fields() []HeraldFieldSpec {
	return []HeraldFieldSpec{
		{Name: "webhook_url_ref", Label: "Vault-ref URL webhook", Required: true, Secret: true, Kind: KindVaultRef},
		{Name: "username", Label: "Имя отправителя (override)", Kind: KindString},
	}
}

func (discordChannel) secretRequired() bool { return false }

func (c discordChannel) validateConfig(config map[string]any) error {
	return validateBySpec(HeraldDiscord, c.fields(), config)
}

func (discordChannel) resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error) {
	url, err := resolveWebhookURLRef(ctx, h, kv)
	if err != nil {
		return nil, err
	}
	username, _ := h.Config["username"].(string)
	body, err := json.Marshal(discordMessage{Content: truncateRunes(messageText(job), discordContentLimit), Username: username})
	if err != nil {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: marshal discord message: %w", err)}
	}
	return &httpDelivery{url: url, method: http.MethodPost, contentType: "application/json", body: body}, nil
}

// --- custom -------------------------------------------------------------------

// customChannel is the driver for arbitrary HTTP endpoint with FIXED body
// (webhookPayload — like webhook, with projection/annotations). NO arbitrary
// body_template (conflicts with ADR-052(h) — body form is fixed). Differences from
// webhook: (1) configurable method (POST/PUT/PATCH), (2) optional header_secret_ref —
// value from Vault placed in Authorization (bearer-style), (3) does NOT use
// top-level secret_ref (no HMAC signature).
type customChannel struct{}

func (customChannel) fields() []HeraldFieldSpec {
	return []HeraldFieldSpec{
		{Name: "url", Label: "URL", Required: true, Kind: KindURL},
		{Name: "method", Label: "HTTP-метод", Kind: KindEnum, EnumValues: []string{"", "POST", "PUT", "PATCH"}},
		{Name: "headers", Label: "HTTP-заголовки", Kind: KindMap},
		{Name: "header_secret_ref", Label: "Vault-ref секрета для Authorization", Secret: true, Kind: KindVaultRef},
		{Name: "http_allowed", Label: "Разрешить http://", Kind: KindBool},
		{Name: "allow_private", Label: "Разрешить приватные IP", Kind: KindBool},
	}
}

func (customChannel) secretRequired() bool { return false }

func (c customChannel) validateConfig(config map[string]any) error {
	if err := validateBySpec(HeraldCustom, c.fields(), config); err != nil {
		return err
	}
	return validateWebhookURL(config)
}

func (customChannel) resolveDelivery(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*httpDelivery, error) {
	rawURL, ok := configString(h.Config, "url")
	if !ok {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: custom channel %q has no url", h.Name)}
	}
	body, err := buildPayload(job)
	if err != nil {
		return nil, errTerminalNoRetry{err}
	}

	method := http.MethodPost
	if m, _ := h.Config["method"].(string); m != "" {
		method = m
	}
	headers := configHeaders(h.Config)

	// header_secret_ref (optional): value from Vault → Authorization. Vault failure
	// is transient (retry). Copy headers to avoid mutating shared config map.
	if ref, ok := configString(h.Config, "header_secret_ref"); ok {
		secret, err := resolveVaultString(ctx, kv, ref)
		if err != nil {
			return nil, err
		}
		merged := make(map[string]string, len(headers)+1)
		for k, v := range headers {
			merged[k] = v
		}
		merged["Authorization"] = secret
		headers = merged
	}

	return &httpDelivery{
		url:          rawURL,
		method:       method,
		contentType:  "application/json",
		body:         body,
		headers:      headers,
		httpAllowed:  configBool(h.Config, "http_allowed"),
		allowPrivate: configBool(h.Config, "allow_private"),
	}, nil
}

// --- HTTP-class common helpers ------------------------------------------------

// messageText assembles human-readable event summary for messengers
// (telegram/slack/mattermost/discord). Form: event_type + tiding/herald + time
// + compact payload-digest (already masked/narrowed by projection via
// [buildPayload], invariant A ADR-027). Text is plain — operator decides formatting
// (parse_mode).
func messageText(job *DeliveryJob) string {
	var b strings.Builder
	b.WriteString(string(job.EventType))
	if job.Tiding != "" || job.Herald != "" {
		b.WriteString("\n")
		b.WriteString(job.Tiding)
		if job.Herald != "" {
			b.WriteString(" via ")
			b.WriteString(job.Herald)
		}
	}
	if !job.OccurredAt.IsZero() {
		b.WriteString("\n")
		b.WriteString(job.OccurredAt.UTC().Format(time.RFC3339))
	}
	if digest := payloadDigest(job); digest != "" {
		b.WriteString("\n")
		b.WriteString(digest)
	}
	return b.String()
}

// payloadDigest serializes payload part of event to compact string (same
// masking/projection circuit as [buildPayload], but without webhook wrapper). Empty
// payload → "".
func payloadDigest(job *DeliveryJob) string {
	body, err := buildPayload(job)
	if err != nil {
		return ""
	}
	var wp webhookPayload
	if err := json.Unmarshal(body, &wp); err != nil || len(wp.Payload) == 0 {
		return ""
	}
	digest, err := json.Marshal(wp.Payload)
	if err != nil {
		return ""
	}
	return string(digest)
}

// resolveWebhookURLRef resolves secret webhook URL of channel (slack/mattermost/
// discord) from config.webhook_url_ref via Vault. Absence of ref in config
// (changed after create) → terminal-no-retry; Vault failure → transient (retry).
func resolveWebhookURLRef(ctx context.Context, h *Herald, kv KVReader) (string, error) {
	ref, ok := configString(h.Config, "webhook_url_ref")
	if !ok {
		return "", errTerminalNoRetry{fmt.Errorf("herald: channel %q has no webhook_url_ref", h.Name)}
	}
	url, err := resolveVaultString(ctx, kv, ref)
	if err != nil {
		return "", err
	}
	return url, nil
}

// truncateRunes truncates s to max runes (not bytes — to not break UTF-8).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// configString reads non-empty string from config. ok=false — absent, not a string,
// or empty.
func configString(config map[string]any, key string) (string, bool) {
	s, ok := config[key].(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// buildHTTPRequest builds *http.Request from httpDelivery: method (empty → POST),
// body, Content-Type (empty → application/json), channel headers, User-Agent and
// optional HMAC signature (signingKey). Single builder for all HTTP types; called from
// [DeliveryWorker.deliver] AFTER SSRF-guard. Channel headers set AFTER service headers —
// operator may override Content-Type/User-Agent if needed.
func buildHTTPRequest(ctx context.Context, hd *httpDelivery) (*http.Request, error) {
	method := hd.method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, hd.url, bytes.NewReader(hd.body))
	if err != nil {
		return nil, err
	}
	ct := hd.contentType
	if ct == "" {
		ct = "application/json"
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("User-Agent", userAgent)
	for k, v := range hd.headers {
		req.Header.Set(k, v)
	}
	if hd.signingKey != nil {
		req.Header.Set(SignatureHeader, signBody(hd.signingKey, hd.body))
	}
	return req, nil
}
