package legion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// voyageTerminalStatuses — терминальные статусы Voyage (ADR-043): прогон завершён,
// poll прекращается. running/pending/scheduled — нетерминальные (ждём дальше).
var voyageTerminalStatuses = map[string]struct{}{
	"succeeded":      {},
	"failed":         {},
	"partial_failed": {},
	"cancelled":      {},
}

// VoyageRunOptions — параметры одного command-Voyage по флоту (ось C run-нагрузки,
// docs/testing/load-testing.md §2). command выбран над scenario: единица батча
// command = ХОСТ (резолв coven→souls.sid напрямую), поэтому Keeper диспетчит
// ErrandRequest на ВЕСЬ N-флот — прямая dispatch-нагрузка по всем стабам. scenario-
// Voyage по coven резолвит coven в ИНКАРНАЦИИ (единица = инкарнация), что не
// нагружает dispatch по флоту и требует засева инкарнации + привязки хостов.
type VoyageRunOptions struct {
	BaseURL      string         // http://127.0.0.1:8080 (OpenAPI-listener)
	JWT          string         // admin-Archon-токен (errand.run)
	Coven        string         // target coven (= --coven легиона)
	Module       string         // command-модуль (default core.cmd.shell)
	Input        map[string]any // params модуля (CEL-rendered на Keeper-side)
	Concurrency  int            // top-level voyage.concurrency: >0 → кладётся в тело create; 0 → НЕ слать (keeper-дефолт=1+один Leg)
	PollInterval time.Duration  // период опроса GET /v1/voyages/{id}
	Timeout      time.Duration  // макс ожидание терминала
}

// VoyageRunReport — итог одного command-Voyage.
type VoyageRunReport struct {
	VoyageID    string
	ScopeSize   int           // сколько единиц (хостов) резолвлено в snapshot
	CreateLat   time.Duration // латентность POST /v1/voyages (приём + резолв + persist)
	EndToEnd    time.Duration // от POST до терминального статуса (dispatch→ErrandResult→commit→audit)
	FinalStatus string
	Succeeded   int
	Failed      int
	Polls       int // сколько раз опрошен GET /v1/voyages/{id} до терминала
}

// RunCommandVoyage создаёт ОДИН command-Voyage по coven легиона и polls до
// терминального статуса, замеряя end-to-end латентность оркестрации (POST →
// dispatch ErrandRequest по N stub-ам → N ErrandResult → voyage-target-terminal →
// audit-INSERT). Возвращает VoyageID для cleanup даже при ошибке poll-а (caller
// обязан снести Voyage из PG — DELETE через API недоступен для терминальных).
func RunCommandVoyage(ctx context.Context, opts VoyageRunOptions) (*VoyageRunReport, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("legion: пустой BaseURL для Voyage")
	}
	if opts.JWT == "" {
		return nil, fmt.Errorf("legion: пустой JWT для Voyage (admin-токен обязателен)")
	}
	if opts.Coven == "" {
		return nil, fmt.Errorf("legion: пустой coven для Voyage (нечего таргетить)")
	}
	module := opts.Module
	if module == "" {
		module = "core.cmd.shell"
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	reqBody := map[string]any{
		"kind":   "command",
		"module": module,
		"input":  opts.Input,
		"target": map[string]any{"coven": []string{opts.Coven}},
	}
	// concurrency=0 → поле НЕ слать (keeper-дефолт concurrency=1 + один Leg на весь
	// scope = последовательно, форма поля — top-level "concurrency" *int omitempty,
	// huma_voyage_op.go:47, minimum:1 maximum:500).
	if opts.Concurrency > 0 {
		reqBody["concurrency"] = opts.Concurrency
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("legion: marshal voyage body: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	createStart := time.Now()
	created, err := createVoyage(ctx, client, opts.BaseURL, opts.JWT, body)
	if err != nil {
		return nil, err
	}
	rep := &VoyageRunReport{
		VoyageID:  created.VoyageID,
		ScopeSize: created.ScopeSize,
		CreateLat: time.Since(createStart),
	}

	// Poll до терминала: end-to-end = от приёма create до терминального статуса.
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			rep.EndToEnd = time.Since(createStart)
			rep.FinalStatus = "ctx_cancelled"
			return rep, ctx.Err()
		}
		if time.Now().After(deadline) {
			rep.EndToEnd = time.Since(createStart)
			if rep.FinalStatus == "" {
				rep.FinalStatus = "timeout"
			}
			return rep, fmt.Errorf("legion: Voyage %s не достиг терминала за %s (последний статус %q)",
				rep.VoyageID, timeout, rep.FinalStatus)
		}

		status, succeeded, failed, gerr := getVoyageStatus(ctx, client, opts.BaseURL, opts.JWT, rep.VoyageID)
		rep.Polls++
		if gerr != nil {
			// Транзиентная ошибка GET — ретраим до deadline (не валим прогон).
			select {
			case <-ctx.Done():
			case <-time.After(poll):
			}
			continue
		}
		rep.FinalStatus = status
		rep.Succeeded = succeeded
		rep.Failed = failed
		if _, terminal := voyageTerminalStatuses[status]; terminal {
			rep.EndToEnd = time.Since(createStart)
			return rep, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
}

// voyageCreateResp — поля 202-тела POST /v1/voyages, нужные легиону.
type voyageCreateResp struct {
	VoyageID  string `json:"voyage_id"`
	ScopeSize int    `json:"scope_size"`
	Status    string `json:"status"`
}

func createVoyage(ctx context.Context, client *http.Client, base, jwt string, body []byte) (voyageCreateResp, error) {
	var out voyageCreateResp
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/voyages", bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("legion: build create voyage: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("legion: POST /v1/voyages: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return out, fmt.Errorf("legion: POST /v1/voyages: HTTP %d: %s", resp.StatusCode, truncate(raw, 400))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("legion: decode create voyage: %w", err)
	}
	if out.VoyageID == "" {
		return out, fmt.Errorf("legion: POST /v1/voyages: пустой voyage_id в ответе: %s", truncate(raw, 400))
	}
	return out, nil
}

// voyageGetResp — поля GET /v1/voyages/{id}, нужные легиону (status + summary).
type voyageGetResp struct {
	Status  string `json:"status"`
	Summary *struct {
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
	} `json:"summary"`
}

func getVoyageStatus(ctx context.Context, client *http.Client, base, jwt, id string) (status string, succeeded, failed int, err error) {
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/voyages/"+id, nil)
	if rerr != nil {
		return "", 0, 0, rerr
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, derr := client.Do(req)
	if derr != nil {
		return "", 0, 0, derr
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(raw, 200))
	}
	var out voyageGetResp
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return "", 0, 0, uerr
	}
	if out.Summary != nil {
		succeeded = out.Summary.Succeeded
		failed = out.Summary.Failed
	}
	return out.Status, succeeded, failed, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
