package util

import (
	"errors"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// SendFinal — отправляет финальный ApplyEvent с changed/failed/output на
// поток. Output может быть nil (тогда поле опускается). Helper нужен, чтобы
// каждый core-модуль не повторял boilerplate сборки *pluginv1.ApplyEvent
// и обращение к stream.Send.
//
// Соглашение по контракту pluginv1.ApplyEvent: финальное событие — это
// событие с changed или failed; промежуточные диагностические message-ы
// (без changed/failed) MVP-core пока не шлёт.
func SendFinal(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, output map[string]any) error {
	ev := &pluginv1.ApplyEvent{Changed: changed}
	if output != nil {
		s, err := structpb.NewStruct(output)
		if err != nil {
			return err
		}
		ev.Output = s
	}
	return stream.Send(ev)
}

// SendFailed — финальное событие с failed=true и текстом ошибки в message.
// Output не передаётся — failure-семантика делает поля output бессмысленными.
func SendFailed(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], message string) error {
	return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: message})
}

// SendPlanFinal — финальный PlanEvent dry-run (ADR-031 Scry) с машинным
// `changed` (drift). Параллель SendFinal для Apply: core-модули не повторяют
// boilerplate сборки *pluginv1.PlanEvent. Output Plan в MVP не передаётся —
// dry-run сообщает только факт расхождения.
func SendPlanFinal(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], changed bool) error {
	return stream.Send(&pluginv1.PlanEvent{Changed: changed})
}

// PlanFailed — ошибка dry-run-а: модуль не смог определить drift (невалидный
// param, неподдержанный backend/state). Возвращается из Plan как Go-error —
// host (runtime.planTask) маппит ненулевой error в FAILED (plan.error), а НЕ в
// clean (ADR-031: не false-clean). У PlanEvent нет failed-поля (only-add
// changed, симметрия с ApplyEvent не дотягивает), поэтому proval плана едет
// именно error-ом возврата Plan, а не событием — host обязан смотреть на error
// до changed. Параллель SendFailed для Apply (тот шлёт failed-событие, т.к.
// ApplyEvent.failed есть; здесь его нет — отсюда разница формы).
func PlanFailed(message string) error {
	return errors.New(message)
}

// StringsToAny конвертирует []string в []any для list-значения output:
// structpb.NewStruct принимает только []any (не []string) в качестве списка.
// Единая точка для core-модулей, кладущих строковый список в output (warnings и
// т.п.), чтобы не повторять boilerplate-цикл у каждого вызова.
func StringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
