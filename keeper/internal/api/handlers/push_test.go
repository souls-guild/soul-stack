package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
)

// PushHandler-list-tests фокусируются на двух уровнях:
//   1. Валидация query (status / pagination) — доменный инвариант ListRunsTyped
//      (ValidStatus → 422, CheckPageBounds → 400), проверяется напрямую без (w,r)
//      и без реального *pushorch.PushRun (handler-native: HTTP/bind покрыт
//      huma-integration в пакете api).
//   2. Маппинг pushorch.PushRunRow → плоский PushRunListEntryView через
//      rowToPushRunListEntryView / extractSummaryCounts — на граничном конвертере.
//
// End-to-end проверка списка с реальной PG-страницей живёт в integration-тестах
// (pushorch/integration_test.go под build-tag `integration_pg`).

// pushProblemType извлекает problem.Type из ошибки *Typed-функции (nil → "").
func pushProblemType(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("ошибка не *problemError: %T %v", err, err)
	}
	return d.Type
}

func TestPush_ListRunsTyped_BadStatus_422(t *testing.T) {
	h := NewPushHandler(nil, nil)
	_, err := h.ListRunsTyped(context.Background(), []string{"bogus"}, "", 0, 50)
	if got := pushProblemType(t, err); got != problem.TypeValidationFailed {
		t.Fatalf("problem.Type = %q, want %q", got, problem.TypeValidationFailed)
	}
}

func TestPush_ListRunsTyped_BadLimit_400(t *testing.T) {
	h := NewPushHandler(nil, nil)
	_, err := h.ListRunsTyped(context.Background(), nil, "", 0, 99999)
	if got := pushProblemType(t, err); got != problem.TypeMalformedRequest {
		t.Fatalf("problem.Type = %q, want %q (out-of-range pagination → 400)", got, problem.TypeMalformedRequest)
	}
}

func TestPush_ListRunsTyped_NilSvc_500(t *testing.T) {
	// Валидный запрос: query-валидация прошла, но svc==nil → 500.
	// Имитирует production-сборку, в которой PushRun не сконфигурирован
	// (no SshDispatcher) — handler не должен паниковать на ListRows.
	h := NewPushHandler(nil, nil)
	_, err := h.ListRunsTyped(context.Background(), nil, "", 0, 50)
	if got := pushProblemType(t, err); !strings.Contains(got, "internal") {
		t.Fatalf("problem.Type = %q, want internal (svc nil)", got)
	}
}

func TestPush_ListRunsTyped_ValidStatusAndProvider_PassValidation(t *testing.T) {
	// status и ssh_provider валидны → query-валидация проходит; svc=nil →
	// дальше 500. Цель — убедиться, что валидные значения НЕ отвергаются (не 422).
	h := NewPushHandler(nil, nil)
	_, err := h.ListRunsTyped(context.Background(), []string{"success", "failed"}, "openssh", 0, 50)
	if got := pushProblemType(t, err); !strings.Contains(got, "internal") {
		t.Fatalf("problem.Type = %q, want internal (валидация прошла, svc=nil)", got)
	}
}

// --- extractSummaryCounts -------------------------------------------------

func TestExtractSummaryCounts_Nil(t *testing.T) {
	t.Parallel()
	if got := extractSummaryCounts(nil); got != nil {
		t.Errorf("nil summary → got %+v, want nil", got)
	}
	if got := extractSummaryCounts(map[string]any{}); got != nil {
		t.Errorf("empty map → got %+v, want nil", got)
	}
}

func TestExtractSummaryCounts_NoNumericFields(t *testing.T) {
	t.Parallel()
	// jsonb с hosts[] но без агрегированных counts — возвращаем nil
	// (terminal статус с только-hosts без агрегатов — теоретический edge).
	summary := map[string]any{"hosts": []any{}}
	if got := extractSummaryCounts(summary); got != nil {
		t.Errorf("non-numeric summary → got %+v, want nil", got)
	}
}

func TestExtractSummaryCounts_AllFields(t *testing.T) {
	t.Parallel()
	// json.Unmarshal -> map[string]any числовые поля кладёт как float64.
	summary := map[string]any{
		"total":         float64(5),
		"success_count": float64(3),
		"fail_count":    float64(2),
	}
	got := extractSummaryCounts(summary)
	if got == nil {
		t.Fatal("got nil, want non-nil")
	}
	if got.Total == nil || *got.Total != 5 {
		t.Errorf("Total = %v, want 5", got.Total)
	}
	if got.SuccessCount == nil || *got.SuccessCount != 3 {
		t.Errorf("SuccessCount = %v, want 3", got.SuccessCount)
	}
	if got.FailCount == nil || *got.FailCount != 2 {
		t.Errorf("FailCount = %v, want 2", got.FailCount)
	}
}

func TestExtractSummaryCounts_PartialFields(t *testing.T) {
	t.Parallel()
	// success_count есть, total/fail_count — нет. Возвращаем non-nil с *SuccessCount.
	summary := map[string]any{"success_count": float64(7)}
	got := extractSummaryCounts(summary)
	if got == nil {
		t.Fatal("got nil, want non-nil")
	}
	if got.SuccessCount == nil || *got.SuccessCount != 7 {
		t.Errorf("SuccessCount = %v, want 7", got.SuccessCount)
	}
	if got.Total != nil {
		t.Errorf("Total = %v, want nil", got.Total)
	}
	if got.FailCount != nil {
		t.Errorf("FailCount = %v, want nil", got.FailCount)
	}
}

func TestRowToPushRunListEntryView_Pending_NoFinishedAt(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	row := &pushorch.PushRunRow{
		ApplyID:       "01HABCDEFGHJKMNPQRSTVWXYZ0",
		InventorySIDs: []string{"a.example", "b.example"},
		DestinyRef:    "redis-base@v1.0.0",
		SSHProvider:   "openssh",
		CleanupStale:  false,
		Status:        pushorch.StatusPending,
		StartedAt:     startedAt,
		StartedByAID:  "archon-alice",
	}
	entry := rowToPushRunListEntryView(row)
	if entry.ApplyID != row.ApplyID {
		t.Errorf("ApplyID = %q", entry.ApplyID)
	}
	if entry.Status != "pending" {
		t.Errorf("Status = %q, want плоская строка pending", entry.Status)
	}
	if entry.FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil (pending)", entry.FinishedAt)
	}
	if entry.SummaryCounts != nil {
		t.Errorf("SummaryCounts = %+v, want nil (no summary)", entry.SummaryCounts)
	}
}

func TestRowToPushRunListEntryView_Terminal_WithCounts(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(30 * time.Second)
	row := &pushorch.PushRunRow{
		ApplyID:       "01HABCDEFGHJKMNPQRSTVWXYZ0",
		InventorySIDs: []string{"a", "b"},
		DestinyRef:    "redis@v1",
		Status:        pushorch.StatusPartialFailed,
		StartedAt:     startedAt,
		FinishedAt:    &finishedAt,
		Summary: map[string]any{
			"total":         float64(2),
			"success_count": float64(1),
			"fail_count":    float64(1),
		},
	}
	entry := rowToPushRunListEntryView(row)
	if entry.Status != "partial_failed" {
		t.Errorf("Status = %q, want плоская строка partial_failed", entry.Status)
	}
	if entry.FinishedAt == nil {
		t.Errorf("FinishedAt is nil, want timestamp")
	}
	if entry.SummaryCounts == nil {
		t.Fatal("SummaryCounts nil")
	}
	if entry.SummaryCounts.Total == nil || *entry.SummaryCounts.Total != 2 {
		t.Errorf("Total = %v", entry.SummaryCounts.Total)
	}
}
