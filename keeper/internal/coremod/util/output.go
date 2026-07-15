package util

import (
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// SendFinal sends the final ApplyEvent with changed/output; nil output omits
// the field. The helper exists so each keeper-side core module doesn't repeat
// building *pluginv1.ApplyEvent and calling stream.Send.
//
// Contract convention for pluginv1.ApplyEvent: the final event is the one
// with changed or failed set; intermediate diagnostic messages (without
// changed/failed) MVP keeper-side core doesn't yet send (mirrors
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

// SendFailed sends the final event with failed=true and the error text in
// message. Output isn't sent — failure semantics make the output field moot.
func SendFailed(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], message string) error {
	return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: message})
}
