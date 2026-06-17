package augur

// ELK-broker MVP-1 (delegate=false, augur.md §2.1 / §6): по уже РАЗРЕШЁННОМУ
// запросу ([Resolve] вернул Decision{Allowed:true}) Keeper сам делает read-only
// search в Elasticsearch-индекс Omen-а и заворачивает JSON-ответ в
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

// elkSearchSuffix — read-only search-endpoint Elasticsearch. Брокер делает
// GET <endpoint>/<index>/_search (read-only); index-write / admin-API не
// используются.
const elkSearchSuffix = "/_search"

// BrokerELK делает read-only search по разрешённому индексу Omen-а и возвращает
// JSON-ответ как Struct (augur.md §5.3, объект «как есть»).
//
// index (decision.Query) уже прошёл exact-match против Rite.allow.indices в
// Resolve — здесь не переавторизуется. endpoint — НЕдоверенный ввод из БД:
// validateEndpoint (https-only + литеральный IP в block-list) + SSRF-guard на
// dial-фазе клиента закрывают egress-вектор.
//
// credential читается из Omen.AuthRef (Vault) и навешивается на запрос; на Soul
// не уходит. Сбой → error (AugurReply{ERROR}); ни credential, ни тело ответа в
// текст ошибки НЕ попадают.
func BrokerELK(ctx context.Context, kv KVReader, doer HTTPDoer, endpoint, authRef, index string) (*structpb.Struct, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	cred, err := resolveCredential(ctx, kv, authRef)
	if err != nil {
		return nil, err
	}

	target, err := buildELKURL(endpoint, index)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("augur: build elk request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	cred.apply(req)

	return doJSONStruct(doer, req)
}

// buildELKURL собирает URL search: <endpoint>/<index>/_search. index экранируется
// через url.PathEscape (НЕдоверенное значение из allow-list; path-injection через
// `../` / лишний слэш исключён). Trailing slash endpoint-а нормализуется.
func buildELKURL(endpoint, index string) (string, error) {
	base := strings.TrimRight(endpoint, "/")
	target := base + "/" + url.PathEscape(index) + elkSearchSuffix
	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("augur: build elk url: %w", err)
	}
	return u.String(), nil
}
