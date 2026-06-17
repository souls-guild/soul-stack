// Package internaltest — общие test-helper-ы для unit-тестов keeper-side
// core-модулей (`core.soul.registered`, `core.cloud.provisioned`,
// `core.vault.kv-read`). Сам пакет без суффикса _test, потому что test-файлы
// разных модульных пакетов не могут импортировать xxx_test друг друга.
//
// Содержимое — только тестовая инфраструктура, в production-сборку попадает
// как dead-code (нет init-ов, нет registry-сторон). Симметрично
// soul/internal/coremod/internaltest/stream.go.
package internaltest

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// ApplyStream — fake grpc.ServerStreamingServer[ApplyEvent].
// Захватывает все Send-события в Events; финальное событие — Last().
type ApplyStream struct {
	grpc.ServerStreamingServer[pluginv1.ApplyEvent]
	Ctx    context.Context
	Events []*pluginv1.ApplyEvent
}

// NewApplyStream — с background-контекстом.
func NewApplyStream() *ApplyStream {
	return &ApplyStream{Ctx: context.Background()}
}

// NewApplyStreamCtx — с переданным контекстом (для cancel-тестов).
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

// Last — последнее отправленное событие; nil если ничего не было.
func (s *ApplyStream) Last() *pluginv1.ApplyEvent {
	if len(s.Events) == 0 {
		return nil
	}
	return s.Events[len(s.Events)-1]
}
