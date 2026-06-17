// Package clouddriver — SDK Soul Stack для авторов CloudDriver-плагинов
// (kind: cloud_driver, бинари soul-cloud-<provider>).
//
// Минимальный путь автора плагина:
//
//	type AwsDriver struct { clouddriver.BaseDriver }
//
//	func (a *AwsDriver) Create(req *pluginv1.CreateRequest, stream pluginv1.CloudDriver_CreateServer) error {
//	    // ...
//	}
//
//	func main() {
//	    if err := clouddriver.Serve(&AwsDriver{}); err != nil { os.Exit(1) }
//	}
//
// BaseDriver даёт no-op-реализации всех RPC (Schema/Validate возвращают пустые
// успешные ответы, Create/Destroy/List закрывают stream без событий, Status
// возвращает пустой State); автор переопределяет только те, которые нужны.
// Serve открывает Unix-socket, делает gRPC-stdio handshake и обрабатывает
// SIGTERM (см. sdk/handshake).
package clouddriver

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	"google.golang.org/grpc"
)

// protocolVersion — версия plugin-протокола MVP (docs/keeper/plugins.md →
// Versioning). Симметрично sdk/module.
const protocolVersion = 1

// CloudDriver — интерфейс, который реализует плагин-автор. Сигнатуры повторяют
// pluginv1.CloudDriverServer, но без must-embed-требования к
// pluginv1.UnimplementedCloudDriverServer: SDK берёт forward-compat на себя
// через внутренний adapter.
type CloudDriver interface {
	Schema(ctx context.Context, req *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error)
	Validate(ctx context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error)
	Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error
	Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error
	Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error)
	List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error
}

// BaseDriver — embeddable default-реализация CloudDriver: все методы no-op.
// Автор переопределяет только нужные RPC; нереализованные продолжают возвращать
// пустые ответы (Validate отдаёт Ok=true, Create/Destroy/List закрывают stream
// без событий). Это допустимо для тестовых плагинов и smoke-test-ов.
type BaseDriver struct{}

func (BaseDriver) Schema(context.Context, *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	return &pluginv1.SchemaReply{}, nil
}

func (BaseDriver) Validate(context.Context, *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	return &pluginv1.ValidateProfileReply{Ok: true}, nil
}

func (BaseDriver) Create(*pluginv1.CreateRequest, grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	return nil
}

func (BaseDriver) Destroy(*pluginv1.DestroyRequest, grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	return nil
}

func (BaseDriver) Status(context.Context, *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	return &pluginv1.StatusReply{}, nil
}

func (BaseDriver) List(*pluginv1.ListRequest, grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	return nil
}

// Serve — типовой main() CloudDriver-плагина: оборачивает sdk/handshake.Serve
// + регистрирует grpc-service pluginv1.CloudDriver с автор-impl.
func Serve(impl CloudDriver) error {
	return handshake.Serve(handshake.Config{
		ProtocolVersion: protocolVersion,
		Kind:            pluginv1.Kind_KIND_CLOUD_DRIVER,
	}, func(s *grpc.Server) {
		pluginv1.RegisterCloudDriverServer(s, &serverAdapter{impl: impl})
	})
}

// serverAdapter — мост между SDK-интерфейсом CloudDriver и
// pluginv1.CloudDriverServer; embed Unimplemented обеспечивает forward-compat
// при добавлении новых RPC в proto/plugin/v2/.
type serverAdapter struct {
	pluginv1.UnimplementedCloudDriverServer
	impl CloudDriver
}

func (a *serverAdapter) Schema(ctx context.Context, req *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	return a.impl.Schema(ctx, req)
}

func (a *serverAdapter) Validate(ctx context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	return a.impl.Validate(ctx, req)
}

func (a *serverAdapter) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	return a.impl.Create(req, stream)
}

func (a *serverAdapter) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	return a.impl.Destroy(req, stream)
}

func (a *serverAdapter) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	return a.impl.Status(ctx, req)
}

func (a *serverAdapter) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	return a.impl.List(req, stream)
}
