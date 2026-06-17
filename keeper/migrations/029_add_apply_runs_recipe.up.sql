-- 029_add_apply_runs_recipe.up.sql
--
-- ADR-027(c)(f) Phase 1: Acolyte рендерит задание для одного хоста just-in-time
-- при claim, воспроизводя render-шаги из «рецепта» в PG, а НЕ из памяти
-- run-goroutine (которая живёт только на одном инстансе и не переживает
-- cross-Keeper-роутинг / рестарт). Рецепт — это то, что нужно Acolyte-у, чтобы
-- повторить путь vault-resolve → input-validation → CEL-render →
-- text/template-render → ApplyRequest для своего SID-а. Целевая форма —
-- keeper/internal/applyrun/recipe.go (тип Recipe) и storage.md (закоммичено
-- с ADR-027).
--
-- Инвариант A (ADR-027): рецепт несёт vault-ref КАК ЕСТЬ (строки), секреты НЕ
-- раскрыты. В Input хранится `incarnation.spec.input` оператора со строковыми
-- `vault:`-ссылками; раскрытые секреты / essence / RenderedTask / ApplyRequest
-- в рецепт НЕ кладутся — Acolyte резолвит их в RAM при claim и отдаёт Soul-у,
-- в PG они не оседают.
--
-- Колонка nullable (аддитивно, forward-only ADR-007): существующие строки и
-- старый путь Insert(running) рецепт НЕ несут → NULL. Новый Acolyte-путь
-- (planned-задание под claim) требует non-NULL recipe — это инвариант кода
-- (claim-логика Phase 1.4.2/1.4.3), не схемы; на уровне DDL колонка остаётся
-- nullable, чтобы миграция и старый путь работали без ошибок.

ALTER TABLE apply_runs
    ADD COLUMN recipe JSONB;

COMMENT ON COLUMN apply_runs.recipe IS
    'Recipe (ADR-027(c)(f)): render-инструкция для just-in-time-рендера задания Acolyte-ом при claim (ServiceRef / ScenarioName / Input). Инвариант A: Input несёт vault-ref СТРОКАМИ, секреты НЕ раскрыты; essence/RenderedTask/ApplyRequest сюда НЕ кладутся. NULL для строк старого пути Insert(running).';
