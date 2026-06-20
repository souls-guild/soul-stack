package audit

// TaskExecutedError — error-часть payload-а task.executed (заполнена только на
// терминалах FAILED/TIMED_OUT). Code — код ошибки модуля (Soul-side
// TaskError.code; keeper-side держит пусто — keeper-модули код не несут).
// Message — текст ошибки (= stderr / message модуля): для no_log-задачи он НЕ
// кладётся (BuildTaskExecutedPayload подавляет), иначе попадает в payload как
// есть, маскинг — на write-path-е (MaskSecrets в auditpg).
type TaskExecutedError struct {
	Code    string
	Module  string
	Message string
}

// TaskExecutedInput — извлечённые примитивы для сборки payload-а события
// task.executed. Заполняется обеими emit-точками (Soul-side TaskEvent-handler и
// keeper-side dispatchKeeperTasks) из своих proto/render-структур — единая форма
// payload держится в [BuildTaskExecutedPayload], чтобы свёртка changed_tasks
// (auditpg, по payload->>'sid'/'task_idx'/'status') одинаково видела задачи
// обеих сторон.
type TaskExecutedInput struct {
	SID     string
	ApplyID string
	// TaskIdx — ЛОКАЛЬНАЯ позиция задачи в ApplyRequest своего Passage (эхо
	// TaskEvent.task_idx). Под staged/per-host-where ≠ глобальному
	// RenderedTask.Index. Остаётся в payload для наблюдаемости/триажа, НЕ ключ
	// корреляции CHANGED-задач с планом — см. PlanIndex.
	TaskIdx int
	// PlanIndex — ГЛОБАЛЬНЫЙ сквозной индекс задачи по всему плану прогона (по
	// всем Passage), = RenderedTask.Index (эхо TaskEvent.plan_index, ADR-056 §S1
	// fix Variant B). Ключ корреляции CHANGED-задачи с планом в
	// auditpg.SelectChangedTaskKeys (state_changes-whitelist + audit changed_tasks).
	// Локальный TaskIdx под staged/per-host-where указывал бы на соседнюю задачу
	// (тот же дефект, что register-канал закрыл миграцией 079). keeper-side задачи
	// (`on: keeper`) исполняются до host-fan-out → PlanIndex==TaskIdx==Index.
	// N=1 / старый Soul (payload без plan_index) → корреляция fallback-ит на
	// TaskIdx, для N=1 совпадающий с глобальным.
	PlanIndex int
	// Status — строковое имя terminal-статуса задачи (keeperv1.TaskStatus.String(),
	// например "TASK_STATUS_CHANGED"). Свёртка changed фильтрует по литералу
	// "TASK_STATUS_CHANGED" — обе стороны кладут одно и то же имя enum-а.
	Status string
	// NoLog — эхо RenderedTask.no_log: подавляет error.message и register_data
	// (корень утечки произвольного секрета, который MaskSecrets по vault-ref не
	// ловит). В payload вместо них едет маркер suppressed:"no_log".
	NoLog bool
	// Error — заполнен только на FAILED/TIMED_OUT (nil иначе).
	Error *TaskExecutedError
	// RegisterData — сериализованный register-результат (Soul-side: protojson от
	// google.protobuf.Struct). Пусто → ключ не кладётся. Для no_log подавляется.
	// keeper-side register_data в audit не кладёт вовсе (секрет-гигиена) —
	// оставляет пустым.
	RegisterData string
	// Passage — индекс Passage staged-render (ADR-056), которому принадлежит
	// задача (эхо TaskEvent.passage). 0 = единственный Passage (поведение как до
	// staged-render). В payload едет всегда (включая 0) для триажа per-Passage;
	// keeper-side задачи (`on: keeper`) исполняются до host-fan-out → passage=0.
	Passage int
}

// BuildTaskExecutedPayload собирает payload audit-события task.executed из
// извлечённых примитивов — единая форма для обеих emit-точек (Soul-side
// events_taskevent.go и keeper-side keeper_dispatch.go). Держать сборку в одном
// месте критично: свёртка changed_tasks (auditpg.SelectChangedTaskKeys) читает
// payload по ключам sid/task_idx/status, и рассинхрон формы между сторонами
// молча обнулил бы её для одной из сторон.
//
// no_log-suppression (симметрично обеим сторонам): для no_log-задачи
// error.message и register_data НЕ кладутся (корень утечки произвольного
// секрета), вместо них — маркер suppressed:"no_log". Маскинг секретов по
// vault-ref/sensitive-ключам — на write-path-е (MaskSecrets в auditpg), здесь
// payload собирается «как есть» (симметрично прежней inline-сборке handler-а).
func BuildTaskExecutedPayload(in TaskExecutedInput) map[string]any {
	payload := map[string]any{
		"sid":      in.SID,
		"apply_id": in.ApplyID,
		"task_idx": in.TaskIdx,
		// plan_index (ADR-056 §S1 fix Variant B): ГЛОБАЛЬНЫЙ сквозной индекс задачи
		// по всему плану (= RenderedTask.Index). Ключ корреляции CHANGED-задачи с
		// планом в auditpg.SelectChangedTaskKeys; task_idx (локальная позиция в
		// своём Passage) под staged/per-host-where указывал бы на соседнюю задачу.
		// Кладётся всегда (аддитивно к task_idx) — старые audit-строки без него
		// читаются корреляцией с fallback на task_idx (N=1 → совпадают).
		"plan_index": in.PlanIndex,
		"status":     in.Status,
		"passage":    in.Passage,
	}
	if in.NoLog {
		payload["suppressed"] = "no_log"
	}
	if in.Error != nil {
		errPayload := map[string]any{
			"code":   in.Error.Code,
			"module": in.Error.Module,
		}
		// message (stderr) — только для не-no_log: для no_log он может нести
		// plaintext-секрет, который MaskSecrets по vault-ref не ловит.
		if !in.NoLog {
			errPayload["message"] = in.Error.Message
		}
		payload["error"] = errPayload
	}
	if in.RegisterData != "" && !in.NoLog {
		payload["register_data"] = in.RegisterData
	}
	return payload
}
