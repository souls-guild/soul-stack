package herald

// HTTP-класс драйверы каналов Herald (ADR-052 amendment): webhook / telegram /
// slack / mattermost / discord / custom. Каждый реализует [channelDriver]:
// validateConfig (по своему [HeraldFieldSpec]-дескриптору + доменные инварианты),
// secretRequired (только webhook), resolveDelivery (готовый [httpDelivery]).
//
// Мессенджеры (telegram/slack/mattermost/discord) шлют ЧЕЛОВЕКОЧИТАЕМЫЙ текст
// ([messageText] — event_type + herald/tiding + компактный payload-digest), а не
// сырой webhookPayload. webhook/custom шлют структурный webhookPayload
// (projection/annotations). Секрет (bot_token/webhook_url) — vault-ref внутри
// config, резолвится из Vault на доставке ([resolveVaultString]); в текст ошибок
// не утекает.

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
	// telegramAPIBase — фиксированный базовый URL Bot API (публичный → SSRF-guard
	// тривиально проходит). Вынесен константой ради теста собранного URL.
	telegramAPIBase = "https://api.telegram.org"

	// discordContentLimit — жёсткий лимит поля content Discord webhook (2000 симв.,
	// иначе 400 от API). Текст обрезается до лимита.
	discordContentLimit = 2000

	// userAgent — единый User-Agent исходящих HTTP-доставок Herald.
	userAgent = "soul-stack-keeper/herald"
)

// --- webhook ------------------------------------------------------------------

// webhookChannel — драйвер webhook-канала. ПОВЕДЕНИЕ бит-в-бит прежнее (тот же
// [webhookTarget]-резолв, тот же [webhookPayload], тот же [SignatureHeader], те
// же opt-out-флаги). Единственный тип с top-level secret_ref (HMAC-подпись).
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
		// Битый config / Vault-сбой signing-token-а. Vault-сбой может быть
		// транзиентным — оставляем retry (НЕ terminal-no-retry).
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

// telegramChannel — драйвер telegram-канала. URL — https://api.telegram.org/
// bot<token>/sendMessage (токен из config.bot_token_ref через Vault), тело —
// {chat_id, text[, parse_mode]}. Endpoint публичный → opt-out-флаги false.
type telegramChannel struct{}

// telegramMessage — тело POST sendMessage. parse_mode опускается при plain.
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

	// Токен бота — секрет через Vault; Vault-сбой транзиентен (retry).
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

// slackChannel — драйвер Slack incoming-webhook. URL целиком секрет (содержит
// токен) → config.webhook_url_ref из Vault. Тело — {text}.
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

// mattermostChannel — драйвер Mattermost incoming-webhook (Slack-совместимое
// тело). URL секрет (config.webhook_url_ref). Опц. channel/username переопределяют
// назначение/имя отправителя.
type mattermostChannel struct{}

// mattermostMessage — тело POST incoming-webhook Mattermost. channel/username
// опускаются при пустых (omitempty).
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

// discordChannel — драйвер Discord-webhook. URL секрет (config.webhook_url_ref).
// Тело — {content[, username]}; content <= 2000 символов (обрезается).
type discordChannel struct{}

// discordMessage — тело POST Discord-webhook. username опускается при пустом.
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

// customChannel — драйвер произвольного HTTP-endpoint-а с ФИКСИРОВАННЫМ телом
// (webhookPayload — как webhook, с projection/annotations). НЕТ произвольного
// body_template (конфликт ADR-052(h) — форма тела фиксирована). Отличия от
// webhook: (1) настраиваемый метод (POST/PUT/PATCH), (2) опц. header_secret_ref —
// значение из Vault кладётся в Authorization (bearer-стиль), (3) НЕ использует
// top-level secret_ref (нет HMAC-подписи).
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

	// header_secret_ref (опц.): значение из Vault → Authorization. Vault-сбой
	// транзиентен (retry). Копируем headers, чтобы не мутировать общий config-map.
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

// --- общие хелперы HTTP-класса ------------------------------------------------

// messageText собирает человекочитаемую сводку события для мессенджеров
// (telegram/slack/mattermost/discord). Форма: event_type + tiding/herald + время
// + компактный payload-digest (уже замаскированный/суженный projection-ом через
// [buildPayload], инвариант A ADR-027). Текст plain — форматирование (parse_mode)
// решает оператор.
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

// payloadDigest сериализует payload-часть события в компактную строку (тот же
// маскинг/projection-контур, что [buildPayload], но без webhook-обёртки). Пустой
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

// resolveWebhookURLRef резолвит секретный webhook-URL канала (slack/mattermost/
// discord) из config.webhook_url_ref через Vault. Отсутствие ref в config
// (изменён после create) → terminal-no-retry; Vault-сбой → transient (retry).
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

// truncateRunes обрезает s до max рун (не байт — чтобы не разрубить UTF-8).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// configString читает непустую строку из config. ok=false — отсутствует, не
// строка или пуста.
func configString(config map[string]any, key string) (string, bool) {
	s, ok := config[key].(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// buildHTTPRequest строит *http.Request из httpDelivery: метод (пуст → POST),
// тело, Content-Type (пуст → application/json), заголовки канала, User-Agent и
// опц. HMAC-подпись (signingKey). Единый билдер для всех HTTP-типов; зовётся из
// [DeliveryWorker.deliver] ПОСЛЕ SSRF-guard. Заголовки канала выставляются ПОСЛЕ
// служебных — оператор может переопределить Content-Type/User-Agent при нужде.
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
