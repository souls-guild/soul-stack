//go:build e2e

package harness

// Voyage-helper-ы для multi-keeper crash-harness: создание scenario-Voyage
// через Operator-API (POST /v1/voyages), наблюдение за claim-владельцем
// (voyages.claimed_by_kid) и за recovery после краша владельца (reclaim_voyages
// → re-claim другим KID → терминал).
//
// Терминал успешного Voyage = status='succeeded' (миграция 059). attempt++ на
// каждый claim/reclaim (fencing-epoch ADR-027(g)).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// VoyageSnapshot — наблюдаемое состояние строки voyages для assert-ов.
type VoyageSnapshot struct {
	Status     string
	ClaimedBy  *string
	Attempt    int
	TotalBatch int
	BatchIndex int
	Finished   bool
}

// CreateScenarioVoyage создаёт scenario-Voyage через POST /v1/voyages поверх
// перечисленных инкарнаций. batchSize>0 → serial-волны по batchSize инкарнаций
// (несколько батчей растягивают прогон, расширяя crash-окно). Возвращает
// voyage_id из 202-тела.
//
// 202 → voyage_id; иной статус — t.Fatal с телом (диагностика без догадок).
// Транзиентный 422 «not registered» (тёплый снимок service-registry) поллится,
// как в CreateIncarnation.
func (s *Stack) CreateScenarioVoyage(t *testing.T, scenario string, incarnations []string, batchSize int) string {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{
		"kind":          "scenario",
		"scenario_name": scenario,
		"target": map[string]any{
			"incarnations": incarnations,
		},
	}
	if batchSize > 0 {
		body["batch_size"] = batchSize
	}

	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/voyages", body)
		if err != nil {
			t.Fatalf("CreateScenarioVoyage %s: http: %v", scenario, err)
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	if status != http.StatusAccepted {
		t.Fatalf("CreateScenarioVoyage %s: status %d, body=%s", scenario, status, string(resp))
	}
	var out struct {
		VoyageID string `json:"voyage_id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateScenarioVoyage %s: decode: %v (body=%s)", scenario, err, string(resp))
	}
	if out.VoyageID == "" {
		t.Fatalf("CreateScenarioVoyage %s: пустой voyage_id (body=%s)", scenario, string(resp))
	}
	return out.VoyageID
}

// VoyageState читает текущий снимок строки voyages по ID. Fatal при отсутствии
// строки или query-ошибке.
func (s *Stack) VoyageState(t *testing.T, voyageID string) VoyageSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var (
		snap     VoyageSnapshot
		finished *time.Time
	)
	err := s.db.QueryRow(ctx, `
		SELECT status, claimed_by_kid, attempt, total_batches, current_batch_index, finished_at
		FROM voyages WHERE voyage_id = $1
	`, voyageID).Scan(&snap.Status, &snap.ClaimedBy, &snap.Attempt, &snap.TotalBatch, &snap.BatchIndex, &finished)
	if err != nil {
		t.Fatalf("VoyageState %s: %v", voyageID, err)
	}
	snap.Finished = finished != nil
	return snap
}

// WaitVoyageRunningOwner поллит, пока Voyage не перейдёт в running с непустым
// claimed_by_kid; возвращает KID-владельца. Это точка съёма «кого убивать».
// Fatal при таймауте (с последним снимком).
func (s *Stack) WaitVoyageRunningOwner(t *testing.T, voyageID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last VoyageSnapshot
	for time.Now().Before(deadline) {
		last = s.VoyageState(t, voyageID)
		if last.Status == "running" && last.ClaimedBy != nil && *last.ClaimedBy != "" {
			return *last.ClaimedBy
		}
		// Если Voyage уже доехал до терминала ДО того, как мы поймали running —
		// окно слишком узкое для kill; тест обязан расширить прогон.
		if isVoyageTerminal(last.Status) {
			t.Fatalf("WaitVoyageRunningOwner %s: Voyage достиг терминала %q до поимки running-владельца (окно слишком узкое — увеличь scope/batch)",
				voyageID, last.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("WaitVoyageRunningOwner %s: running-владелец не появился за %s (последний снимок=%+v)",
		voyageID, timeout, last)
	return ""
}

// WaitVoyageReclaimed поллит, пока Voyage не будет перезахвачен ДРУГИМ KID
// (claimed_by_kid != killedKID) при attempt > attemptBeforeKill. Это прямое
// доказательство, что reclaim_voyages вернул протухший claim в pending, и живой
// keeper его подобрал. Возвращает KID нового владельца.
//
// Допускает промежуточные снимки (pending без владельца между reclaim и
// re-claim, либо уже succeeded с claimed_by_kid нового владельца) — ключевой
// инвариант: владелец сменился И attempt вырос.
func (s *Stack) WaitVoyageReclaimed(t *testing.T, voyageID, killedKID string, attemptBeforeKill int, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last VoyageSnapshot
	for time.Now().Before(deadline) {
		last = s.VoyageState(t, voyageID)
		if last.Attempt > attemptBeforeKill && last.ClaimedBy != nil &&
			*last.ClaimedBy != "" && *last.ClaimedBy != killedKID {
			return *last.ClaimedBy
		}
		// Терминал с новым владельцем-не-killed и выросшим attempt — тоже
		// валидный re-claim (быстрый прогон успел добежать).
		if isVoyageTerminal(last.Status) && last.Attempt > attemptBeforeKill &&
			last.ClaimedBy != nil && *last.ClaimedBy != killedKID {
			return *last.ClaimedBy
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("WaitVoyageReclaimed %s: re-claim другим KID не произошёл за %s (killed=%s, attempt_before=%d, последний снимок=%+v)",
		voyageID, timeout, killedKID, attemptBeforeKill, last)
	return ""
}

// WaitVoyageSucceeded поллит, пока Voyage не достигнет status='succeeded' с
// finished_at. Терминал != succeeded (failed/partial_failed/cancelled) —
// немедленный t.Fatal (recovery должен доисполнить прогон до УСПЕХА, не до
// любого терминала).
func (s *Stack) WaitVoyageSucceeded(t *testing.T, voyageID string, timeout time.Duration) VoyageSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last VoyageSnapshot
	for time.Now().Before(deadline) {
		last = s.VoyageState(t, voyageID)
		switch last.Status {
		case "succeeded":
			if !last.Finished {
				t.Fatalf("WaitVoyageSucceeded %s: status=succeeded, но finished_at пуст (нарушен инвариант voyages_terminal_finished_at)", voyageID)
			}
			return last
		case "failed", "partial_failed", "cancelled":
			t.Fatalf("WaitVoyageSucceeded %s: терминал %q вместо succeeded (recovery не доисполнил прогон; снимок=%+v)",
				voyageID, last.Status, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("WaitVoyageSucceeded %s: succeeded не достигнут за %s (последний снимок=%+v)", voyageID, timeout, last)
	return VoyageSnapshot{}
}

// AssertVoyageTargetsTerminal проверяет, что в voyage_targets все строки прогона
// достигли терминала (status='succeeded'), то есть прогон РЕАЛЬНО доисполнился
// по каждой единице (не «формально succeeded на пустом scope»). Возвращает
// число succeeded-target-ов.
func (s *Stack) AssertVoyageTargetsTerminal(t *testing.T, voyageID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx,
		"SELECT target_id, status FROM voyage_targets WHERE voyage_id = $1", voyageID)
	if err != nil {
		t.Fatalf("AssertVoyageTargetsTerminal %s: query: %v", voyageID, err)
	}
	defer rows.Close()
	statuses := map[string]string{}
	for rows.Next() {
		var ref, st string
		if err := rows.Scan(&ref, &st); err != nil {
			t.Fatalf("AssertVoyageTargetsTerminal %s: scan: %v", voyageID, err)
		}
		statuses[ref] = st
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertVoyageTargetsTerminal %s: rows.Err: %v", voyageID, err)
	}
	if len(statuses) == 0 {
		t.Fatalf("AssertVoyageTargetsTerminal %s: ни одной строки voyage_targets", voyageID)
	}
	succeeded := 0
	for ref, st := range statuses {
		if st != "succeeded" {
			t.Fatalf("AssertVoyageTargetsTerminal %s: target=%s status=%q (не succeeded; матрица=%v)",
				voyageID, ref, st, statuses)
		}
		succeeded++
	}
	return succeeded
}

// IncarnationsInStatus возвращает имена инкарнаций (из incNames) в заданном
// статусе. Используется для детекта seam-дефекта: инкарнация, осиротевшая в
// `applying` после краша keeper-владельца её per-incarnation scenario-run-а.
func (s *Stack) IncarnationsInStatus(t *testing.T, incNames []string, status string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []string
	for _, name := range incNames {
		var st string
		if err := s.db.QueryRow(ctx, "SELECT status FROM incarnation WHERE name = $1", name).Scan(&st); err != nil {
			t.Fatalf("IncarnationsInStatus(%s): %v", name, err)
		}
		if st == status {
			out = append(out, name)
		}
	}
	return out
}

// IncarnationStatusDetails возвращает (status, status_details-как-строка) для
// инкарнации. status_details — JSONB с причиной error_locked (для диагностики
// seam-варианта, когда reclaim приводит к error_locked вместо succeeded).
func (s *Stack) IncarnationStatusDetails(t *testing.T, name string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	var details *string
	if err := s.db.QueryRow(ctx,
		"SELECT status, status_details::text FROM incarnation WHERE name = $1", name).Scan(&status, &details); err != nil {
		t.Fatalf("IncarnationStatusDetails(%s): %v", name, err)
	}
	d := ""
	if details != nil {
		d = *details
	}
	return status, d
}

// CountApplyRunsForIncarnation возвращает число строк apply_runs для инкарнации
// (любой статус). Для подтверждения, что осиротевший `applying`-lock не имеет
// живых apply_runs (нечего реклеймить через reclaim_apply_runs — lock висит
// только на incarnation.status).
func (s *Stack) CountApplyRunsForIncarnation(t *testing.T, incarnation string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := s.db.QueryRow(ctx,
		"SELECT COUNT(*) FROM apply_runs WHERE incarnation_name = $1", incarnation).Scan(&n); err != nil {
		t.Fatalf("CountApplyRunsForIncarnation(%s): %v", incarnation, err)
	}
	return n
}

// DumpRecoveryState логирует диагностический срез состояния recovery: статусы
// инкарнаций + apply_runs + voyage_targets. Используется при разборе seam-багов
// (краш→reclaim→зависание): показывает, какие инкарнации застряли в applying и
// какие apply_runs осиротели после краша владельца.
func (s *Stack) DumpRecoveryState(t *testing.T, voyageID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	incRows, err := s.db.Query(ctx, `SELECT name, status FROM incarnation ORDER BY name`)
	if err == nil {
		var dump []string
		for incRows.Next() {
			var name, st string
			_ = incRows.Scan(&name, &st)
			dump = append(dump, name+"="+st)
		}
		incRows.Close()
		t.Logf("DUMP incarnation: %v", dump)
	}

	arRows, err := s.db.Query(ctx, `
		SELECT incarnation_name, status, claim_by_kid, COUNT(*)
		FROM apply_runs GROUP BY incarnation_name, status, claim_by_kid ORDER BY incarnation_name`)
	if err == nil {
		var dump []string
		for arRows.Next() {
			var inc, st string
			var kid *string
			var n int
			_ = arRows.Scan(&inc, &st, &kid, &n)
			k := "<nil>"
			if kid != nil {
				k = *kid
			}
			dump = append(dump, fmt.Sprintf("%s/%s@%s×%d", inc, st, k, n))
		}
		arRows.Close()
		t.Logf("DUMP apply_runs: %v", dump)
	}

	tRows, err := s.db.Query(ctx, `
		SELECT target_id, status, batch_index FROM voyage_targets
		WHERE voyage_id = $1 ORDER BY batch_index, target_id`, voyageID)
	if err == nil {
		var dump []string
		for tRows.Next() {
			var id, st string
			var bi int
			_ = tRows.Scan(&id, &st, &bi)
			dump = append(dump, fmt.Sprintf("%s/%s@b%d", id, st, bi))
		}
		tRows.Close()
		t.Logf("DUMP voyage_targets: %v", dump)
	}
}

// isVoyageTerminal — true для терминальных статусов voyages (миграция 059).
func isVoyageTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "partial_failed", "cancelled":
		return true
	default:
		return false
	}
}

// CountAuditEvents возвращает число audit_log-записей с заданным event_type,
// у которых payload->>'voyage_id' = voyageID. Для доказательства, что
// `voyage.reclaimed` реально эмитнут на crash-recovery.
func (s *Stack) CountAuditEvents(t *testing.T, eventType, voyageID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = $1 AND payload->>'voyage_id' = $2
	`, eventType, voyageID).Scan(&n)
	if err != nil {
		t.Fatalf("CountAuditEvents(%s, %s): %v", eventType, voyageID, err)
	}
	return n
}
