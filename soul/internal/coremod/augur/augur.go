// Package augur реализует core-модуль `core.augur.fetch` (ADR-025,
// docs/keeper/augur.md) — Soul-side read-probe живого доступа к внешней системе
// через брокер Augur.
//
// Verb MVP:
//   - fetch: запросить у Keeper-а значение из Omen-а (vault KV / prometheus /
//     elk) в момент apply. Модуль шлёт AugurRequest в EventStream и ждёт
//     коррелированный AugurReply; при OK кладёт inline_data в register-output.
//
// Семантика changed:
//   - changed = false ВСЕГДА, конструктивно и ненастраиваемо: read-probe не
//     меняет состояние хоста (прецедент — core.http.probe / core.exec.run).
//
// Граница ADR-012(d): данные приходят inline ЧЕРЕЗ Keeper (delegate=false,
// MVP-1). На Soul внешний токен/credential не попадает — Augur-клиент знает
// только request_id-корреляцию, не master-cred. Делегация (delegate=true,
// scoped_*) — MVP-2; здесь не обрабатывается (Augur-клиент вернёт ошибку на OK
// без inline_data).
//
// Авторизация — Keeper-side (§6 augur.md): Omen-существование, SID→covens, Rite,
// allow-list. DENIED/ERROR/UNSPECIFIED → ошибка шага без секретного материала.
package augur

import (
	"context"
	"errors"
	"fmt"

	soulaugur "github.com/souls-guild/soul-stack/soul/internal/augur"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — каноническая верхушка адреса модуля.
const Name = "core.augur"

// verbFetch — единственный поддерживаемый verb.
const verbFetch = "fetch"

// Module — реализация sdk/module.SoulModule для core.augur.
//
// Augur-клиент модуль НЕ держит полем: он приходит per-прогон через
// stream.Context() (soul/internal/augur.FromContext) — клиент привязан к
// конкретной EventStream-сессии, а модуль stateless и переиспользуется между
// прогонами.
type Module struct{}

func New() *Module { return &Module{} }

// Validate проверяет verb и обязательные params (omen / query). known-state и
// required частично дублируются с manifest-DSL осознанно — здесь только базовые
// семантики формы; авторизацию (allow-list) проверяет Keeper, не Soul.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.GetState() != verbFetch {
		errs = append(errs, fmt.Sprintf("unknown verb %q (want %s)", req.GetState(), verbFetch))
	}
	if _, err := util.StringParam(req.GetParams(), "omen"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringParam(req.GetParams(), "query"); err != nil {
		errs = append(errs, err.Error())
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.GetState() != verbFetch {
		return util.SendFailed(stream, fmt.Sprintf("unknown verb %q", req.GetState()))
	}
	return m.applyFetch(stream, req)
}

// applyFetch реализует verb `fetch`: один AugurRequest → AugurReply через
// EventStream. Контракт ошибок:
//   - Augur недоступен в прогоне (push-режим / нет сессии) → failed;
//   - DENIED/ERROR/UNSPECIFIED от Keeper-а → failed (причина без секрета);
//   - таймаут/разрыв стрима (ctx / клиент закрыт) → failed;
//   - OK → changed=false + inline_data как register-output.
func (m *Module) applyFetch(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest) error {
	omen, err := util.StringParam(req.GetParams(), "omen")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	query, err := util.StringParam(req.GetParams(), "query")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	ctx := stream.Context()
	fetcher, applyID, ok := soulaugur.FromContext(ctx)
	if !ok {
		// Push-режим (soul apply) или сессия без Augur-плумбинга: брокер
		// недоступен. Не молчим — read-probe без брокера бессмыслен.
		return util.SendFailed(stream, "core.augur.fetch: брокер Augur недоступен в этом прогоне (нет EventStream-сессии)")
	}

	reply, ferr := fetcher.Fetch(ctx, applyID, omen, query)
	if ferr != nil {
		return util.SendFailed(stream, fetchErrorMessage(omen, ferr))
	}

	// OK: inline_data — google.protobuf.Struct (§5.3 augur.md). Кладём его как
	// register-output as-is (скаляр уже завёрнут Keeper-ом в {value:..}; map —
	// натуральный объект). changed=false конструктивно — read-probe.
	out := reply.GetInlineData().AsMap()
	return util.SendFinal(stream, false, out)
}

// fetchErrorMessage формирует понятное сообщение об ошибке шага без секретного
// материала. Причина (от Keeper-а / транспорта) уже без значения/токена (§8
// augur.md), но мы добавляем имя Omen-а для диагностики — query/значение НЕ
// логируем (query может нести путь к секрету).
func fetchErrorMessage(omen string, err error) string {
	switch {
	case errors.Is(err, soulaugur.ErrDenied):
		return fmt.Sprintf("core.augur.fetch: доступ к Omen %q запрещён: %v", omen, err)
	case errors.Is(err, soulaugur.ErrRemote):
		return fmt.Sprintf("core.augur.fetch: Omen %q вернул ошибку: %v", omen, err)
	case errors.Is(err, soulaugur.ErrClientClosed):
		return fmt.Sprintf("core.augur.fetch: EventStream-сессия закрыта до ответа по Omen %q", omen)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("core.augur.fetch: запрос к Omen %q прерван (%v)", omen, err)
	default:
		return fmt.Sprintf("core.augur.fetch: запрос к Omen %q не выполнен: %v", omen, err)
	}
}
