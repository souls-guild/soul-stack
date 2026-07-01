package main

import (
	"fmt"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// sendOutcome отправляет финальное событие с changed/message/output.
func sendOutcome(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, message string, output map[string]any) error {
	out, err := structpb.NewStruct(output)
	if err != nil {
		return fmt.Errorf("build output struct: %w", err)
	}
	return stream.Send(&pluginv1.ApplyEvent{Message: message, Changed: changed, Output: out})
}

// sendFailure отправляет финальное событие с failed=true. message уже
// санитизирован (redactError) — секретов не несёт.
func sendFailure(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], message string) error {
	return stream.Send(&pluginv1.ApplyEvent{Message: message, Failed: true})
}
