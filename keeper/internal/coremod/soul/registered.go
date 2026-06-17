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
// `refresh_soulprint` MVP игнорируется: сценарий-уровень scenario-runner-а
// пока не существует (M2.x). Поле принимается и эхо-выдаётся в output как
// `refreshed: false`, чтобы input-схема была стабильной.
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

// Module — реализация sdk/module.SoulModule поверх Store.
type Module struct {
	Store Store
}

// New строит модуль с переданным Store. Caller обычно даёт adapter поверх
// pgxpool — см. NewPGStore.
func New(store Store) *Module {
	return &Module{Store: store}
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
	if _, err := util.StringParam(req.Params, "sid"); err != nil {
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

	sid, err := util.StringParam(req.Params, "sid")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !keepersoul.ValidSID(sid) {
		return util.SendFailed(stream, fmt.Sprintf("invalid sid %q", sid))
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

	// refresh_soulprint принимается, но не выполняется в MVP — scenario-applier
	// keeper-стороны пока не интегрирован. Echo в output для стабильной формы.
	_, _, err = util.OptBoolParam(req.Params, "refresh_soulprint")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// replace + пустой coven — ошибка (двойная footgun-защита).
	if modeParam == ModeReplace && len(wanted) == 0 {
		return util.SendFailed(stream, "mode=replace requires non-empty coven (footgun protection: host must keep at least one coven label)")
	}

	cur, created, ferr := m.fetchOrCreate(ctx, sid)
	if ferr != nil {
		return util.SendFailed(stream, ferr.Error())
	}

	before := append([]string(nil), cur.Coven...)
	final, removed := keepersoul.ApplyCovenMode(before, wanted, keepersoul.CovenMode(modeParam))

	covenChanged := !keepersoul.CovenSetEqual(before, final)
	changed := created || covenChanged
	var saved []string
	if covenChanged {
		saved, err = m.Store.UpdateCoven(ctx, sid, final)
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("update coven: %v", err))
		}
	} else {
		saved = before
	}

	out := map[string]any{
		"sid":       sid,
		"coven":     toAnySlice(saved),
		"mode":      modeParam,
		"created":   created,
		"refreshed": false,
		"removed":   toAnySlice(removed),
	}
	return util.SendFinal(stream, changed, out)
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
