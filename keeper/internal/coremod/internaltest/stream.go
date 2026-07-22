// Package internaltest holds shared test helpers for keeper-side unit tests of
// core modules (`core.soul.registered`, `core.cloud.provisioned`,
// `core.vault.kv-read`). The package itself has no _test suffix because test files
// of different module packages can't import each other's xxx_test.
//
// Content is test infrastructure only; it ends up as dead code in the
// production build (no init funcs, no registry side effects). Mirrors
// soul/internal/coremod/internaltest/stream.go.
package internaltest

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// ApplyStream — fake grpc.ServerStreamingServer[ApplyEvent].
// Captures every Send event in Events; the final event is Last().
type ApplyStream struct {
	grpc.ServerStreamingServer[pluginv1.ApplyEvent]
	Ctx    context.Context
	Events []*pluginv1.ApplyEvent
}

// NewApplyStream uses a background context.
func NewApplyStream() *ApplyStream {
	return &ApplyStream{Ctx: context.Background()}
}

// NewApplyStreamCtx uses the given context (for cancellation tests).
func NewApplyStreamCtx(ctx context.Context) *ApplyStream {
	return &ApplyStream{Ctx: ctx}
}

func (s *ApplyStream) Send(e *pluginv1.ApplyEvent) error {
	s.Events = append(s.Events, e)
	return nil
}

func (s *ApplyStream) Context() context.Context {
	if s.Ctx == nil {
		return context.Background()
	}
	return s.Ctx
}

// Last is the last sent event; nil if nothing was sent.
func (s *ApplyStream) Last() *pluginv1.ApplyEvent {
	if len(s.Events) == 0 {
		return nil
	}
	return s.Events[len(s.Events)-1]
}
