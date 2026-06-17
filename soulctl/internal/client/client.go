// Package client — тонкий HTTP-клиент к Operator API Keeper-а.
//
// Контракт API — docs/keeper/operator-api.md + docs/keeper/openapi.yaml. Здесь
// только транспорт + типизированные обёртки `Incarnations`/`Souls` для команд
// soulctl. Бизнес-логика (форматирование, валидация) — на стороне cmd-пакета.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/soulctl/internal/config"
)

// HTTPDoer — минимальный интерфейс над http.Client для подмены в тестах
// (httptest.NewServer возвращает реальный *http.Client; мок-Doer полезнее
// для unit-тестов команд soulctl).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client — обёртка над HTTPDoer с base URL Keeper-а и JWT-bearer header.
type Client struct {
	baseURL string
	jwt     string
	http    HTTPDoer

	Incarnations  *IncarnationsAPI
	Souls         *SoulsAPI
	Archon        *ArchonAPI
	Errand        *ErrandAPI
	Voyages       *VoyagesAPI
	PushProviders *PushProvidersAPI
	Push          *PushAPI
}

// New собирает клиент из credentials. Базовый URL нормализуется (убирается
// trailing slash). Возвращает осмысленную ошибку, если credentials неполные.
func New(c *config.Credentials) (*Client, error) {
	if c == nil || c.KeeperURL == "" || c.ArchonJWT == "" {
		return nil, errors.New("credentials пусты")
	}
	u, err := url.Parse(c.KeeperURL)
	if err != nil {
		return nil, fmt.Errorf("разобрать keeper_url %q: %w", c.KeeperURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("keeper_url %q должен быть полным URL (scheme + host)", c.KeeperURL)
	}
	cl := &Client{
		baseURL: strings.TrimRight(c.KeeperURL, "/"),
		jwt:     c.ArchonJWT,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	cl.bindAPIs()
	return cl, nil
}

// NewWithDoer — для тестов: даёт подменить транспорт (httptest или мок).
// Валидацию URL/JWT повторяет, чтобы поведение совпадало с New.
func NewWithDoer(baseURL, jwt string, doer HTTPDoer) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("baseURL пуст")
	}
	cl := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		jwt:     jwt,
		http:    doer,
	}
	cl.bindAPIs()
	return cl, nil
}

func (c *Client) bindAPIs() {
	c.Incarnations = &IncarnationsAPI{c: c}
	c.Souls = &SoulsAPI{c: c}
	c.Archon = &ArchonAPI{c: c}
	c.Errand = &ErrandAPI{c: c}
	c.Voyages = &VoyagesAPI{c: c}
	c.PushProviders = &PushProvidersAPI{c: c}
	c.Push = &PushAPI{c: c}
}

// APIError — структурированная ошибка HTTP-ответа Keeper-а. Несёт HTTP-статус,
// тип/title/detail из RFC 7807 ProblemDetails (если ответ — application/problem+json).
type APIError struct {
	Status  int    `json:"status"`
	Type    string `json:"type,omitempty"`
	Title   string `json:"title,omitempty"`
	Detail  string `json:"detail,omitempty"`
	RawBody string `json:"-"`
	Method  string `json:"-"`
	Path    string `json:"-"`
}

// Error — человекочитаемое представление; команды поверх форматируют 401/403/404
// в типовые подсказки (см. internal/cmd/errors.go).
func (e *APIError) Error() string {
	switch {
	case e.Detail != "":
		return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, e.Title, e.Detail)
	case e.Title != "":
		return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, e.Title)
	case e.RawBody != "":
		return fmt.Sprintf("%s %s: %d: %s", e.Method, e.Path, e.Status, e.RawBody)
	default:
		return fmt.Sprintf("%s %s: %d", e.Method, e.Path, e.Status)
	}
}

// AsAPIError — удобный switch для cmd-слоя: errors.As + проверка статуса.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// Do выполняет HTTP-запрос с авторизацией и JSON-сериализацией body (если не nil).
// При HTTP-статусе ≥400 возвращает *APIError (RFC 7807 распарсен по возможности).
//
// `out` — указатель на структуру для декодинга JSON-ответа; nil = ответ
// игнорируется. Если body пустое (204 No Content) и out != nil — ошибки нет,
// out остаётся zero-value.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("сериализовать запрос: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("собрать запрос: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	req.Header.Set("Accept", "application/json, application/problem+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		apiErr := &APIError{
			Status:  resp.StatusCode,
			RawBody: strings.TrimSpace(string(payload)),
			Method:  method,
			Path:    path,
		}
		// RFC 7807 — application/problem+json. Если парсинг не удался, оставим RawBody.
		_ = json.Unmarshal(payload, apiErr)
		return apiErr
	}
	if out != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("разобрать ответ %s %s: %w", method, path, err)
		}
	}
	return nil
}

// JWT возвращает текущий JWT клиента (для archon whoami — декодирование claims).
func (c *Client) JWT() string { return c.jwt }

// BaseURL возвращает базовый URL (для дианостики в whoami).
func (c *Client) BaseURL() string { return c.baseURL }
