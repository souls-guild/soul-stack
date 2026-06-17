package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// ErrandAPI — типизированные методы /v1/souls/{sid}/exec и /v1/errands*.
type ErrandAPI struct {
	c *Client
}

// ErrandExecRequest — body POST /v1/souls/{sid}/exec. SID живёт в path.
type ErrandExecRequest struct {
	SID            string         `json:"-"`
	Module         string         `json:"module"`
	Input          map[string]any `json:"input,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	DryRun         bool           `json:"dry_run,omitempty"`
}

// ErrandResult — JSON-форма ответа /v1/souls/{sid}/exec (200) и
// /v1/errands/{errand_id} (200). Поля те же, что у keeper-side
// errandResultResponse — клиент держит локальную копию, чтобы не зависеть
// от внутренних пакетов keeper-а.
type ErrandResult struct {
	ErrandID        string         `json:"errand_id"`
	SID             string         `json:"sid"`
	Module          string         `json:"module"`
	Status          string         `json:"status"`
	ExitCode        *int32         `json:"exit_code,omitempty"`
	Stdout          string         `json:"stdout,omitempty"`
	Stderr          string         `json:"stderr,omitempty"`
	StdoutTruncated bool           `json:"stdout_truncated"`
	StderrTruncated bool           `json:"stderr_truncated"`
	DurationMs      *int64         `json:"duration_ms,omitempty"`
	ErrorMessage    string         `json:"error_message,omitempty"`
	Output          map[string]any `json:"output,omitempty"`
	StartedByAID    string         `json:"started_by_aid"`
	StartedAt       string         `json:"started_at"`
	FinishedAt      string         `json:"finished_at,omitempty"`
}

// errandAcceptedResponse — 202 body при async-эскалации (Errand-результат
// продолжится в фоне). errand_id + status — единственные стабильные поля.
type errandAcceptedResponse struct {
	ErrandID string `json:"errand_id"`
	Status   string `json:"status"`
}

// ErrandListOptions — query-фильтры GET /v1/errands.
type ErrandListOptions struct {
	SID          string
	Status       string
	StartedAfter string
	Limit        int
	Offset       int
}

// ErrandListReply — страница списка (paged-response).
type ErrandListReply struct {
	Items  []ErrandResult `json:"items"`
	Offset int            `json:"offset"`
	Limit  int            `json:"limit"`
	Total  int            `json:"total"`
}

// Exec — POST /v1/souls/{sid}/exec. Возвращает result+async-флаг:
//   - 200 → (result, false, nil).
//   - 202 → (result-with-only-id-and-running-status, true, nil); caller
//     дальше делает poll через Get.
//   - 4xx/5xx → (zero, false, *APIError).
func (a *ErrandAPI) Exec(ctx context.Context, req ErrandExecRequest) (ErrandResult, bool, error) {
	if req.SID == "" {
		return ErrandResult{}, false, fmt.Errorf("SID пуст")
	}
	if req.Module == "" {
		return ErrandResult{}, false, fmt.Errorf("module пуст")
	}
	path := "/v1/souls/" + url.PathEscape(req.SID) + "/exec"

	// 202 / 200 различаются по форме body. Чтобы не делать второй raw-call,
	// используем общий []byte-канал: декодируем сперва в acceptedResponse, и
	// если status="running" + остальные поля пусты → async; иначе full result.
	// Простейший путь — отдельный Do-метод. Но текущий Do укладывает 4xx в
	// *APIError и парсит JSON. Здесь подход: пытаемся декодировать в полную
	// форму; если sid пуст (т.е. result не пришёл) — считаем 202.
	//
	// Альтернатива — добавить специальный канал. Делаем так: декодим в
	// специально устроенный wrapper, который держит и accept, и full.
	var raw struct {
		ErrandResult
		// поля errandAcceptedResponse уже включены в ErrandResult (errand_id, status).
	}
	if err := a.c.Do(ctx, "POST", path, req, &raw); err != nil {
		return ErrandResult{}, false, err
	}
	// Async признак: ErrandResult.Status == "running" и нет finished_at — Keeper
	// в этом случае отдал минимальный 202-body (errand_id + status). На терминал-
	// строке status ∈ {success/failed/timed_out/cancelled/module_not_allowed},
	// finished_at заполнен.
	async := raw.Status == "running" && raw.FinishedAt == ""
	return raw.ErrandResult, async, nil
}

// Get — GET /v1/errands/{errand_id}. Keeper отдаёт 200 на терминалы и 202 на
// running. Для CLI обе формы одинаково полезны: возвращаем result + async-флаг.
func (a *ErrandAPI) Get(ctx context.Context, errandID string) (ErrandResult, bool, error) {
	if errandID == "" {
		return ErrandResult{}, false, fmt.Errorf("errand_id пуст")
	}
	var raw ErrandResult
	if err := a.c.Do(ctx, "GET", "/v1/errands/"+url.PathEscape(errandID), nil, &raw); err != nil {
		return ErrandResult{}, false, err
	}
	async := raw.Status == "running" && raw.FinishedAt == ""
	return raw, async, nil
}

// Cancel — DELETE /v1/errands/{errand_id} (ADR-033 slice E5). Permission:
// errand.cancel. Возвращает nil на 204; *APIError на 404/409/500. Финальный
// статус cancelled оператор увидит через Get (poll) — Soul пришлёт
// ErrandResult{CANCELLED} после получения CancelErrand-сигнала.
func (a *ErrandAPI) Cancel(ctx context.Context, errandID string) error {
	if errandID == "" {
		return fmt.Errorf("errand_id пуст")
	}
	return a.c.Do(ctx, "DELETE", "/v1/errands/"+url.PathEscape(errandID), nil, nil)
}

// List — GET /v1/errands. Query-параметры собираются из opts.
func (a *ErrandAPI) List(ctx context.Context, opts ErrandListOptions) (*ErrandListReply, error) {
	q := url.Values{}
	if opts.SID != "" {
		q.Set("sid", opts.SID)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.StartedAfter != "" {
		q.Set("started_after", opts.StartedAfter)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := "/v1/errands"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var reply ErrandListReply
	if err := a.c.Do(ctx, "GET", path, nil, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
