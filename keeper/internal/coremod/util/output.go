package util

import (
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// SendFinal — финальный ApplyEvent с changed/output. nil output → поле
// опускается. Helper нужен, чтобы каждый keeper-side core-модуль не повторял
// сборку *pluginv1.ApplyEvent и обращение к stream.Send.
//
// Соглашение по контракту pluginv1.ApplyEvent: финальное событие — это
// событие с changed или failed; промежуточные диагностические message-ы
// (без changed/failed) MVP keeper-side core пока не шлёт (симметрично
// Soul-side).
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
