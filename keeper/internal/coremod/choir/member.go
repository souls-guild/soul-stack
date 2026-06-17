// Package choir реализует keeper-side core-модуль `core.choir`
// (ADR-044, паттерн `core.soul.registered` из ADR-017).
//
// Author-форма адреса задачи — `core.choir.present` / `core.choir.absent`
// (base `core.choir` + state, как `core.file.present`/`core.file.absent`
// Soul-side). Декларируемая сущность — членство «SID является Voice-ом в
// указанном Choir-е данной инкарнации» (declared-партия хора, ADR-044 пункт 2).
//
// State-семантика (симметрия present/absent остальных core-модулей):
//   - present (default): AddVoice — SID становится Voice-ом Choir-а.
//     Идемпотентно: Voice уже есть → changed=false, не ошибка.
//   - absent: RemoveVoice — членство снимается. Идемпотентно: Voice-а нет →
//     changed=false, не ошибка.
//
// Инвариант членства (ADR-044 пункт 3 — Voice только для SID, который уже член
// инкарнации) НЕ дублируется здесь: он реализован в choir-CRUD (AddVoice →
// ErrNotMembers) и переиспользуется. При ErrNotMembers Apply отдаёт failed-event
// (прогон уходит в onfail/error_locked).
//
// ОГРАНИЧЕНИЯ S-T5 (future, не реализовано здесь):
//   - Cross-incarnation guard (param.incarnation == run-context incarnation):
//     run-context модулю недоступен (ADR-044/architect A1). Модуль доверяет
//     param `incarnation` и лишь валидирует его существование. Жёсткий guard —
//     отдельная задача (RunContext-инъекция в keeper-dispatch, needs_architect).
//   - Roster-growth (новый Voice виден следующему шагу прогона) — не реализовано.
package choir

import (
	"context"
	"errors"
	"fmt"

	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — base-имя модуля без state-суффикса (ключ Registry). Author-форма
// адреса задачи — `core.choir.present` / `core.choir.absent`.
const Name = "core.choir"

// State-значения (симметрия present/absent с Soul-side core-модулями).
const (
	StatePresent = "present"
	StateAbsent  = "absent"
)

// Store — узкое подмножество choir-CRUD + проверка существования инкарнации,
// нужное модулю. Полный pgxpool наружу не подсовываем (как у core.soul.registered):
// fake реализует только три метода, контракт явный.
//
// AddVoice/RemoveVoice — обёртки над одноимёнными package-функциями choir
// (S-T2). IncarnationExists — лёгкая проверка существования инкарнации для
// absent-ветки (present косвенно покрыт FK choir→incarnation внутри AddVoice).
type Store interface {
	AddVoice(ctx context.Context, v *keeperchoir.Voice) error
	RemoveVoice(ctx context.Context, incarnation, choirName, sid string) error
	IncarnationExists(ctx context.Context, incarnation string) (bool, error)
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

// Validate проверяет state и обязательные параметры. Запускается до Apply;
// ошибки наружу как ValidateReply.errors[], не как gRPC-error.
//
// Required: incarnation, choir, sid. Optional: role, position (int >= 0),
// state (present/absent; пусто → present). soul-lint валидирует author-форму
// статически; этот метод — runtime-страховка (как у core.soul.registered).
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "" && !isKnownState(req.State) {
		errs = append(errs, fmt.Sprintf("unknown state %q (want present/absent)", req.State))
	}
	if _, err := util.StringParam(req.Params, "incarnation"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringParam(req.Params, "choir"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringParam(req.Params, "sid"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptStringParam(req.Params, "role"); err != nil {
		errs = append(errs, err.Error())
	}
	if pos, ok, err := util.OptIntParam(req.Params, "position"); err != nil {
		errs = append(errs, err.Error())
	} else if ok && pos < 0 {
		errs = append(errs, fmt.Sprintf("param %q: must be >= 0, got %d", "position", pos))
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op в MVP (симметрично остальным core-модулям).
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// Apply применяет состояние present/absent. Все ошибки уходят как failed-event (не
// gRPC-error), чтобы scenario-applier зашёл в onfail-ветку.
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	state := req.State
	if state == "" {
		state = StatePresent
	}
	if !isKnownState(state) {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q (want present/absent)", req.State))
	}

	incarnation, err := util.StringParam(req.Params, "incarnation")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	choirName, err := util.StringParam(req.Params, "choir")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !keeperchoir.ValidChoirName(choirName) {
		return util.SendFailed(stream, fmt.Sprintf("invalid choir name %q", choirName))
	}
	sid, err := util.StringParam(req.Params, "sid")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !keepersoul.ValidSID(sid) {
		return util.SendFailed(stream, fmt.Sprintf("invalid sid %q", sid))
	}

	role, err := util.OptStringParam(req.Params, "role")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	position, posSet, err := util.OptIntParam(req.Params, "position")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if posSet && position < 0 {
		return util.SendFailed(stream, fmt.Sprintf("param %q: must be >= 0, got %d", "position", position))
	}

	// S-T5 substitute жёсткого cross-incarnation guard: явно валидируем, что
	// param-инкарнация существует. present косвенно покрыт FK choir→incarnation
	// внутри AddVoice, но absent (RemoveVoice — единичный DELETE без FK-захода)
	// иначе тихо вернул бы ErrVoiceNotFound на опечатке имени инкарнации.
	exists, err := m.Store.IncarnationExists(ctx, incarnation)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("check incarnation %q: %v", incarnation, err))
	}
	if !exists {
		return util.SendFailed(stream, fmt.Sprintf("incarnation %q not found", incarnation))
	}

	switch state {
	case StatePresent:
		return m.applyPresent(ctx, stream, incarnation, choirName, sid, role, position, posSet)
	case StateAbsent:
		return m.applyAbsent(ctx, stream, incarnation, choirName, sid)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", state))
	}
}

// applyPresent добавляет Voice. ErrVoiceExists → идемпотентный no-op
// (changed=false). ErrNotMembers (инвариант членства, ADR-044 пункт 3) →
// failed-event (прогон в error_locked). ErrChoirNotFound → failed.
func (m *Module) applyPresent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], incarnation, choirName, sid, role string, position int64, posSet bool) error {
	v := &keeperchoir.Voice{
		IncarnationName: incarnation,
		ChoirName:       choirName,
		SID:             sid,
	}
	if role != "" {
		v.Role = &role
	}
	if posSet {
		p := int(position)
		v.Position = &p
	}

	err := m.Store.AddVoice(ctx, v)
	switch {
	case err == nil:
		return util.SendFinal(stream, true, presentOutput(incarnation, choirName, sid, true))
	case errors.Is(err, keeperchoir.ErrVoiceExists):
		// Идемпотентность: Voice уже есть → ничего не меняли.
		return util.SendFinal(stream, false, presentOutput(incarnation, choirName, sid, false))
	default:
		// ErrNotMembers / ErrChoirNotFound / прочее — failed-event.
		return util.SendFailed(stream, fmt.Sprintf("add voice %q to choir %q/%q: %v", sid, incarnation, choirName, err))
	}
}

// applyAbsent снимает Voice. ErrVoiceNotFound → идемпотентный no-op
// (changed=false). Прочие ошибки → failed-event.
func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], incarnation, choirName, sid string) error {
	err := m.Store.RemoveVoice(ctx, incarnation, choirName, sid)
	switch {
	case err == nil:
		return util.SendFinal(stream, true, absentOutput(incarnation, choirName, sid, true))
	case errors.Is(err, keeperchoir.ErrVoiceNotFound):
		return util.SendFinal(stream, false, absentOutput(incarnation, choirName, sid, false))
	default:
		return util.SendFailed(stream, fmt.Sprintf("remove voice %q from choir %q/%q: %v", sid, incarnation, choirName, err))
	}
}

func presentOutput(incarnation, choirName, sid string, added bool) map[string]any {
	return map[string]any{
		"incarnation": incarnation,
		"choir":       choirName,
		"sid":         sid,
		"state":       StatePresent,
		"added":       added,
	}
}

func absentOutput(incarnation, choirName, sid string, removed bool) map[string]any {
	return map[string]any{
		"incarnation": incarnation,
		"choir":       choirName,
		"sid":         sid,
		"state":       StateAbsent,
		"removed":     removed,
	}
}

func isKnownState(s string) bool {
	return s == StatePresent || s == StateAbsent
}
