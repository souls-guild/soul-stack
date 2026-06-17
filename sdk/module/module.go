// Package module — SDK Soul Stack для авторов SoulModule-плагинов
// (kind: soul_module, бинари soul-mod-<name>).
//
// Минимальный путь автора плагина:
//
//	type RedisFailover struct { module.BaseModule }
//
//	func (r *RedisFailover) Apply(req *pluginv1.ApplyRequest, stream pluginv1.SoulModule_ApplyServer) error {
//	    // ...
//	}
//
//	func main() {
//	    if err := module.Serve(&RedisFailover{}); err != nil { os.Exit(1) }
//	}
//
// BaseModule даёт no-op-реализации Validate (ok=true) и Plan (empty stream);
// автор переопределяет только Apply. Serve открывает Unix-socket, делает
// gRPC-stdio handshake и обрабатывает SIGTERM (см. sdk/handshake).
package module

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"google.golang.org/grpc"
)

// protocolVersion — версия plugin-протокола MVP (docs/keeper/plugins.md →
// Versioning, единственная поддерживаемая host-ами версия в SupportedProtocolVersions).
const protocolVersion = 1

// SoulModule — интерфейс, который реализует плагин-автор. Сигнатуры повторяют
// pluginv1.SoulModuleServer, но без must-embed-требования к
// pluginv1.UnimplementedSoulModuleServer: SDK берёт forward-compat на себя
// через внутренний adapter.
//
// Контракт Plan (ADR-031 Scry) — pure-read dry-run: модуль ЧИТАЕТ текущее
// состояние ресурса (тот же read, что в начале Apply) и шлёт финальный
// PlanEvent с машинным `changed` — «Apply изменил бы ресурс?» (drift). Plan
// НЕ МУТИРУЕТ хост: никаких install/write/start. Host (Soul) на dry_run зовёт
// Plan ВМЕСТО Apply. Модуль без настоящей pure-read-реализации Plan должен
// объявить это, НЕ реализуя [PlanReadSafe] — тогда host применит default-deny
// (Plan не вызывается, задача получает явный «drift не поддержан», не ложное
// «нет дрифта»). См. [PlanReadSafe].
//
// **Инвариант (read-safe Plan):** реализация Plan, объявившая [PlanReadSafe],
// ОБЯЗАНА отправить РОВНО ОДИН финальный PlanEvent с машинным `changed` ДО
// возврата из метода (без ошибки). Возврат `nil` без финального события host
// трактует как FAILED `plan.no_result` (защита от misbehaving-модуля,
// который «молча → clean»). Возврат ошибки → FAILED `plan.error`.
type SoulModule interface {
	Validate(ctx context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error)
	Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error
	Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error
}

// PlanReadSafe — опциональный marker-интерфейс (ADR-031 Scry, default-deny):
// модуль реализует его, чтобы ОБЪЯВИТЬ, что его Plan — настоящая pure-read
// реализация, безопасная к вызову на dry_run (читает состояние, НЕ мутирует
// хост). Host (Soul) на dry_run зовёт Plan ТОЛЬКО для модулей, реализующих этот
// интерфейс; всё остальное (custom-плагин на BaseModule, core-модуль без
// pure-read Plan) — default-deny: Plan не вызывается, задача получает явный
// отказ «drift не поддержан», а НЕ ложное clean.
//
// Метод-маркер без аргументов: его наличие = декларация capability. Реализуют
// его ТОЛЬКО модули с проверенным pure-read Plan; BaseModule его НЕ реализует
// (no-op Plan не определяет drift), поэтому плагин на BaseModule по умолчанию
// получает безопасный default-deny без действий автора.
type PlanReadSafe interface {
	// PlanReadSafe — маркер; вызывается host-ом как type-assertion, тело не важно.
	PlanReadSafe()
}

// ErrandReadSafe — опциональный marker-интерфейс (ADR-033 Errand, default-deny):
// модуль реализует его, чтобы ОБЪЯВИТЬ, что его Apply безопасен к вызову через
// Errand pull-ad-hoc контур (не мутирует incarnation.state, не имеет побочных
// эффектов вне декларированных в `side_effects` манифеста). Soul-side
// Errand-runner проверяет реализацию ДО вызова Apply и ОТВЕРГАЕТ
// не-помеченные модули с `ErrandResult.status = MODULE_NOT_ALLOWED`
// (defense-in-depth, симметрично [PlanReadSafe] из ADR-031).
//
// Жёсткий список `core.cmd.shell` / `core.exec.run` обходит этот интерфейс —
// verb-модули императивные by-design и Errand-runner допускает их по имени
// без marker-check-а (ADR-033 §2).
//
// BaseModule НЕ реализует [ErrandReadSafe] — плагин на BaseModule по умолчанию
// получает безопасный default-deny на Errand-вызов. Автор плагина, чей Apply
// действительно безопасен к ad-hoc invocation, переопределяет Apply И
// реализует ErrandReadSafe явно.
type ErrandReadSafe interface {
	// ErrandReadSafe — маркер; вызывается host-ом как type-assertion, тело не важно.
	ErrandReadSafe()
}

// BaseModule — embeddable default-реализация SoulModule:
// Validate возвращает Ok=true, Plan не шлёт событий, Apply — TODO для автора.
//
// Apply здесь намеренно тоже no-op: embed-ер обязан переопределить его,
// иначе плагин не делает ничего полезного. Это допустимо для тестовых
// плагинов и smoke-test-ов.
//
// BaseModule НЕ реализует [PlanReadSafe] СОЗНАТЕЛЬНО: его Plan — no-op (drift
// не определяет), и плагин на BaseModule по умолчанию должен получать безопасный
// default-deny на dry_run (host не зовёт Plan → явный «drift не поддержан»),
// а не молча выдавать «нет дрифта». Автор плагина с настоящим pure-read Plan
// переопределяет Plan И реализует PlanReadSafe явно (ADR-031 Scry).
type BaseModule struct{}

func (BaseModule) Validate(context.Context, *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// Plan — no-op default: событий не шлёт, drift не определяет. Host применяет
// default-deny к модулю без [PlanReadSafe] — этот Plan не вызывается на dry_run.
func (BaseModule) Plan(*pluginv1.PlanRequest, grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (BaseModule) Apply(*pluginv1.ApplyRequest, grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	return nil
}

// Serve — типовой main() SoulModule-плагина: оборачивает sdk/handshake.Serve
// + регистрирует grpc-service pluginv1.SoulModule с автор-impl.
func Serve(impl SoulModule) error {
	return handshake.Serve(handshake.Config{
		ProtocolVersion: protocolVersion,
		Kind:            pluginv1.Kind_KIND_SOUL_MODULE,
	}, func(s *grpc.Server) {
		pluginv1.RegisterSoulModuleServer(s, &serverAdapter{impl: impl})
	})
}

// serverAdapter — мост между SDK-интерфейсом SoulModule и
// pluginv1.SoulModuleServer; embed Unimplemented обеспечивает forward-compat
// при добавлении новых RPC в proto/plugin/v2/.
type serverAdapter struct {
	pluginv1.UnimplementedSoulModuleServer
	impl SoulModule
}

func (a *serverAdapter) Validate(ctx context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return a.impl.Validate(ctx, req)
}

func (a *serverAdapter) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return a.impl.Plan(req, stream)
}

func (a *serverAdapter) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	return a.impl.Apply(req, stream)
}
