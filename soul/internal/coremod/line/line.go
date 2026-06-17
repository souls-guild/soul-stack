// Package line реализует core-модуль `core.line` ([ADR-015]) — in-place
// построчную правку существующего файла. Это первый core-модуль, который НЕ
// перезаписывает файл целиком (как core.file), а изменяет отдельные строки.
//
// Состояния MVP (урезанный безопасный вариант — ровно та причина, по которой
// lineinfile откладывали: «regex matches not what you think»):
//
//   - present: строка `line` присутствует в файле. С `regexp` — первая
//     матчащая строка заменяется на `line` (при >1 совпадении меняется только
//     первая + warning); без `regexp` — точная строка добавляется, если её нет.
//   - absent:  с `regexp` удаляются ВСЕ матчащие строки; без `regexp` —
//     удаляются все точные совпадения `line`.
//
// Сознательные ограничения MVP (расширяемо позже без breaking change):
//   - backrefs (подстановка групп regexp в `line`) НЕ поддержаны.
//   - insertafter / insertbefore — только литеральная строка либо
//     EOF / BOF соответственно, НЕ regexp (предсказуемость позиции вставки).
//
// Запись — атомарная (util.AtomicWrite: temp+rename), а не in-place truncate.
// Идемпотентность: повторный прогон → changed=false.
//
// [ADR-015]: docs/adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список
package line

import (
	"context"
	"fmt"
	"os/user"
	"regexp"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — каноническая верхушка адреса.
const Name = "core.line"

// Module — реализация sdk/module.SoulModule для core.line.
//
// LookupUser / LookupGroup вынесены в поля для тестабельности (подмена без
// обращения к /etc/passwd) — симметрично core.file; используются только при
// create=true с owner/group.
type Module struct {
	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		LookupUser:  user.Lookup,
		LookupGroup: user.LookupGroup,
	}
}

// Validate НЕ делегирован целиком в util.ValidateAgainstManifest (в отличие от
// core.exec): сверх known-state + required(path) у core.line есть cross-field-
// инварианты, которые manifest-DSL не выражает — `line` обязателен для present
// (но не для absent), absent требует line ИЛИ regexp, insertafter/insertbefore
// взаимоисключаемы, regexp обязан компилироваться. known-state/required(path)
// дублируются с line.yaml осознанно — единый источник невозможен без cross-field
// в DSL.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case "present", "absent":
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want present|absent)", req.State))
	}
	if _, err := util.StringParam(req.Params, "path"); err != nil {
		errs = append(errs, err.Error())
	}

	rx, rerr := util.OptStringParam(req.Params, "regexp")
	if rerr != nil {
		errs = append(errs, rerr.Error())
	} else if rx != "" {
		if _, cerr := regexp.Compile(rx); cerr != nil {
			errs = append(errs, fmt.Sprintf("param %q: invalid regexp: %v", "regexp", cerr))
		}
	}

	// present обязан иметь line — это точное значение, которым управляем
	// (добавляем при отсутствии regexp / на него заменяем при regexp). absent
	// без regexp использует line как критерий точного совпадения для удаления,
	// поэтому хотя бы одно из line/regexp обязано присутствовать.
	line, lerr := util.OptStringParam(req.Params, "line")
	if lerr != nil {
		errs = append(errs, lerr.Error())
	}
	switch req.State {
	case "present":
		if line == "" {
			errs = append(errs, `param "line": required for state present`)
		}
	case "absent":
		if line == "" && rx == "" {
			errs = append(errs, `state absent requires "line" or "regexp"`)
		}
	}

	// insertafter / insertbefore взаимоисключающие; конкретные допустимые
	// значения (EOF/BOF/литерал) разруливаются в Apply при вставке.
	insAfter, iaerr := util.OptStringParam(req.Params, "insertafter")
	if iaerr != nil {
		errs = append(errs, iaerr.Error())
	}
	insBefore, iberr := util.OptStringParam(req.Params, "insertbefore")
	if iberr != nil {
		errs = append(errs, iberr.Error())
	}
	if insAfter != "" && insBefore != "" {
		errs = append(errs, `params "insertafter" and "insertbefore" are mutually exclusive`)
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe объявляет, что core.line.Plan — pure-read (ADR-031 Scry):
// читает файл и прогоняет ту же чистую функцию presentEdit/absentEdit, что
// applyPresent/applyAbsent (changed уже там). НЕ мутирует ФС (маркер для host-а,
// default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущее содержимое файла
// (тот же readFile, что в Apply) и переиспользует чистые presentEdit/absentEdit
// — те уже возвращают `changed`. НЕ мутирует ФС: ни AtomicWrite, ни AtomicWritePreserving.
//
// create=true (state present, файла нет) → drift=true (Apply создал бы файл).
// create=false (state present, файла нет) → drift=true (Apply вернул бы failed,
// но это всё ещё «отличие от желаемого», и Plan честно его репортит — оператор
// видит, что dry_run упрётся в create:false).
//
// absent + файла нет → drift=false (нечего удалять, симметрично applyAbsent).
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	p, err := readParamsFromStruct(req.Params)
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	switch req.State {
	case "present":
		if p.line == "" {
			return util.PlanFailed(`param "line": required for state present`)
		}
	case "absent":
		if p.line == "" && p.regexp == nil {
			return util.PlanFailed(`state absent requires "line" or "regexp"`)
		}
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}

	content, existed, rerr := readFile(path)
	if rerr != nil {
		return util.PlanFailed(rerr.Error())
	}
	switch req.State {
	case "present":
		if !existed {
			// Apply создал бы файл (если create=true) или вернул бы failed; в обоих
			// случаях желаемое состояние НЕ достигнуто → drift.
			return util.SendPlanFinal(stream, true)
		}
		lines, _ := splitLines(content)
		return util.SendPlanFinal(stream, presentEdit(lines, p).changed)
	case "absent":
		if !existed {
			return util.SendPlanFinal(stream, false)
		}
		lines, _ := splitLines(content)
		return util.SendPlanFinal(stream, absentEdit(lines, p).changed)
	}
	// unreachable.
	return util.PlanFailed("unreachable")
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	switch req.State {
	case "present":
		return m.applyPresent(stream, req, path)
	case "absent":
		return m.applyAbsent(stream, req, path)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}
