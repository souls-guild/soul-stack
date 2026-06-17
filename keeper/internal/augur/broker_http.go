package augur

// Общий HTTP-слой брокеров prom/elk (delegate=false, augur.md §2.1): выполнение
// запроса к НЕдоверенному endpoint-у Omen-а через SSRF-guarded клиент (egress.go),
// инъекция credential, прочитанного из Vault по Omen.AuthRef, чтение
// лимитированного JSON-тела. На Soul внешний credential НЕ попадает — данные
// текут через Keeper в AugurReply.inline_data.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// HTTPDoer — минимальная поверхность http-клиента, нужная брокерам prom/elk.
// Параметр broker-а для тестируемости: unit-тесты подменяют на guarded-клиент,
// бьющий в httptest-сервер, без реальной сети. Прод — [NewEgressClient]
// (SSRF-guarded). grpc-handler хранит один экземпляр и передаёт в брокеры.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// credential — разобранный из Vault-секрета материал авторизации к внешней
// системе. Ровно один вариант несёт значение (или все пусты → без auth).
// Никогда не логируется и не уходит на Soul.
type credential struct {
	bearer   string // токен Bearer (Prometheus / Grafana-style)
	apiKey   string // Elasticsearch API key (header `Authorization: ApiKey <...>`)
	username string // Basic-auth
	password string
}

// apply навешивает credential на запрос по конвенции (brokered read-доступ).
// Приоритет: bearer → apiKey → basic. Несколько одновременно заданных полей —
// невалидный секрет, но детерминированный выбор безопаснее молчаливого смешения.
func (c credential) apply(req *http.Request) {
	switch {
	case c.bearer != "":
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	case c.apiKey != "":
		req.Header.Set("Authorization", "ApiKey "+c.apiKey)
	case c.username != "":
		enc := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
		req.Header.Set("Authorization", "Basic "+enc)
	}
}

// resolveCredential читает root-credential (учётку внешней системы) из Vault по
// Omen.AuthRef и маппит KV-поля в [credential] по конвенции:
//
//	token / bearer_token → Bearer
//	api_key              → ApiKey (ELK)
//	username + password  → Basic
//
// auth_ref — всегда vault-ref (инвариант augur.md §4.1; CRUD требует
// ValidAuthRef). Пустой не ожидается, но трактуется как «без auth» (best-effort
// для внешних систем без авторизации). Credential-значения в ошибки/логи НЕ
// попадают — только vault-path (не секрет).
func resolveCredential(ctx context.Context, kv KVReader, authRef string) (credential, error) {
	if authRef == "" {
		return credential{}, nil
	}
	path, err := vault.ParseRef(authRef)
	if err != nil {
		return credential{}, fmt.Errorf("augur: omen auth_ref invalid: %w", err)
	}
	data, err := kv.ReadKV(ctx, path)
	if err != nil {
		return credential{}, fmt.Errorf("augur: read omen credential %q: %w", path, err)
	}
	return credentialFromKV(data), nil
}

// credentialFromKV извлекает auth-поля из KV-секрета. Неизвестные поля
// игнорируются. Значения берутся только если строки.
func credentialFromKV(data map[string]any) credential {
	var c credential
	c.bearer = kvString(data, "token")
	if c.bearer == "" {
		c.bearer = kvString(data, "bearer_token")
	}
	c.apiKey = kvString(data, "api_key")
	c.username = kvString(data, "username")
	c.password = kvString(data, "password")
	return c
}

func kvString(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

// doJSONStruct выполняет запрос req через guarded doer, читает лимитированное
// тело, декодирует как JSON и заворачивает в structpb.Struct для inline_data.
//
// Тело читается через io.LimitReader на maxResponseBytes (DoS-защита:
// НЕдоверенный endpoint не аллоцирует произвольно). Не-2xx → ошибка БЕЗ тела
// внешней системы в тексте (тело может нести leak / reflected-input) — только
// статус-код. SSRF/сетевой сбой dial-а приходит как ошибка doer-а (guard
// сработал в DialContext).
func doJSONStruct(doer HTTPDoer, req *http.Request) (*structpb.Struct, error) {
	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("augur: http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("augur: upstream returned status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("augur: read upstream body: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("augur: upstream body exceeds %d bytes limit", int64(maxResponseBytes))
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("augur: upstream body is not valid JSON: %w", err)
	}
	return wrapInlineData(decoded)
}

// wrapInlineData заворачивает декодированный JSON-результат в Struct по
// shape-convention (augur.md §5.3): объект (map) — натурально «как есть»;
// массив / скаляр — в единственный ключ `value` (Struct не несёт не-объект на
// верхнем уровне).
func wrapInlineData(v any) (*structpb.Struct, error) {
	if m, ok := v.(map[string]any); ok {
		s, err := structpb.NewStruct(m)
		if err != nil {
			return nil, fmt.Errorf("augur: encode upstream object: %w", err)
		}
		return s, nil
	}
	s, err := structpb.NewStruct(map[string]any{"value": v})
	if err != nil {
		return nil, fmt.Errorf("augur: encode upstream value: %w", err)
	}
	return s, nil
}
