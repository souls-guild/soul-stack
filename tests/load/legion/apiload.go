package legion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// APILoadOptions — параметры оси B (docs/testing/load-testing.md §2): concurrent-
// гон флот-зависимых /v1-ручек поверх фона из N подключённых stub-ов. Стоимость
// этих ручек растёт с размером реестра souls (presence-резолв на list, roster-
// резолв coven на preview), поэтому гонять их имеет смысл только при живом легионе.
type APILoadOptions struct {
	BaseURL     string        // http://127.0.0.1:8080 (OpenAPI-listener, plain HTTP в dev)
	JWT         string        // admin-Archon-токен (Authorization: Bearer ...)
	Coven       string        // target coven для POST /v1/voyages/preview (= --coven легиона)
	Concurrency int           // число параллельных воркеров-клиентов
	Duration    time.Duration // длительность гона
}

// endpoint — описание одной молотимой ручки в таблице оси B. Безопасные для
// гона ручки: read-only GET-collection (молотить без мутации реестра) +
// единственный read-like POST /v1/voyages/preview (dry-resolve, без создания
// Voyage и без audit). method+path фиксированы; body непустой только у preview.
type endpoint struct {
	name   string // человекочитаемое имя (метод+путь) для отчёта/FirstErr
	method string
	path   string // относительно BaseURL (с query при пагинации)
	body   []byte // nil для GET
}

// EndpointStat — агрегат латентности по одной ручке за гон оси B.
type EndpointStat struct {
	Name       string        // человекочитаемое имя (метод+путь)
	Requests   int           // успешных (2xx) запросов
	Errors     int           // не-2xx / транспортных ошибок
	P50        time.Duration // медиана латентности успешных
	P99        time.Duration
	Max        time.Duration
	Throughput float64 // успешных req/s за фактическую длительность
}

// APILoadReport — итог оси B: per-endpoint статистика + первая ошибка.
// Skipped — пути ручек, исключённых стартовым probe-ом (вернули 404 «no such
// endpoint» — не смонтированы в этом keeper-конфиге, нечего мерить).
type APILoadReport struct {
	Endpoints []EndpointStat
	Skipped   []string      // пути ручек, исключённых probe-ом (404, не смонтированы)
	Wall      time.Duration // фактическая длительность гона
	FirstErr  string        // первая ошибка по любой ручке (с её именем)
}

// endpointAcc — потокобезопасный аккумулятор замеров одной ручки.
type endpointAcc struct {
	mu           sync.Mutex
	lat          []time.Duration
	reqs         int64
	errs         int64
	firstHTTPErr string
}

func (a *endpointAcc) record(d time.Duration) {
	a.mu.Lock()
	a.lat = append(a.lat, d)
	a.mu.Unlock()
	atomic.AddInt64(&a.reqs, 1)
}

func (a *endpointAcc) recordErr(msg string) {
	atomic.AddInt64(&a.errs, 1)
	a.mu.Lock()
	if a.firstHTTPErr == "" {
		a.firstHTTPErr = msg
	}
	a.mu.Unlock()
}

func (a *endpointAcc) finalize(name string, wall time.Duration) EndpointStat {
	a.mu.Lock()
	lat := a.lat
	a.mu.Unlock()
	st := EndpointStat{
		Name:     name,
		Requests: int(atomic.LoadInt64(&a.reqs)),
		Errors:   int(atomic.LoadInt64(&a.errs)),
	}
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		st.P50 = percentile(lat, 50)
		st.P99 = percentile(lat, 99)
		st.Max = lat[len(lat)-1]
	}
	if wall > 0 {
		st.Throughput = float64(st.Requests) / wall.Seconds()
	}
	return st
}

// safeEndpoints собирает таблицу безопасных для гона ручек: 24 read-only GET-
// collection (молотить без мутации реестра) + единственный read-like POST
// /v1/voyages/preview (dry-resolve по coven легиона — НЕ создаёт Voyage и не
// пишет audit). На list-ручках с пагинацией стоит ?limit=100, каталоги/me —
// bare. Пути сверены с /openapi.json. previewBody — тело dry-resolve.
func safeEndpoints(baseURL string, previewBody []byte) []endpoint {
	get := []string{
		"/v1/souls?limit=100",
		"/v1/audit?limit=100",
		"/v1/voyages?limit=100",
		"/v1/errands?limit=100",
		"/v1/incarnations",
		"/v1/cadences",
		"/v1/operators",
		"/v1/synods",
		"/v1/services",
		"/v1/push-runs?limit=100",
		"/v1/push-providers",
		"/v1/heralds",
		"/v1/decrees",
		"/v1/vigils",
		"/v1/tidings",
		"/v1/augur/omens",
		// rites требует обязательный query-параметр omen (валиден по
		// ^[a-z0-9-]{1,63}$); без него ручка 422-ит. load-probe — фиксированный
		// валидный omen, вернёт 200 с (возможно пустым) списком rites.
		"/v1/augur/rites?omen=load-probe",
		"/v1/sigil/keys",
		"/v1/plugins/sigils",
		"/v1/modules",
		"/v1/event-types",
		"/v1/permissions",
		"/v1/roles",
		"/v1/me/permissions",
	}
	eps := make([]endpoint, 0, len(get)+1)
	for _, p := range get {
		eps = append(eps, endpoint{
			name:   "GET " + p,
			method: http.MethodGet,
			path:   baseURL + p,
		})
	}
	eps = append(eps, endpoint{
		name:   "POST /v1/voyages/preview (coven)",
		method: http.MethodPost,
		path:   baseURL + "/v1/voyages/preview",
		body:   previewBody,
	})
	return eps
}

// RunAPILoad гонит все безопасные флот-зависимые ручки (см. safeEndpoints)
// параллельно в Concurrency воркеров на протяжении Duration. Каждый воркер по
// кругу проходит весь список ручек (round-robin), поэтому каждая ручка получает
// ~равную долю нагрузки. Замеряет per-endpoint p50/p99/throughput. Не-2xx ответ
// (напр. выключенная фича) учитывается в Errors своей строки и НЕ роняет гон;
// dry-resolve preview НЕ создаёт Voyage (read-like, без audit) — гон безопасен.
func RunAPILoad(ctx context.Context, opts APILoadOptions) (*APILoadReport, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("legion: пустой BaseURL для API-нагрузки")
	}
	if opts.JWT == "" {
		return nil, fmt.Errorf("legion: пустой JWT для API-нагрузки (admin-токен обязателен)")
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 16
	}

	// Тело preview: kind=command, target по coven легиона. Read-like dry-resolve —
	// та же валидация/резолв, что create, но без создания Voyage и без audit.
	previewBody, err := json.Marshal(map[string]any{
		"kind":   "command",
		"module": "core.cmd.shell",
		"input":  map[string]any{"cmd": "echo ok"},
		"target": map[string]any{"coven": []string{opts.Coven}},
	})
	if err != nil {
		return nil, fmt.Errorf("legion: marshal preview body: %w", err)
	}

	allEps := safeEndpoints(opts.BaseURL, previewBody)

	// Пул переиспользуемых соединений: API-нагрузка не должна упираться в TCP-
	// handshake/conn-churn (мерим Keeper, не клиента).
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        conc * 2,
			MaxIdleConnsPerHost: conc * 2,
			MaxConnsPerHost:     conc * 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	// Стартовый probe: один запрос на ручку. Часть /v1-ручек монтируется на
	// роутер условно (при non-nil Deps своего сервиса); в dev-конфиге их сервисы
	// могут быть не прокинуты — тогда роутер отдаёт 404 «no such endpoint». Это
	// норма конфига, не баг харнеса: такие ручки исключаем из нагрузки. Прочие
	// статусы (422/403/5xx) — реальные сигналы, их меряет load-цикл, не исключаем.
	eps, skipped := probeEndpoints(ctx, client, allEps, opts.JWT)
	accs := make([]endpointAcc, len(eps))

	loadCtx := ctx
	var cancel context.CancelFunc
	if opts.Duration > 0 {
		loadCtx, cancel = context.WithTimeout(ctx, opts.Duration)
		defer cancel()
	}

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for loadCtx.Err() == nil {
				for i := range eps {
					if loadCtx.Err() != nil {
						return
					}
					doRequest(loadCtx, client, eps[i].method, eps[i].path, opts.JWT, eps[i].body, &accs[i])
				}
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	rep := &APILoadReport{
		Endpoints: make([]EndpointStat, len(eps)),
		Skipped:   skipped,
		Wall:      wall,
	}
	for i := range eps {
		rep.Endpoints[i] = accs[i].finalize(eps[i].name, wall)
		if rep.FirstErr == "" && accs[i].firstHTTPErr != "" {
			rep.FirstErr = eps[i].name + ": " + accs[i].firstHTTPErr
		}
	}
	return rep, nil
}

// probeEndpoints делает ОДИН пробный запрос на каждую ручку до load-цикла и
// делит таблицу на смонтированные (mounted) и пропущенные (skipped). Критерий
// пропуска — ровно HTTP 404 (ручка условно не смонтирована в этом keeper-
// конфиге). Любой другой исход (2xx/422/403/5xx/транспортная ошибка) → ручка
// считается смонтированной и идёт в нагрузку: не-404-статусы — реальные сигналы,
// глушить их нельзя (принцип «no silent cap»). Пропуск логируется явно одной
// строкой [api] probe: ... с перечнем путей.
func probeEndpoints(ctx context.Context, client *http.Client, eps []endpoint, jwt string) (mounted []endpoint, skipped []string) {
	mounted = make([]endpoint, 0, len(eps))
	for i := range eps {
		if probeStatus(ctx, client, eps[i], jwt) == http.StatusNotFound {
			skipped = append(skipped, eps[i].path)
			continue
		}
		mounted = append(mounted, eps[i])
	}
	if len(skipped) > 0 {
		fmt.Printf("[api] probe: пропущено %d не-смонтированных ручек (404): %s\n",
			len(skipped), strings.Join(skipped, ", "))
	}
	return mounted, skipped
}

// probeStatus шлёт один пробный запрос и возвращает HTTP-статус. Транспортная
// ошибка/таймаут возвращает 0 (≠ 404 → ручка не исключается: load-цикл сам
// учтёт обрывы в Errors). Probe-запрос ходит с тем же телом, что и load (preview
// требует валидное тело для резолва, иначе вернул бы 4xx не из-за монтирования).
func probeStatus(ctx context.Context, client *http.Client, ep endpoint, jwt string) int {
	var rdr io.Reader
	if ep.body != nil {
		rdr = bytes.NewReader(ep.body)
	}
	req, err := http.NewRequestWithContext(ctx, ep.method, ep.path, rdr)
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	if ep.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// doRequest шлёт один запрос и записывает латентность/ошибку в acc. Контекст-
// отмена (истёк Duration) не считается ошибкой ручки — это штатный конец гона.
func doRequest(ctx context.Context, client *http.Client, method, url, jwt string, body []byte, acc *endpointAcc) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return // штатное завершение гона
		}
		acc.recordErr(err.Error())
		return
	}
	// Дренируем тело: без этого keep-alive-соединение не переиспользуется.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		acc.recordErr(fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}
	acc.record(time.Since(t0))
}
