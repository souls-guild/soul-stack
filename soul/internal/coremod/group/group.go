// Package group реализует core-модуль `core.group` ([ADR-015]).
//
// Состояния:
//   - present: группа существует с заданным gid.
//   - absent:  группа удалена.
//
// Опциональные params present:
//   - gid (int):     явный gid (groupadd -g).
//   - system (bool): системная группа (groupadd -r), gid из системного
//     диапазона. Совместимо с gid (можно задать оба). Нужно для сервис-
//     аккаунтов stateful-сервисов (например primary-группа redis).
//
// Backend: groupadd/groupdel.
package group

import (
	"context"
	"fmt"
	"os/user"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.group"

type Module struct {
	Runner      util.Runner
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		Runner:      util.OSRunner{},
		LookupGroup: user.LookupGroup,
	}
}

// Validate — known-state + required-param (name) делегированы в
// shared/coremanifest/group.yaml (единый источник с soul-lint, убран дубль).
// Тип-проверка опциональных gid/system оставлена поверх делегации: это ранний
// type-guard значения (manifest-DSL его не выражает) — контракт «non-bool system
// / non-int gid отвергаются на Validate» (см. group_test.go).
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	if _, _, err := util.OptIntParam(req.Params, "gid"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptBoolParam(req.Params, "system"); err != nil {
		errs = append(errs, err.Error())
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe объявляет, что core.group.Plan — pure-read (ADR-031 Scry):
// читает LookupGroup и НЕ мутирует хост (маркер для host-а, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущее наличие группы (тот
// же LookupGroup, что в начале Apply) и шлёт PlanEvent.changed — «Apply
// изменил бы группу?». НЕ мутирует хост: ни groupadd, ни groupdel.
//
// Семантика 1:1 с Apply: present-or-create (gid/system на уже существующей
// группе НЕ триггерят reconcile в MVP — см. doc Apply), поэтому drift для
// present = «группы нет», для absent = «группа есть».
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	_, lookupErr := m.LookupGroup(name)
	exists := lookupErr == nil
	switch req.State {
	case "present":
		return util.SendPlanFinal(stream, !exists)
	case "absent":
		return util.SendPlanFinal(stream, exists)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	switch req.State {
	case "present":
		return m.applyPresent(ctx, stream, req, name)
	case "absent":
		return m.applyAbsent(ctx, stream, name)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyPresent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, name string) error {
	gid, hasGID, err := util.OptIntParam(req.Params, "gid")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	system, err := util.OptBoolParam(req.Params, "system")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if _, lookupErr := m.LookupGroup(name); lookupErr == nil {
		return util.SendFinal(stream, false, map[string]any{
			"name":    name,
			"exists":  true,
			"created": false,
		})
	}
	args := []string{}
	if system {
		args = append(args, "-r")
	}
	if hasGID {
		args = append(args, "-g", strconv.FormatInt(gid, 10))
	}
	args = append(args, name)
	if err := m.must(ctx, "groupadd", args...); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":    name,
		"exists":  true,
		"created": true,
	})
}

func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], name string) error {
	if _, lookupErr := m.LookupGroup(name); lookupErr != nil {
		return util.SendFinal(stream, false, map[string]any{
			"name":   name,
			"exists": false,
		})
	}
	if err := m.must(ctx, "groupdel", name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":   name,
		"exists": false,
	})
}

func (m *Module) must(ctx context.Context, name string, args ...string) error {
	r := m.Runner.Run(ctx, name, args...)
	if r.Err != nil {
		return fmt.Errorf("%s: %v", name, r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", name, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}
