package beacon

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// HTTPUnhealthyName — адрес core-beacon (`core.beacon.<name>`, VigilDef.check).
const HTTPUnhealthyName = beaconaddr.HTTPUnhealthy

const (
	stateHTTPHealthy   State = "healthy"
	stateHTTPUnhealthy State = "unhealthy"
)

// httpUnhealthyDefaultTimeout — таймаут одного health-GET. Короткий: beacon-тик,
// а не download (симметрично core.http.probe).
const httpUnhealthyDefaultTimeout = 30 * time.Second

// HTTPUnhealthy — core-beacon наблюдения за здоровьем HTTP-эндпоинта (ADR-030).
// Read-only: один GET, без чтения тела. State: "healthy" если статус-код входит
// в status_codes (default [200]), иначе "unhealthy". Транспортная ошибка
// (DNS/TLS/timeout/недоступен) — тоже "unhealthy" с точки зрения наблюдателя
// (это событие интереса, а не ошибка Check). Переход healthy↔unhealthy
// edge-triggered → Portent.
//
// Безопасность переиспользуется у core.http (паттерн opt-out security-vs-
// flexibility, симметрично core.http.probe / core.url): util.ValidateFetchURL +
// util.NewHTTPClient (SSRF-guard на dial-фазе, downgrade-защита редиректов,
// системный TLS trust store). Дефолт максимально безопасный (https + SSRF-guard +
// TLS-верификация); под internal-таргет (`https://127.0.0.1:8443/health`,
// RFC1918) оператор явно поднимает opt-out-флаги в VigilDef.params. data НЕ несёт
// тела/заголовков (sensitive) — только url и статус-код.
//
// warn при снятии guard здесь не нужен (в отличие от apply-модулей): beacon —
// read-probe по расписанию без output-warnings-канала; явный флаг в Vigil.params
// и есть согласие оператора.
//
// Params:
//   - `url` (string, required) — https-эндпоинт (http:// — только с allow_http);
//   - `status_codes` (list of int, optional, default [200]) — «здоровые» коды;
//   - `timeout` (string duration, optional, default "30s");
//   - `allow_http` (bool, optional, default false) — принять http:// (снимает
//     https-only и downgrade-защиту редиректов, не открывает SSRF);
//   - `insecure_skip_verify` (bool, optional, default false) — не верифицировать
//     TLS (self-signed / internal CA);
//   - `allow_private` (bool, optional, default false) — снять SSRF dial-guard
//     (loopback/RFC1918 internal-эндпоинт).
type HTTPUnhealthy struct {
	// NewClient вынесен в поле для подмены fake HTTPDoer в unit-тестах (без
	// выхода в сеть). В проде — util.NewHTTPClient.
	NewClient func(util.HTTPClientOpts) util.HTTPDoer
}

// NewHTTPUnhealthy собирает beacon с production-фабрикой HTTP-клиента.
func NewHTTPUnhealthy() *HTTPUnhealthy {
	return &HTTPUnhealthy{
		NewClient: func(opts util.HTTPClientOpts) util.HTTPDoer { return util.NewHTTPClient(opts) },
	}
}

func (b *HTTPUnhealthy) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	rawURL, err := util.StringParam(params, "url")
	if err != nil {
		return "", nil, err
	}
	allowHTTP, err := util.OptBoolParam(params, "allow_http")
	if err != nil {
		return "", nil, err
	}
	if verr := util.ValidateFetchURL(rawURL, allowHTTP); verr != nil {
		return "", nil, verr
	}
	allowPrivate, err := util.OptBoolParam(params, "allow_private")
	if err != nil {
		return "", nil, err
	}
	insecureSkipVerify, err := util.OptBoolParam(params, "insecure_skip_verify")
	if err != nil {
		return "", nil, err
	}
	wantCodes, err := util.OptIntSliceParam(params, "status_codes")
	if err != nil {
		return "", nil, err
	}
	if len(wantCodes) == 0 {
		wantCodes = []int64{http.StatusOK}
	}
	timeout, err := optBeaconTimeout(params, httpUnhealthyDefaultTimeout)
	if err != nil {
		return "", nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("build request for %s: %v", rawURL, err)
	}

	// Клиент строится под opt-out-флаги задачи (нулевые opts = максимально
	// безопасный клиент: SSRF-guard + downgrade-защита + TLS-верификация). Три
	// контура ортогональны — allow_http не открывает SSRF (dial-guard отдельно).
	resp, derr := b.NewClient(util.HTTPClientOpts{
		AllowPrivate:       allowPrivate,
		InsecureSkipVerify: insecureSkipVerify,
		AllowHTTPRedirect:  allowHTTP,
	}).Do(req)
	if derr != nil {
		// Транспортная ошибка — эндпоинт недоступен наблюдателю → "unhealthy"
		// (status 0). Это валидное состояние, а не ошибка Check.
		return stateHTTPUnhealthy, httpData(rawURL, 0), nil
	}
	_ = resp.Body.Close()

	if containsStatus(wantCodes, resp.StatusCode) {
		return stateHTTPHealthy, httpData(rawURL, resp.StatusCode), nil
	}
	return stateHTTPUnhealthy, httpData(rawURL, resp.StatusCode), nil
}

// httpData несёт ТОЛЬКО url и статус-код. Тело и заголовки ответа сюда не
// попадают: sensitive-by-construction (ADR-010 §7.4) — beacon не светит payload
// в Portent/логи. status == 0 — транспортная ошибка (эндпоинт недоступен).
func httpData(url string, status int) *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"url":    url,
		"status": status,
	})
	return s
}

// containsStatus — линейный поиск (status_codes — короткий список, типично 1–3).
func containsStatus(codes []int64, status int) bool {
	for _, c := range codes {
		if c == int64(status) {
			return true
		}
	}
	return false
}
