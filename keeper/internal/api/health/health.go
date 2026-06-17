// Package health — реализация `/healthz` (liveness) и `/readyz` (readiness)
// для Keeper-а.
//
// Маршруты вне `/v1/*`, без auth (operator-api.md § Health / Meta), не
// пишутся в audit (high-frequency probes от k8s/balancer).
//
// `/healthz` — статический 200; «процесс жив». `/readyz` — пингует
// зависимости (Postgres + Redis обязательны, Vault — если сконфигурён), на
// failure отдаёт 503 со списком провалившихся checks в JSON. Полный набор
// зависимостей нужен для fail-fast: нездоровый Keeper отдаёт `not_ready`, и
// LB уводит трафик на здоровый инстанс кластера (ADR-002, HA-кластер Keeper).
package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// perCheckTimeout — жёсткий per-dependency timeout для ping-операции.
// Без него `/readyz` (unauthenticated endpoint) становится DoS-вектором:
// атакующий открывает сотни параллельных запросов, каждый удерживает
// PG-connection до общего request-timeout-а (десятки секунд).
// 2s — порядок «достаточно для здорового PG/Vault, бьёт по slow-path».
const perCheckTimeout = 2 * time.Second

// Pinger — минимальный интерфейс health-проверки одной зависимости.
// PG-pool и Vault-client оба реализуют `Ping(ctx) error`, поэтому
// каждый из них подходит без adapter-а.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps — зависимости, доступность которых проверяет `/readyz`. PG и Redis —
// обязательные (без них Keeper не обслуживает запросы: реестры в Postgres,
// lease/heartbeat в Redis — ADR-005/006); Vault — опционален (nil, если в
// инсталляции не сконфигурён). Любой nil-Pinger check пропускается и в
// response не упоминается.
type Deps struct {
	PG    Pinger
	Redis Pinger
	Vault Pinger
}

// Handler — собранные health-эндпоинты, регистрируется в роутере.
type Handler struct {
	deps Deps
}

// NewHandler собирает handler из зависимостей. Любая зависимость может
// быть nil — тогда соответствующий check пропускается (handler не
// сообщает о ней в response).
func NewHandler(deps Deps) *Handler {
	return &Handler{deps: deps}
}

// Healthz пишет 200 OK с фиксированным телом. Не зависит от состояния
// внешних систем (по определению liveness — «процесс отвечает»).
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz проверяет все непустые зависимости параллельно, каждую под
// per-check timeout-ом ([perCheckTimeout]). Overall-latency = max по
// проверкам (а не sum) — нужно для k8s probes с коротким period.
//
// На failure любой из проверок — 503 со списком статусов в `checks{}`.
type readyResp struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	type result struct {
		name string
		msg  string // пустой = ok
	}

	type namedPinger struct {
		name string
		p    Pinger
	}
	candidates := []namedPinger{
		{"postgres", h.deps.PG},
		{"redis", h.deps.Redis},
		{"vault", h.deps.Vault},
	}
	pingers := make([]namedPinger, 0, len(candidates))
	for _, c := range candidates {
		if c.p != nil {
			pingers = append(pingers, c)
		}
	}

	results := make([]result, len(pingers))
	var wg sync.WaitGroup
	for i, item := range pingers {
		wg.Add(1)
		go func(i int, name string, p Pinger) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), perCheckTimeout)
			defer cancel()
			err := p.Ping(ctx)
			if err == nil {
				results[i] = result{name: name}
				return
			}
			// Различаем timeout (наш per-check ctx истёк) от транспортной
			// ошибки — оператору сильно полезнее: timeout = «висит», error
			// = «отказывает с понятной причиной».
			if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil {
				results[i] = result{name: name, msg: "ping timeout (2s)"}
				return
			}
			results[i] = result{name: name, msg: "unreachable: " + err.Error()}
		}(i, item.name, item.p)
	}
	wg.Wait()

	checks := make(map[string]string, len(results))
	ok := true
	for _, res := range results {
		if res.msg == "" {
			checks[res.name] = "ok"
			continue
		}
		checks[res.name] = res.msg
		ok = false
	}

	resp := readyResp{Checks: checks}
	status := http.StatusOK
	if ok {
		resp.Status = "ok"
	} else {
		resp.Status = "not_ready"
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
