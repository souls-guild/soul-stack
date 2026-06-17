package legion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// WriteLoadOptions — параметры write-оси (docs/testing/load-testing.md): профиль
// write+audit-пути под нагрузкой через create→delete циклы безопасных сущностей.
// В отличие от оси B (read-only/dry-resolve, без мутации реестра), эта ось мерит
// именно write-путь: каждый POST проходит валидацию+persist+audit-INSERT, каждый
// DELETE — каскадное удаление+audit-INSERT. Сущности подобраны так, чтобы цикл
// create→delete был самоочищающимся (без накопления данных в реестре) и НЕ
// Tempo-лимитированным (не Voyage/Errand). Каждая итерация удаляет за собой то,
// что создала; финальный sweep — страховка от лика при сбое per-iteration delete.
type WriteLoadOptions struct {
	BaseURL     string        // http://127.0.0.1:8080 (OpenAPI-listener, plain HTTP в dev)
	JWT         string        // admin-Archon-токен (Authorization: Bearer ...)
	Concurrency int           // число параллельных воркеров (write тяжелее read → меньше)
	Duration    time.Duration // длительность гона
}

// writeEntity — описание одной create→delete-молотимой сущности write-оси.
// Имена итераций строятся как legionload-<kind>-w<worker>-<seq>, символы только
// [a-z0-9-] (паттерны всех 4 ручек запрещают _ и . — см. ТЗ/live-подтверждение).
type writeEntity struct {
	kind       string                   // короткое имя для отчёта/имён сущностей (a-z0-9-)
	listPath   string                   // GET /v1/<entity> для финального sweep
	createPath string                   // POST-путь (относительно BaseURL)
	createBody func(name string) []byte // тело create с уникальным именем
	deletePath func(name string) string // DELETE-путь по имени (относительно BaseURL)
}

// writeEntities — таблица безопасных самоочищающихся сущностей write-оси.
// Тела минимальны (только обязательные поля). herald.config.url — ОБЯЗАТЕЛЬНО
// https:// (netguard блокирует http/loopback на этой ручке), иначе POST 4xx-ит.
func writeEntities() []writeEntity {
	jsonBody := func(m map[string]any) []byte {
		b, _ := json.Marshal(m)
		return b
	}
	return []writeEntity{
		{
			kind:       "synod",
			listPath:   "/v1/synods",
			createPath: "/v1/synods",
			createBody: func(name string) []byte { return jsonBody(map[string]any{"name": name}) },
			deletePath: func(name string) string { return "/v1/synods/" + name },
		},
		{
			kind:       "role",
			listPath:   "/v1/roles",
			createPath: "/v1/roles",
			createBody: func(name string) []byte { return jsonBody(map[string]any{"name": name}) },
			deletePath: func(name string) string { return "/v1/roles/" + name },
		},
		{
			kind:       "push-provider",
			listPath:   "/v1/push-providers",
			createPath: "/v1/push-providers",
			createBody: func(name string) []byte { return jsonBody(map[string]any{"name": name}) },
			deletePath: func(name string) string { return "/v1/push-providers/" + name },
		},
		{
			kind:       "herald",
			listPath:   "/v1/heralds",
			createPath: "/v1/heralds",
			createBody: func(name string) []byte {
				return jsonBody(map[string]any{
					"name": name,
					"type": "webhook",
					"config": map[string]any{
						// ОБЯЗАТЕЛЬНО https:// — netguard блокирует http/loopback.
						"url": "https://example.com/hook",
					},
				})
			},
			deletePath: func(name string) string { return "/v1/heralds/" + name },
		},
	}
}

// WriteEntityStat — агрегат по одной сущности: create и delete мерятся раздельно
// (две строки отчёта на сущность — POST <kind> и DELETE <kind>).
type WriteEntityStat struct {
	Kind   string
	Create EndpointStat // POST: req=успешных 201, err=не-201/транспортные
	Delete EndpointStat // DELETE: req=успешных 204, err=не-204/транспортные
}

// WriteLoadReport — итог write-оси: per-kind create/delete-статистика + sweep +
// первая ошибка по любой ручке.
type WriteLoadReport struct {
	Entities []WriteEntityStat
	Swept    int           // сколько остаточных legionload-* снёс финальный sweep
	Wall     time.Duration // фактическая длительность цикла create→delete
	FirstErr string        // первая ошибка (с именем kind+операцией)
}

// RunWriteLoad гонит create→delete циклы безопасных сущностей в Concurrency
// воркеров на протяжении Duration. Каждый воркер round-robin проходит таблицу
// сущностей: на каждой итерации создаёт сущность с УНИКАЛЬНЫМ именем
// (legionload-<kind>-w<worker>-<seq>), при 201 сразу удаляет её, замеряя create-
// и delete-латентность РАЗДЕЛЬНО per-kind. При create не-201 delete НЕ шлётся
// (создавать нечего) — ошибка учитывается в Create.Errors. После цикла —
// best-effort sweep: для каждой сущности GET список, фильтр по префиксу
// legionload-, DELETE остаточных (страховка от лика при сбое delete в цикле).
func RunWriteLoad(ctx context.Context, opts WriteLoadOptions) (*WriteLoadReport, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("legion: пустой BaseURL для write-нагрузки")
	}
	if opts.JWT == "" {
		return nil, fmt.Errorf("legion: пустой JWT для write-нагрузки (admin-токен обязателен)")
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 8
	}

	ents := writeEntities()

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        conc * 2,
			MaxIdleConnsPerHost: conc * 2,
			MaxConnsPerHost:     conc * 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	// Раздельные аккумуляторы create/delete на каждую сущность.
	createAcc := make([]endpointAcc, len(ents))
	deleteAcc := make([]endpointAcc, len(ents))

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
		go func(worker int) {
			defer wg.Done()
			seq := 0
			for loadCtx.Err() == nil {
				for i := range ents {
					if loadCtx.Err() != nil {
						return
					}
					seq++
					name := fmt.Sprintf("legionload-%s-w%d-%d", ents[i].kind, worker, seq)
					if writeCreate(loadCtx, client, opts.BaseURL, opts.JWT, ents[i].createPath, ents[i].createBody(name), &createAcc[i]) {
						writeDelete(loadCtx, client, opts.BaseURL, opts.JWT, ents[i].deletePath(name), &deleteAcc[i])
					}
				}
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(start)

	rep := &WriteLoadReport{
		Entities: make([]WriteEntityStat, len(ents)),
		Wall:     wall,
	}
	for i := range ents {
		rep.Entities[i] = WriteEntityStat{
			Kind:   ents[i].kind,
			Create: createAcc[i].finalize("POST "+ents[i].kind, wall),
			Delete: deleteAcc[i].finalize("DELETE "+ents[i].kind, wall),
		}
		if rep.FirstErr == "" && createAcc[i].firstHTTPErr != "" {
			rep.FirstErr = "POST " + ents[i].kind + ": " + createAcc[i].firstHTTPErr
		}
		if rep.FirstErr == "" && deleteAcc[i].firstHTTPErr != "" {
			rep.FirstErr = "DELETE " + ents[i].kind + ": " + deleteAcc[i].firstHTTPErr
		}
	}

	// Финальный sweep — на background-context (как souls-cleanup): прибрать
	// остаточные legionload-* даже если основной ctx уже отменён (Ctrl-C/Duration).
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	rep.Swept = sweepResidual(sctx, client, opts.BaseURL, opts.JWT, ents)
	scancel()
	if rep.Swept > 0 {
		fmt.Printf("[write] sweep: удалено %d остаточных\n", rep.Swept)
	}
	return rep, nil
}

// writeCreate шлёт POST create и записывает латентность/ошибку. Возвращает true
// только при HTTP 201 (есть что удалять); иначе record err и false (delete не
// шлётся). Контекст-отмена (истёк Duration) — штатный конец гона, не ошибка.
func writeCreate(ctx context.Context, client *http.Client, base, jwt, path string, body []byte, acc *endpointAcc) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		acc.recordErr(err.Error())
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		acc.recordErr(fmt.Sprintf("HTTP %d", resp.StatusCode))
		return false
	}
	acc.record(time.Since(t0))
	return true
}

// writeDelete шлёт DELETE и записывает латентность/ошибку. Успех — HTTP 204.
func writeDelete(ctx context.Context, client *http.Client, base, jwt, path string, acc *endpointAcc) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+path, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		acc.recordErr(err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		acc.recordErr(fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}
	acc.record(time.Since(t0))
}

// sweepResidual — best-effort страховка против лика: для каждой сущности GET
// список, фильтр имён по префиксу legionload-, DELETE каждого. Bounded (на случай
// если delete в цикле падал — иначе списки уже пусты). В норме per-iteration
// delete всё прибрал и sweep удаляет 0. Ошибки sweep-а молча проглатываются —
// это уборка, не предмет замера.
func sweepResidual(ctx context.Context, client *http.Client, base, jwt string, ents []writeEntity) int {
	const maxPerKind = 5000 // защитный потолок: не зацикливаться на огромном чужом списке
	total := 0
	for i := range ents {
		names := listResidualNames(ctx, client, base, jwt, ents[i].listPath)
		swept := 0
		for _, name := range names {
			if swept >= maxPerKind {
				break
			}
			if !strings.HasPrefix(name, "legionload-") {
				continue
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+ents[i].deletePath(name), nil)
			if err != nil {
				continue
			}
			req.Header.Set("Authorization", "Bearer "+jwt)
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				swept++
			}
		}
		total += swept
	}
	return total
}

// listResidualNames делает один GET <listPath> и достаёт name-поля из ответа.
// Все 4 list-ручки отдают либо плоский массив объектов, либо обёртку
// {"items":[...]} — разбираем оба варианта по полю name. Ошибки → пустой список
// (sweep пропустит этот kind, основной per-iteration delete всё равно прибрал).
func listResidualNames(ctx context.Context, client *http.Client, base, jwt, listPath string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+listPath, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	// Сначала пробуем обёртку {"items":[{name}...]}, затем плоский массив [{name}].
	var wrapped struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Items) > 0 {
		out := make([]string, 0, len(wrapped.Items))
		for _, it := range wrapped.Items {
			if it.Name != "" {
				out = append(out, it.Name)
			}
		}
		return out
	}
	var flat []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &flat) == nil {
		out := make([]string, 0, len(flat))
		for _, it := range flat {
			if it.Name != "" {
				out = append(out, it.Name)
			}
		}
		return out
	}
	return nil
}
