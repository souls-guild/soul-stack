package grpc

import (
	"context"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// handleErrandResult — обработчик payload-а [keeperv1.ErrandResult]
// (ADR-033, slice E2). Симметричен handleRunResult, но БЕЗ
// state_changes / apply_runs / incarnation-commit-а: Errand НЕ мутирует
// incarnation.state (ADR-033 §4).
//
// Обязанности:
//
//  1. Нормализовать stdout/stderr/output через secret-masking + cap 64 KiB
//     (defense-in-depth: Soul-side errand-runner делает то же, но keeper —
//     write-path для журналов/audit).
//  2. Опубликовать ResultEvent в applybus с kind=KindErrand* (terminal-
//     mapping по статусу) → Dispatcher.waitForResult / SSE-subscriber
//     (если позже подключим UI-стрим) получат событие.
//
// Audit-events `errand.completed`/`errand.failed`/`errand.timed_out` пишет
// Dispatcher из waitForResult — он же владеет инициатором (StartedByAID) и
// корреляцией. Здесь, в gRPC-handler-е, информации об инициаторе нет
// (proto несёт только errand_id), писать audit преждевременно — получится
// событие без archon_aid.
//
// ApplyBus=nil (dev-сборка без applybus) → drop (диагностика в логах).
// Аналогично handleRunResult — handler не валит стрим из-за best-effort
// pub/sub-канала.
func (h *eventStreamHandler) handleErrandResult(ctx context.Context, sid, sessionID string, ev *keeperv1.ErrandResult) {
	_ = ctx // ctx используется только для audit, который здесь не пишется.
	if ev == nil {
		h.logger.Warn("eventstream: ErrandResult payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	errandID := ev.GetErrandId()
	if errandID == "" {
		h.logger.Warn("eventstream: ErrandResult без errand_id",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if h.deps.ApplyBus == nil {
		h.logger.Debug("eventstream: ErrandResult без ApplyBus — drop",
			slog.String("sid", sid),
			slog.String("errand_id", errandID))
		return
	}

	status := errand.StatusFromProto(ev.GetStatus())
	kind := errandKindFromStatus(status)

	// Secret-masking + cap. Soul-side errand-runner делает то же, но
	// keeper — write-path для журналов/audit/SSE: дублируем как защиту
	// от случайного proto-bypass-а или ранней-pre-cap-сборки клиента.
	stdoutMasked, stdoutTrunc := errand.MaskAndCapBytes(ev.GetStdout())
	stderrMasked, stderrTrunc := errand.MaskAndCapBytes(ev.GetStderr())
	// Эхо: если Soul уже отрезал — флаг с обеих сторон выставляем.
	if ev.GetStdoutTruncated() {
		stdoutTrunc = true
	}
	if ev.GetStderrTruncated() {
		stderrTrunc = true
	}

	var output map[string]any
	if out := ev.GetOutput(); out != nil {
		output = errand.MaskOutputMap(out.AsMap())
	}

	exitCode := ev.GetExitCode()
	duration := ev.GetDurationMs()

	payload := errand.ResultEvent{
		ErrandID:        errandID,
		Status:          status,
		ExitCode:        &exitCode,
		Stdout:          stdoutMasked,
		Stderr:          stderrMasked,
		StdoutTruncated: stdoutTrunc,
		StderrTruncated: stderrTrunc,
		DurationMs:      &duration,
		ErrorMessage:    ev.GetErrorMessage(),
		Output:          output,
	}

	h.deps.ApplyBus.Publish(applybus.Event{
		ApplyID: errandID,
		Kind:    kind,
		Payload: payload,
	})
}

// errandKindFromStatus — терминальный EventKind для applybus-publish-а.
// Несвязные с терминалом статусы (RUNNING) сюда попадать не должны (Soul
// шлёт ErrandResult только финально); defensive default — failed.
func errandKindFromStatus(s errand.Status) applybus.EventKind {
	switch s {
	case errand.StatusSuccess:
		return applybus.KindErrandCompleted
	case errand.StatusTimedOut:
		return applybus.KindErrandTimedOut
	case errand.StatusCancelled:
		return applybus.KindErrandCancelled
	case errand.StatusModuleNotAllowed:
		return applybus.KindErrandModuleNotAllowed
	default:
		return applybus.KindErrandFailed
	}
}
