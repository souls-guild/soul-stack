package herald

// Двухклассовая транспортная модель доставки Herald (ADR-052 amendment, вердикт
// architect). HTTP-класс (webhook/telegram/…): ЕДИНЫЙ SSRF-guarded транспорт
// ([guardedDeliveryClient]+[validateDeliveryEndpoint], egress.go) + per-type
// request-builder ([HeraldTransport]). SMTP-класс (email) появится отдельным
// слайсом своей веткой в [DeliveryWorker.deliver] — здесь только HTTP-класс.
//
// Точка расширения — граница deliver(): tr := transportFor(type); dr :=
// tr.BuildRequest(...); ЕДИНЫЙ guard(dr.req.URL, dr.opt-out); client.Do(dr.req).
// Один security-контур для всех HTTP-типов by construction — новый тип НЕ может
// обойти SSRF-guard, т.к. deliver() зовёт guard сам, а не транспорт.
//
// NB(имя на подтверждении PM/юзера, propose-and-wait): HeraldTransport
// предложено architect; в naming-rules пока НЕ фиксируется.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// telegramAPIBase — фиксированный базовый URL Bot API. Публичный хост → SSRF-
// guard тривиально проходит (httpAllowed/allowPrivate=false). Вынесен константой
// ради теста (проверка собранного URL) и единственной точки правки.
const telegramAPIBase = "https://api.telegram.org"

// deliveryRequest — готовый HTTP-request доставки + per-type SSRF-opt-out-флаги
// для ЕДИНОГО guard в [DeliveryWorker.deliver]. Транспорт строит request и
// сообщает, какие opt-out взведены (webhook — из config; telegram — оба false).
type deliveryRequest struct {
	req          *http.Request
	httpAllowed  bool
	allowPrivate bool
}

// HeraldTransport — per-type request-builder HTTP-класса (имя на подтверждении).
// BuildRequest резолвит канал в готовый *http.Request (URL + метод + тело +
// заголовки + опц. подпись) и opt-out-флаги для SSRF-guard. Ошибки резолва
// секрета пробрасываются как есть (terminal/transient классификацию сохраняет
// caller: Vault-сбой transient, битый config — terminal). Сам SSRF-guard и
// client.Do — НЕ забота транспорта (единый контур в deliver()).
type HeraldTransport interface {
	BuildRequest(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*deliveryRequest, error)
}

// transportFor возвращает транспорт HTTP-класса для типа. ok=false — тип не
// HTTP-класса (или неизвестен): caller (deliver) трактует как terminal-fail
// (нет транспорта — доставлять нечем). SMTP-класс (email) сюда не попадёт —
// у deliver() будет отдельная ветка.
func transportFor(t HeraldType) (HeraldTransport, bool) {
	switch t {
	case HeraldWebhook:
		return webhookTransport{}, true
	case HeraldTelegram:
		return telegramTransport{}, true
	default:
		return nil, false
	}
}

// webhookTransport — request-builder webhook-канала. ПОВЕДЕНИЕ бит-в-бит прежнее
// (тот же [webhookTarget]-резолв, тот же [webhookPayload], тот же
// [SignatureHeader], те же opt-out-флаги) — рефактор прежней inline-логики
// deliver() без изменения контракта webhook.
type webhookTransport struct{}

func (webhookTransport) BuildRequest(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*deliveryRequest, error) {
	target, err := resolveWebhook(ctx, h, kv)
	if err != nil {
		// Битый config / Vault-сбой signing-token-а. Vault-сбой может быть
		// транзиентным — оставляем retry (НЕ terminal-no-retry).
		return nil, err
	}

	body, err := buildPayload(job)
	if err != nil {
		return nil, errTerminalNoRetry{err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.url, bytes.NewReader(body))
	if err != nil {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: build request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "soul-stack-keeper/herald")
	for k, v := range target.headers {
		req.Header.Set(k, v)
	}
	if target.signingKey != nil {
		req.Header.Set(SignatureHeader, signBody(target.signingKey, body))
	}
	return &deliveryRequest{req: req, httpAllowed: target.httpAllowed, allowPrivate: target.allowPrivate}, nil
}

// telegramTransport — request-builder telegram-канала (HTTP-класс, эталон
// паттерна для будущих типов). URL — https://api.telegram.org/bot<token>/
// sendMessage (токен из config.bot_token_ref через Vault), тело —
// {chat_id, text[, parse_mode]}. Endpoint публичный → opt-out-флаги false, SSRF-
// guard в deliver() тривиально проходит. Секрет (токен) в URL — request не
// логируется, ошибки резолва маскируются caller-ом.
type telegramTransport struct{}

// telegramMessage — тело POST sendMessage. text — человекочитаемая сводка
// события ([telegramText]); parse_mode опускается при plain (omitempty).
type telegramMessage struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

func (telegramTransport) BuildRequest(ctx context.Context, h *Herald, job *DeliveryJob, kv KVReader) (*deliveryRequest, error) {
	tokenRef, _ := h.Config["bot_token_ref"].(string)
	if tokenRef == "" {
		// Config провалидирован на CRUD, но канал мог быть изменён — устойчивая
		// ошибка конфигурации, ретраить бессмысленно.
		return nil, errTerminalNoRetry{fmt.Errorf("herald: telegram channel %q has no bot_token_ref", h.Name)}
	}
	chatID, _ := h.Config["chat_id"].(string)
	if chatID == "" {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: telegram channel %q has no chat_id", h.Name)}
	}
	parseMode, _ := h.Config["parse_mode"].(string)

	// Токен бота — секрет через Vault (bot_token_ref). Vault-сбой транзиентен
	// (retry), как webhook signing-token: resolveSigningKey не оборачивает в
	// terminal-no-retry.
	token, err := resolveSigningKey(ctx, kv, tokenRef)
	if err != nil {
		return nil, err
	}

	msg := telegramMessage{ChatID: chatID, Text: telegramText(job), ParseMode: parseMode}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: marshal telegram message: %w", err)}
	}

	url := telegramAPIBase + "/bot" + string(token) + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, errTerminalNoRetry{fmt.Errorf("herald: build telegram request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "soul-stack-keeper/herald")
	// Публичный API telegram — SSRF opt-out не взводим (guard в deliver проходит).
	return &deliveryRequest{req: req, httpAllowed: false, allowPrivate: false}, nil
}

// telegramText собирает человекочитаемую сводку события для тела sendMessage.
// Форма: заголовок event_type + herald/tiding + компактный payload-JSON (уже
// замаскированный: MaskSecrets применён на выходе, инвариант A ADR-027). Payload
// сериализуется в одну строку — Bot API принимает plain-text; форматирование
// (MarkdownV2/HTML) — забота оператора через parse_mode, здесь текст plain.
func telegramText(job *DeliveryJob) string {
	var b strings.Builder
	b.WriteString(string(job.EventType))
	if job.Herald != "" || job.Tiding != "" {
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
	if payload := telegramPayloadDigest(job); payload != "" {
		b.WriteString("\n")
		b.WriteString(payload)
	}
	return b.String()
}

// telegramPayloadDigest сериализует payload события в компактную строку для
// текста telegram. Применяет тот же маскинг/projection-контур, что [buildPayload]
// (секрет-гигиена), но берёт ТОЛЬКО payload-часть (без webhook-обёртки). Пустой
// payload → "".
func telegramPayloadDigest(job *DeliveryJob) string {
	body, err := buildPayload(job)
	if err != nil {
		return ""
	}
	var wp webhookPayload
	if err := json.Unmarshal(body, &wp); err != nil {
		return ""
	}
	if len(wp.Payload) == 0 {
		return ""
	}
	digest, err := json.Marshal(wp.Payload)
	if err != nil {
		return ""
	}
	return string(digest)
}
