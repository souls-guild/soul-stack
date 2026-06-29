package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ClaimDeps — зависимости claim-callback-а Acolyte-а (ADR-027, Phase 1.4.3).
// Несёт render-конвейер (через [Deps], переиспользуемый [RenderForHost]) +
// gRPC-Outbound + параметры захвата Ward. Собирается при wire-up-е (1.4.4) и
// передаётся в [NewClaimRunner]; пул дёргает [ClaimRunner.Claim] на каждом
// poll-tick-е / Summons-wake (acolyte.Pool.SetClaim).
type ClaimDeps struct {
	// Deps — те же зависимости, что у Runner-а: Loader/Topology/Essence/Render/
	// Vault/Audit/Destiny/DB + Outbound (SID-lease-маршрутизация SendApply уже
	// внутри). InputDenyPaths/Logger тоже отсюда.
	Deps Deps

	// KID — идентификатор Acolyte-владельца (claim_by_kid, fencing-epoch).
	KID string

	// Lease — TTL Ward-захвата (claim_expires_at = NOW()+Lease). Recovery-скан
	// (ADR-027 amend GATE-1) переклеймит просроченные.
	Lease time.Duration

	// Batch — максимум planned-заданий, захватываемых одним тиком (LIMIT
	// claim-запроса). Воркеры разных инстансов делят очередь через
	// FOR UPDATE SKIP LOCKED — батч лишь ограничивает аппетит одного тика.
	Batch int
}

// ClaimRunner исполняет один цикл claim→render→apply Acolyte-а. Stateless по
// прогонам (всё из PG + recipe), безопасен для конкурентного вызова несколькими
// воркерами: единственная разделяемая операция — [applyrun.ClaimNext] под
// FOR UPDATE SKIP LOCKED, гарантирующая один Acolyte на строку.
type ClaimRunner struct {
	deps   ClaimDeps
	logger *slog.Logger
}

// NewClaimRunner собирает ClaimRunner. Паникует на nil обязательных
// зависимостях / некорректных параметрах — программная ошибка wire-up-а (1.4.4),
// не runtime-условие.
func NewClaimRunner(deps ClaimDeps) *ClaimRunner {
	if deps.Deps.Loader == nil || deps.Deps.Topology == nil || deps.Deps.Essence == nil ||
		deps.Deps.Render == nil || deps.Deps.Outbound == nil || deps.Deps.DB == nil {
		panic("scenario: NewClaimRunner: required dependency is nil")
	}
	if deps.KID == "" {
		panic("scenario: NewClaimRunner: empty KID")
	}
	if deps.Lease <= 0 || deps.Batch <= 0 {
		panic("scenario: NewClaimRunner: non-positive Lease/Batch")
	}
	return &ClaimRunner{deps: deps, logger: depsLogger(deps.Deps)}
}

// Claim — один проход claim-callback-а (форма acolyte.ClaimFunc): атомарно
// клеймит пачку planned-заданий и для каждого делает render→MarkDispatched→SendApply.
// Возвращаемая ошибка — только сбой самого ClaimNext (PG недоступна): пул
// логирует её и не останавливается. Ошибки рендера/отправки отдельных заданий
// НЕ всплывают наверх — они переводят строку в failed (барьер run-goroutine
// засчитает), иначе он завис бы до runTimeout.
func (c *ClaimRunner) Claim(ctx context.Context) error {
	claimed, err := applyrun.ClaimNext(ctx, c.deps.Deps.DB, c.deps.KID, c.deps.Lease, c.deps.Batch)
	if err != nil {
		return fmt.Errorf("scenario: claim next: %w", err)
	}
	for _, run := range claimed {
		c.execute(ctx, run)
	}
	return nil
}

// execute доводит одно заклеймленное задание: render для его SID → фильтр задач
// этого хоста → (no-op no_match, если on:/where: всё отфильтровал — FINDING-01
// вариант (б)) → MarkDispatched (claimed→dispatched) СТРОГО ПЕРЕД SendApply.
// Любая ошибка render/SendApply →
// failed (masked-summary), чтобы барьер run-goroutine не висел до runTimeout.
// Отметка ДО send — инвариант против двойного apply (ADR-027 amend S3): после
// dispatched recovery-reclaim строку не трогает.
//
// Инвариант A (ADR-027): resolved input/essence/rendered params живут только в
// стеке RenderForHost (в RAM); в PG/логи/status уходит лишь masked-форма.
func (c *ClaimRunner) execute(ctx context.Context, run *applyrun.ApplyRun) {
	log := c.logger.With(
		slog.String("apply_id", run.ApplyID),
		slog.String("sid", run.SID),
		slog.Int("attempt", run.Attempt),
	)

	if run.Recipe == nil {
		// planned-задание без рецепта Acolyte отрендерить не может (программная
		// ошибка dispatch-а — InsertPlanned требует non-nil recipe). Закрываем
		// failed, чтобы барьер не висел.
		c.markFailed(ctx, run, "recipe_missing", log)
		return
	}

	tasks, plans, err := RenderForHost(ctx, c.deps.Deps, run.Recipe, run.IncarnationName, run.ApplyID, run.SID)
	if err != nil {
		// Drain-прерывание (claimCtx отменён по истечении grace, graceful-drain
		// ADR-027 amend GATE-1): НЕ помечаем задание failed — оставляем Ward как есть
		// (claimed), lease истечёт → подберёт recovery-скан (ADR-027(i)). Маркер
		// failed здесь «доел бы» задание силой и сжёг бы attempt без apply.
		if c.aborted(ctx) {
			log.Info("scenario: claim — render прерван drain-ом, Ward оставлен для recovery")
			return
		}
		// err может транзитом нести vault:secret/-ref — маскируем перед записью
		// в status (operator-facing, без маскинга наружу) и логом. Acolyte-путь
		// seal-набор не ведёт (per-host render при claim, отдельный слайс) → nil
		// sealed-пути: деградация к vault+regex слоям (ADR-010 §7.4), БИТ-В-БИТ.
		c.markFailed(ctx, run, maskErrText(err, nil), log)
		return
	}

	// Задачи, таргетящие именно этот SID (после on:/where: в RenderForHost).
	hostTasks := groupByHost(tasks, plans)[run.SID]
	if len(hostTasks) == 0 {
		// on:/where: отфильтровал все задачи на этом хосте: хост нецелевой для
		// прогона. FINDING-01 вариант (б) — терминал `no_match`, НЕ `success`:
		// apply_runs больше не over-reports «успех там, где ничего не
		// применялось» (Acolyte-путь пишет planned на КАЖДЫЙ roster-хост ДО
		// per-host резолва on:/where:). UpdateStatus проставит finished_at.
		// Барьер засчитает no_match как benign-терминал (как success) → прогон
		// идёт в ready, не error_locked.
		if err := applyrun.UpdateStatus(ctx, c.deps.Deps.DB, run.ApplyID, run.SID, run.Passage, applyrun.StatusNoMatch, nil); err != nil {
			if errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) {
				log.Info("scenario: claim no-op no_match — single-winner no-op, первый коммиттер победил")
				return
			}
			log.Error("scenario: claim no-op no_match не записан", slog.Any("error", err))
			return
		}
		log.Info("scenario: claim — ни одна задача не таргетит хост, no-op no_match")
		return
	}

	// Cancel-окно planned/claimed (ADR-027 cutover, minor-фикс): между claim-ом
	// и SendApply мог встать cluster-wide Cancel-флаг (RequestCancel теперь бьёт
	// и по planned/claimed). Свежий read ПЕРЕД отправкой: если Cancel запрошен —
	// apply на Soul НЕ уходит, задание переводится в терминал cancelled (барьер
	// run-goroutine засчитает его как не-success и отменит прогон). Отмена ДО
	// SendApply безопасна — на хост ничего не отправлено. PG-read-ошибку трактуем
	// fail-open (не отменяем): редкая, и пропуск отмены безопаснее ложной отмены
	// уже валидного apply — повторный Cancel/recovery её добьёт.
	if cancelled, cerr := applyrun.SelectCancelRequested(ctx, c.deps.Deps.DB, run.ApplyID, run.SID); cerr != nil {
		log.Warn("scenario: claim — чтение cancel_requested провалено, продолжаем apply", slog.Any("error", cerr))
	} else if cancelled {
		if err := applyrun.UpdateStatus(ctx, c.deps.Deps.DB, run.ApplyID, run.SID, run.Passage, applyrun.StatusCancelled, nil); err != nil {
			if errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) {
				log.Info("scenario: claim cancelled — single-winner no-op, первый коммиттер победил")
				return
			}
			log.Error("scenario: claim — cancelled-статус не записан после Cancel в claim-окне", slog.Any("error", err))
			return
		}
		log.Info("scenario: claim — Cancel запрошен до SendApply, apply не отправлен, задание отменено")
		return
	}

	// Drain-прерывание ПЕРЕД отметкой dispatched: если claim-ctx отменён
	// drain-ом — НЕ отмечаем dispatched и НЕ шлём, оставляем Ward как есть
	// (claimed), lease истечёт → подберёт recovery-скан (ADR-027(i)). Ничего не
	// отдано — двойного apply нет.
	if c.aborted(ctx) {
		log.Info("scenario: claim — drain до отметки dispatched, Ward оставлен для recovery")
		return
	}

	// claimed → dispatched СТРОГО ПЕРЕД SendApply (deliver-once intent-маркер,
	// ADR-027 amend S3). Это сердце инварианта против двойного apply: как только
	// строка dispatched, recovery-reclaim её НЕ трогает (reclaim сужен до
	// status='claimed', S4). Если MarkDispatched упал (PG-сбой) — НЕ шлём apply,
	// оставляем Ward в claimed (recovery пере-claim-ит); ничего не отдано —
	// двойного apply нет. Гонка/повторный перевод отсекается фильтром
	// status='claimed' внутри MarkDispatched.
	if err := applyrun.MarkDispatched(ctx, c.deps.Deps.DB, run.ApplyID, run.SID); err != nil {
		log.Error("scenario: claimed → dispatched не записан, SendApply не вызван (Ward оставлен claimed для recovery)", slog.Any("error", err))
		return
	}

	// ApplyRequest несёт attempt = run.Attempt (fencing-epoch, ADR-027(g)):
	// ClaimNext инкрементил его при захвате Ward (claimNextSQL: attempt = r.attempt+1),
	// так что пере-claim протухшего задания приедет с большим attempt и Soul-guard
	// (приём RunResult) отсечёт оригинальный stale-дубль. SendApply дополнительно
	// маршрутизирует по SID-lease (apply только через владельца стрима — первый
	// слой защиты от двойного исполнения).
	//
	// DryRun=true (Scry, ADR-031) — Acolyte-путь для check-drift: Soul зовёт
	// Plan вместо Apply. Поле проброса из persisted Recipe.DryRun (мутация
	// контракта Recipe forward-compat: пустое поле в старых рецептах = false).
	// Двойной dry_run безопасен (Plan read-only), поэтому Cancel-окно /
	// fencing-эпоха работают тем же путём без особых веток.
	req := &keeperv1.ApplyRequest{
		ApplyId: run.ApplyID,
		// ToProtoTasksForHost(run.SID): per-host render_context ЭТОГО хоста для
		// self-вариативной core.file.rendered (open Q №25, render_context.self).
		// Acolyte рендерит полный roster (RenderForHost) — RenderContextBySID
		// заполнен теми же per-host вариантами, что и в run-goroutine-пути.
		Tasks:   render.ToProtoTasksForHost(hostTasks, run.SID),
		Attempt: int32(run.Attempt),
		DryRun:  run.Recipe.DryRun,
	}
	if err := c.deps.Deps.Outbound.SendApply(ctx, run.SID, req); err != nil {
		// SendApply вернул ошибку: доставка НЕ ПОДТВЕРЖДЕНА (сетевой сбой мог
		// прийти и ПОСЛЕ транзита — хост мог задание получить). Терминалим failed
		// (updateStatusSQL допускает dispatched→failed) — это безопасно даже при
		// возможной фактической доставке: fencing по attempt + single-winner
		// отсекут дубль, а reclaim терминал не трогает. Drain-прерывание тоже сюда
		// (на отменённом ctx закрыть терминалом безопаснее, чем оставлять висеть
		// dispatched, который recovery уже не подберёт). req несёт зарезолвленные
		// Params — НЕ эхаем их в status; safe-причина.
		c.markFailed(ctx, run, "send_apply_failed", log)
		return
	}

	// KNOWN GAP устранён: старый разрыв «строка осталась claimed после SendApply,
	// recovery пере-claim-ит → ДВОЙНОЙ SendApply» больше невозможен — отметка
	// (claimed→dispatched) теперь СТРОГО ДО send, reclaim берёт только claimed.
	//
	// Новый известный БЕЗОПАСНЫЙ gap W-a (ADR-027 amend): MarkDispatched прошёл,
	// но Keeper упал ДО SendApply — строка dispatched, на хост ничего не
	// отдано, RunResult не придёт → задание висит. Это НЕ двойной apply.
	// Закрытие — Soul-reconcile (post-MVP, Вариант А): Soul при reconnect
	// сообщает свои in-flight apply_id, Keeper реконсилит зависшие dispatched.
	log.Info("scenario: ApplyRequest отправлен (claim, dispatched)", slog.Int("tasks", len(hostTasks)))
}

// aborted сообщает, что ctx claim-а отменён — claimCtx пула отменён Shutdown-ом
// по истечении drain-grace (graceful-drain пула Acolyte, ADR-027 amend GATE-1). На
// таком прерывании ДО отметки dispatched (render-ветка / pre-mark drain-check)
// заклеймленное задание НЕ переводится в терминал: его Ward остаётся claimed,
// attempt/lease не трогаются, lease истекает → recovery-скан возвращает задание
// в очередь (ADR-027(i)). Отличает drain-abort от доменной ошибки render,
// которая штатно ведёт в failed.
func (c *ClaimRunner) aborted(ctx context.Context) bool {
	return ctx.Err() != nil
}

// markFailed переводит заклеймленное задание в failed с уже-безопасной summary
// (без раскрытого секрета). summary читается наружу через barrier/status_details
// без маскинга — caller обязан передать масштабированную/нейтральную строку.
func (c *ClaimRunner) markFailed(ctx context.Context, run *applyrun.ApplyRun, summary string, log *slog.Logger) {
	if err := applyrun.UpdateStatus(ctx, c.deps.Deps.DB, run.ApplyID, run.SID, run.Passage, applyrun.StatusFailed, &summary); err != nil {
		if errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) {
			log.Info("scenario: claim failed — single-winner no-op, первый коммиттер победил",
				slog.String("summary", summary))
			return
		}
		log.Error("scenario: claim failed-статус не записан — барьер может зависнуть до timeout",
			slog.String("summary", summary), slog.Any("error", err))
		return
	}
	log.Warn("scenario: claim — задание провалено", slog.String("summary", summary))
}
