// Package noop реализует core-модуль `core.noop` ([ADR-015]) — no-op-шаг,
// который ничего не делает и всегда возвращает успех без изменения состояния
// (changed=false).
//
// Verb MVP:
//   - run: no-op. Не читает и не меняет состояние хоста. Любые `params:`
//     принимаются и игнорируются (схема пустая) — шаг существует как
//     синтаксический якорь, а не как операция над ресурсом.
//
// Назначение:
//   - barrier-якорь: задача `core.noop.run`, обращающаяся к `register.*`
//     нескольких предыдущих задач, даёт точку, в которой фреймворк дожидается
//     их завершения (implicit barrier через register-зависимости). Сам барьер
//     даёт `require:`/register-граф, а не модуль; noop — пустое тело такой
//     задачи.
//   - placeholder: пустой шаг, удобный как заглушка в каркасе destiny/scenario
//     до появления реальной логики, либо как носитель `output:`-проекции
//     (`output:` читает `register.*` предыдущих задач, своей работы не делает).
//
// Семантика changed:
//   - changed = false ВСЕГДА, конструктивно и ненастраиваемо: no-op не меняет
//     состояние хоста. Прецедент — read-probe-модули (`core.http`, `core.exec`):
//     модуль не объявляет drift, а интерпретацию задаёт scenario.
//
// Идемпотентность: no-op идемпотентен по природе (пустая операция).
//
// [ADR-015]: docs/adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список
package noop

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — каноническая верхушка адреса.
const Name = "core.noop"

// Module — реализация sdk/module.SoulModule для core.noop. Состояния нет:
// модуль не держит зависимостей, единственный verb `run` — пустая операция.
type Module struct{}

func New() *Module { return &Module{} }

// Validate принимает только verb `run`. `params:` не проверяются — схема пустая
// (любые ключи игнорируются Apply), известность verb-а достаточна.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op (без PlanReadSafe). core.noop не имеет желаемого состояния хоста,
// сверяемого pure-read-ом: drift в смысле ADR-031 не определён (changed всегда
// false конструктивно). Host применяет default-deny — dry_run для core.noop
// возвращает FAILED `plan.unsupported`, и это конструктивный отказ, а не ложное
// «нет дрифта». Сам шаг no-op по природе, но вне контракта Plan/Apply ADR-031.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// ErrandReadSafe — marker [sdkmodule.ErrandReadSafe] (ADR-033 §2): no-op не
// мутирует состояние хоста и не имеет побочных эффектов, поэтому безопасен к
// ad-hoc invocation через Errand pull-контур. Явный опт-ин в whitelist
// Errand-runner-а, симметрично read-probe-модулям (core.http).
func (m *Module) ErrandReadSafe() {}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != "run" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	return util.SendFinal(stream, false, nil)
}
