// Package soul реализует keeper-side core-модуль `core.soul.registered`
// (ADR-017, docs/keeper/modules.md).
//
// Состояние:
//   - registered: декларативная форма «Soul с указанным sid находится в
//     реестре и привязан к указанному набору Coven-меток».
//
// Mode-семантика:
//   - append (default): existing ∪ переданные.
//   - replace: переданные (пустой набор — ошибка, footgun-защита).
//   - remove: existing \ переданные.
//
// Side-effect: если записи в `souls` для sid нет — модуль создаёт её под
// status: pending (новый хост, добавленный сценарием — host-ветка add_replica
// или после cloud-provision). Bootstrap-токены/SoulSeed не выписывает —
// это компетенция онбординга.
//
// `sid` принимает строку ИЛИ список строк (ADR-061): одиночный SID остаётся
// валиден (обратная совместимость), список — регистрация+ожидание N созданных
// хостов одним шагом-барьером. `coven` применяется ко всем SID списка.
//
// Барьер онбординга (ADR-061): при `await_online: true` после регистрации
// всех SID шаг блокирующе поллит presence (Redis SID-lease через
// PresenceChecker) до `await_min_count` online или `await_timeout`. B1-strict:
// недобор кворума к таймауту → шаг failed (см. await.go).
//
// `refresh_soulprint` (ADR-061 §S2/§S3 — оживлён): при `true` шаг становится
// passage-определяющей границей (Stratify), а scenario-runner ПОСЛЕ его успеха
// пере-резолвит roster перед следующим Passage (run.go stage-loop). Output несёт
// `refreshed` = значение флага (true ⇒ re-resolve гарантированно выполнится).
package soul

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — base-имя модуля без state-суффикса (ключ Registry). Author-форма
// адреса задачи — `core.soul.registered` (base + state, как core-модули
// Soul-side); state `registered` приходит в pluginv1.ApplyRequest.state.
const Name = "core.soul"

// Mode-значения. Совпадают с docs/keeper/modules.md → семантика mode.
const (
	ModeAppend  = "append"
	ModeReplace = "replace"
	ModeRemove  = "remove"
)

// Store — узкое подмножество keeper/internal/soul, нужное модулю.
// Полный pgxpool наружу не подсовываем — это упрощает unit-тестирование
// (fake реализует только три метода) и фиксирует контракт.
type Store interface {
	SelectBySID(ctx context.Context, sid string) (*keepersoul.Soul, error)
	Insert(ctx context.Context, s *keepersoul.Soul) error
	UpdateCoven(ctx context.Context, sid string, coven []string) ([]string, error)
}

// PresenceChecker — узкая поверхность batch-проверки «жив ли Redis SID-lease»
// (presence=online, ADR-006(a)/ADR-061), нужная барьеру онбординга
// `await_online`. Сужение до одного метода изолирует модуль от полного
// keeperredis.Client и допускает fake в unit-тестах; реальная реализация —
// обёртка над keeperredis.SoulsStreamAlive, собранная в cmd/keeper
// (симметрично topology.SoulLeaseChecker).
//
// Источник истины «online» — именно lease (живой EventStream), а НЕ PG
// souls.status (lifecycle-снимок, отстаёт): барьер не должен считать хост
// online до фактического стрима.
type PresenceChecker interface {
	SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error)
}

// Module — реализация sdk/module.SoulModule поверх Store.
//
// presence/maxAwaitTimeout заполняются опционально через WithPresence — нужны
// только барьеру онбординга (`await_online`, ADR-061). Без них шаг работает
// как до ADR-061 (регистрация без барьера); запрос `await_online: true` без
// сконфигурированного presence-checker-а завершается failed (барьер не может
// работать без источника presence — молчаливый success недопустим).
type Module struct {
	Store    Store
	presence PresenceChecker
	// maxAwaitTimeout — провайдер строкового потолка await_timeout из текущего
	// snapshot keeper.yml (hot-reload: читается на каждом Apply). nil → дефолт
	// config.DefaultMaxAwaitTimeout. Функция (а не значение): config.Store.Get()
	// меняется при reload.
	maxAwaitTimeout func() string
}

// New строит модуль с переданным Store. Caller обычно даёт adapter поверх
// pgxpool — см. NewPGStore.
func New(store Store) *Module {
	return &Module{Store: store}
}

// WithPresence подключает presence-checker (Redis SID-lease) и провайдер
// потолка await_timeout для барьера онбординга (ADR-061). maxAwaitTimeout —
// функция, возвращающая текущее строковое значение keeper.yml::max_await_timeout
// (hot-reload-aware); nil-функция или пустая строка → config.DefaultMaxAwaitTimeout.
func (m *Module) WithPresence(p PresenceChecker, maxAwaitTimeout func() string) *Module {
	m.presence = p
	m.maxAwaitTimeout = maxAwaitTimeout
	return m
}

// Validate проверяет state и обязательные параметры. Запускается host-ом
// до Apply; ошибки наружу как `ValidateReply.errors[]`, не как gRPC-error.
//
// НЕ делегирован в manifest-проверку (как url/repo на Soul-side): сверх
// known-state + required(sid/coven) у модуля есть enum `mode` (append/replace/
// remove), которого урезанный plugin.InputParamDef DSL не выражает. soul-lint
// валидирует author-форму статически по shared/coremanifest/soul.yaml; этот
// метод — runtime-страховка. (keeper-side util не несёт ValidateAgainstManifest —
// его coremanifest-фасад живёт в Soul-side util; дублировать ради одного модуля
// сейчас избыточно.)
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "registered" {
		errs = append(errs, fmt.Sprintf("unknown state %q (want registered)", req.State))
	}
	sids, err := util.StringOrSliceParam(req.Params, "sid")
	if err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringSliceParam(req.Params, "coven"); err != nil {
		errs = append(errs, err.Error())
	}
	if mode, err := util.OptStringParam(req.Params, "mode"); err != nil {
		errs = append(errs, err.Error())
	} else if mode != "" && !isValidMode(mode) {
		errs = append(errs, fmt.Sprintf("param %q: unknown mode (want append/replace/remove)", "mode"))
	}
	errs = append(errs, validateAwaitParams(req.Params, len(sids))...)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op в MVP (симметрично Soul-side core-модулям).
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// Apply применяет состояние registered. Все ошибки уходят как failed-event
// (не gRPC-error), чтобы scenario-applier видел их через ApplyEvent.failed
// и зашёл в `onfail:`-ветку.
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	if req.State != "registered" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}

	// sid принимает строку ИЛИ список (ADR-061). Одиночная строка нормализуется
	// в список из одного элемента; форма output (агрегат vs одиночный) выбирается
	// по len(sids) в самом конце.
	sids, err := util.StringOrSliceParam(req.Params, "sid")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	for _, sid := range sids {
		if !keepersoul.ValidSID(sid) {
			return util.SendFailed(stream, fmt.Sprintf("invalid sid %q", sid))
		}
	}
	wanted, err := util.StringSliceParam(req.Params, "coven")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Симметрия с API-границей (POST /v1/souls → soul.ValidCoven): в souls.coven
	// попадают только kebab-case-метки. Без этой проверки мусор вроде "Prod"/"a_b"
	// через scenario-шаг тихо персистился бы в реестр.
	for _, c := range wanted {
		if !keepersoul.ValidCoven(c) {
			return util.SendFailed(stream, fmt.Sprintf("invalid coven %q (want kebab-case, 1..63)", c))
		}
	}

	modeParam, err := util.OptStringParam(req.Params, "mode")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if modeParam == "" {
		modeParam = ModeAppend
	}
	if !isValidMode(modeParam) {
		return util.SendFailed(stream, fmt.Sprintf("unknown mode %q (want append/replace/remove)", modeParam))
	}

	// refresh_soulprint (ADR-061 §S3 — оживлён). При true scenario-runner ПОСЛЕ
	// успеха этого шага пере-резолвит roster перед СЛЕДУЮЩИМ Passage (S2 уже сделала
	// шаг passage-определяющим, S3 исполняет re-resolve в run.go stage-loop). Поэтому
	// echo refreshed = значение флага: true ⇒ re-resolve гарантированно выполнится
	// (созданные+онбордившиеся хосты войдут в roster последующих Passage). false /
	// отсутствие ⇒ refreshed:false (поведение до ADR не меняется).
	refreshSoulprint, _, err := util.OptBoolParam(req.Params, "refresh_soulprint")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// Барьер онбординга (ADR-061): разбор+валидация ДО любых side-effect-ов в
	// souls — недостижимый кворум / превышение потолка не должны оставлять
	// частичную регистрацию (fail-fast на параметрах барьера).
	awaitCfg, aerr := m.parseAwait(req.Params, len(sids))
	if aerr != nil {
		return util.SendFailed(stream, aerr.Error())
	}

	// replace + пустой coven — ошибка (двойная footgun-защита).
	if modeParam == ModeReplace && len(wanted) == 0 {
		return util.SendFailed(stream, "mode=replace requires non-empty coven (footgun protection: host must keep at least one coven label)")
	}

	// Регистрация всех SID (общий набор coven применяется к каждому).
	anyCreated := false
	anyChanged := false
	var savedFirst, removedFirst []string
	for i, sid := range sids {
		res, rerr := m.registerOne(ctx, sid, wanted, keepersoul.CovenMode(modeParam))
		if rerr != nil {
			return util.SendFailed(stream, rerr.Error())
		}
		anyCreated = anyCreated || res.created
		anyChanged = anyChanged || res.created || res.covenChanged
		if i == 0 {
			savedFirst, removedFirst = res.saved, res.removed
		}
	}

	// Барьер: блокирующе ждём presence по всем SID до min_count/timeout.
	var online, pending []string
	satisfied := true
	if awaitCfg != nil {
		var lastErr error
		online, pending, satisfied, lastErr, err = m.awaitOnline(ctx, sids, awaitCfg)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
		if !satisfied {
			// B1-strict: недобор кворума к таймауту → failed (fail-stop прогона,
			// state не коммитится → error_locked).
			msg := fmt.Sprintf(
				"onboarding barrier: %d/%d souls online to await_min_count=%d within %s (pending: %v)",
				len(online), len(sids), awaitCfg.minCount, awaitCfg.timeout, pending)
			// Persistent presence-сбой на последних опросах: иначе infra-проблема
			// (redis недоступен) маскируется под «хосты не онбордились».
			if lastErr != nil {
				msg += fmt.Sprintf(" (last presence error: %v)", lastErr)
			}
			return util.SendFailed(stream, msg)
		}
	}

	out := buildOutput(sids, savedFirst, modeParam, anyCreated, removedFirst, refreshSoulprint, awaitCfg != nil, online, pending, satisfied)
	return util.SendFinal(stream, anyChanged, out)
}

// registerResult — итог регистрации одного SID.
type registerResult struct {
	created      bool
	covenChanged bool
	saved        []string
	removed      []string
}

// registerOne создаёт/обновляет souls-запись одного SID и применяет coven-mode.
func (m *Module) registerOne(ctx context.Context, sid string, wanted []string, mode keepersoul.CovenMode) (registerResult, error) {
	cur, created, ferr := m.fetchOrCreate(ctx, sid)
	if ferr != nil {
		return registerResult{}, ferr
	}
	before := append([]string(nil), cur.Coven...)
	final, removed := keepersoul.ApplyCovenMode(before, wanted, mode)
	covenChanged := !keepersoul.CovenSetEqual(before, final)
	saved := before
	if covenChanged {
		var err error
		saved, err = m.Store.UpdateCoven(ctx, sid, final)
		if err != nil {
			return registerResult{}, fmt.Errorf("update coven %q: %w", sid, err)
		}
	}
	return registerResult{created: created, covenChanged: covenChanged, saved: saved, removed: removed}, nil
}

// buildOutput собирает register-payload. Одиночный SID сохраняет историческую
// форму (`sid` строкой, `coven`/`removed` от единственного хоста); список —
// `sid` массивом. Поля барьера (online/pending/satisfied) добавляются только
// при await.
func buildOutput(sids, savedFirst []string, mode string, created bool, removedFirst []string, refreshed, awaited bool, online, pending []string, satisfied bool) map[string]any {
	out := map[string]any{
		"mode":      mode,
		"created":   created,
		"refreshed": refreshed,
		"coven":     toAnySlice(savedFirst),
		"removed":   toAnySlice(removedFirst),
	}
	if len(sids) == 1 {
		out["sid"] = sids[0]
	} else {
		out["sid"] = toAnySlice(sids)
	}
	if awaited {
		out["online"] = toAnySlice(online)
		out["pending"] = toAnySlice(pending)
		out["satisfied"] = satisfied
	}
	return out
}

// fetchOrCreate возвращает текущую запись souls; если её нет — создаёт под
// status: pending с пустым coven (модуль сам потом обновит coven через
// UpdateCoven, см. Apply).
func (m *Module) fetchOrCreate(ctx context.Context, sid string) (*keepersoul.Soul, bool, error) {
	got, err := m.Store.SelectBySID(ctx, sid)
	if err == nil {
		return got, false, nil
	}
	if !errors.Is(err, keepersoul.ErrSoulNotFound) {
		return nil, false, fmt.Errorf("lookup soul %q: %w", sid, err)
	}
	// Создаём pending-запись. Поля LastSeenAt/CreatedByAID — nil: cloud-provision
	// или scenario-host-add не несут оператора (это keeper-internal action).
	stub := &keepersoul.Soul{
		SID:       sid,
		Transport: keepersoul.TransportAgent,
		Status:    keepersoul.StatusPending,
		Coven:     []string{},
	}
	if err := m.Store.Insert(ctx, stub); err != nil {
		return nil, false, fmt.Errorf("create soul %q: %w", sid, err)
	}
	return stub, true, nil
}

// isValidMode — closed-enum проверка mode-строки модуля. Делегирует
// в keeper-side soul.ValidCovenMode (единый словарь режимов; рефактор-пилот
// bulk coven-assign вынес set-семантику в keeper/internal/soul).
func isValidMode(m string) bool {
	return keepersoul.ValidCovenMode(keepersoul.CovenMode(m))
}

// toAnySlice — конверт []string → []any для structpb.NewStruct.
// structpb не умеет list of string напрямую: только list of any с auto-coerce.
func toAnySlice(xs []string) []any {
	if xs == nil {
		return []any{}
	}
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
