// Package beacon — SDK Soul Stack для авторов SoulBeacon-плагинов
// (kind: soul_beacon, бинари soul-beacon-<name>, ADR-030 V5-2).
//
// Минимальный путь автора плагина:
//
//	type ZFSDegraded struct { beacon.BaseBeacon }
//
//	func (z *ZFSDegraded) Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
//	    // ...
//	}
//
//	func main() {
//	    if err := beacon.Serve(&ZFSDegraded{}); err != nil { os.Exit(1) }
//	}
//
// BaseBeacon даёт no-op-реализации Validate (Ok=true) и Check
// (State="unknown"); автор переопределяет только нужный метод. Serve открывает
// Unix-socket, делает gRPC-stdio handshake и обрабатывает SIGTERM
// (см. sdk/handshake).
//
// Beacon — read-only по конструкции (ADR-030): Check наблюдает состояние хоста и
// возвращает его, но НЕ мутирует систему. Любая запись в систему — баг плагина.
package beacon

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"google.golang.org/grpc"
)

// protocolVersion — версия plugin-протокола MVP (docs/keeper/plugins.md →
// Versioning). Симметрично sdk/module / sdk/clouddriver / sdk/sshprovider.
const protocolVersion = 1

// Beacon — интерфейс, который реализует плагин-автор. Сигнатуры повторяют
// pluginv1.SoulBeaconServer, но без must-embed-требования к
// pluginv1.UnimplementedSoulBeaconServer: SDK берёт forward-compat на себя
// через внутренний adapter.
type Beacon interface {
	Validate(ctx context.Context, req *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error)
	Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error)
}

// BaseBeacon — embeddable default-реализация Beacon: Validate отдаёт Ok=true,
// Check отдаёт State="unknown" без payload. Автор переопределяет только нужные
// RPC; нереализованные продолжают возвращать ожидаемые «безопасные» ответы.
// Это допустимо для тестовых плагинов и smoke-test-ов.
type BaseBeacon struct{}

func (BaseBeacon) Validate(context.Context, *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error) {
	return &pluginv1.ValidateVigilReply{Ok: true}, nil
}

func (BaseBeacon) Check(context.Context, *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
	return &pluginv1.CheckReply{State: "unknown"}, nil
}

// Serve — типовой main() SoulBeacon-плагина: оборачивает sdk/handshake.Serve
// + регистрирует grpc-service pluginv1.SoulBeacon с автор-impl.
func Serve(impl Beacon) error {
	return handshake.Serve(handshake.Config{
		ProtocolVersion: protocolVersion,
		Kind:            pluginv1.Kind_KIND_SOUL_BEACON,
	}, func(s *grpc.Server) {
		pluginv1.RegisterSoulBeaconServer(s, &serverAdapter{impl: impl})
	})
}

// serverAdapter — мост между SDK-интерфейсом Beacon и
// pluginv1.SoulBeaconServer; embed Unimplemented обеспечивает forward-compat
// при добавлении новых RPC в proto/plugin/v2/.
type serverAdapter struct {
	pluginv1.UnimplementedSoulBeaconServer
	impl Beacon
}

func (a *serverAdapter) Validate(ctx context.Context, req *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error) {
	return a.impl.Validate(ctx, req)
}

func (a *serverAdapter) Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
	return a.impl.Check(ctx, req)
}
