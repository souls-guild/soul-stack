//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// Operator-API HTTP-клиент с JWT-авторизацией. Тонкая обёртка над net/http:
// инжектит `Authorization: Bearer <JWT>` и сериализует тело в JSON.
//
// Не использует `keeper-side`-пакеты api/openapi-клиента — harness обязан
// тестировать публичный контракт OpenAPI как чёрный ящик (иначе E2E деградирует
// в integration-test).

// opHTTPTimeout — deadline на один Operator-API HTTP-вызов. CreateIncarnation
// / RunScenario — async-202 (запуск scenario не блокирует), реально-долгого
// синхронного контракта в MVP нет; 30s — широкий запас от обычных 50–500 мс.
const opHTTPTimeout = 30 * time.Second

// opClient — JWT-аутентифицированный HTTP-клиент против Operator-API.
type opClient struct {
	baseURL string
	jwt     string
	client  *http.Client
}

// opClient собирает клиент с JWT первого Архонта из Stack.JWT.
func (s *Stack) opClient(t *testing.T) *opClient {
	t.Helper()
	if s.JWT == "" {
		t.Fatal("opClient: Stack.JWT пуст (NewStack не отработал bootstrap?)")
	}
	return &opClient{
		baseURL: s.KeeperHTTPURL,
		jwt:     s.JWT,
		client:  &http.Client{Timeout: opHTTPTimeout},
	}
}

// post сериализует body в JSON, кладёт `Authorization: Bearer <jwt>` и
// возвращает (body, statusCode, err).
func (c *opClient) post(ctx context.Context, path string, body any) ([]byte, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	return b, resp.StatusCode, nil
}

// get — GET с JWT, возвращает body+status. Используется AssertMetricGE
// (через прямой http.Get, минуя JWT — metrics-listener без auth в MVP) и
// будущими read-эндпоинтами.
func (c *opClient) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	return b, resp.StatusCode, nil
}
