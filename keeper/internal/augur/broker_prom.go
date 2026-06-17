package augur

// Prometheus-broker MVP-1 (delegate=false, augur.md §2.1 / §6): по уже
// РАЗРЕШЁННОМУ запросу ([Resolve] вернул Decision{Allowed:true}) Keeper сам
// делает live instant-query в Prometheus и заворачивает JSON-ответ в
// google.protobuf.Struct для AugurReply.inline_data. На Soul внешний credential
// не попадает.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// promQueryPath — endpoint instant-query Prometheus HTTP API v1. Брокер делает
// read-only instant query; range-query / admin-API не используются.
const promQueryPath = "/api/v1/query"

// BrokerPrometheus делает instant-query promQL к Prometheus-endpoint-у Omen-а и
// возвращает JSON-ответ как Struct (augur.md §5.3, объект «как есть»).
//
// promQL (decision.Query) уже прошёл exact-match против Rite.allow.queries в
// Resolve — здесь не переавторизуется. endpoint — НЕдоверенный ввод из БД:
// validateEndpoint (https-only + литеральный IP в block-list) + SSRF-guard на
// dial-фазе клиента (doer) закрывают egress-вектор.
//
// credential читается из Omen.AuthRef (Vault) и навешивается на запрос; на Soul
// он не уходит (данные текут через Keeper inline). Сбой чтения/запроса → error
// (caller отдаёт AugurReply{ERROR}); ни credential, ни тело ответа в текст
// ошибки НЕ попадают.
func BrokerPrometheus(ctx context.Context, kv KVReader, doer HTTPDoer, endpoint, authRef, promQL string) (*structpb.Struct, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	cred, err := resolveCredential(ctx, kv, authRef)
	if err != nil {
		return nil, err
	}

	target, err := buildPromURL(endpoint, promQL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("augur: build prometheus request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	cred.apply(req)

	return doJSONStruct(doer, req)
}

// buildPromURL собирает URL instant-query: <endpoint>/api/v1/query?query=<promQL>.
// promQL кодируется через url.Values (без склейки строк) — инъекция доп.
// query-параметров через содержимое promQL исключена. Trailing slash endpoint-а
// нормализуется, чтобы не получить `//api/v1/query`.
func buildPromURL(endpoint, promQL string) (string, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/") + promQueryPath)
	if err != nil {
		return "", fmt.Errorf("augur: build prometheus url: %w", err)
	}
	q := url.Values{}
	q.Set("query", promQL)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
