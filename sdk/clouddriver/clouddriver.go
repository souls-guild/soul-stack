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
// возвращает пустой State, Resize — default-deny resize.unsupported); автор
// переопределяет только те, которые нужны. Resize дополнительно требует
// объявить marker-интерфейс Resizable (default-deny capability).
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

	// Resize расширяет ресурсы VM (cpu/ram/disk, наши единицы). Объявлен в
	// интерфейсе для forward-compat (gRPC-контракт его уже несёт), но capability
	// объявляется ОТДЕЛЬНО через marker-интерфейс [Resizable] (default-deny):
	// драйвер, реально поддерживающий resize, переопределяет этот метод И
	// реализует [Resizable]. Драйвер на [BaseDriver] получает безопасный
	// default-deny — host (keeper) не зовёт Resize, а возвращает
	// resize.unsupported. См. [BaseDriver.Resize], [Resizable].
	Resize(req *pluginv1.ResizeRequest, stream grpc.ServerStreamingServer[pluginv1.ResizeEvent]) error
}

// Resizable — опциональный marker-интерфейс (default-deny, паттерн [PlanReadSafe]
// из sdk/module / ADR-031): драйвер реализует его, чтобы ОБЪЯВИТЬ, что его Resize —
// настоящая реализация (умеет менять ресурсы VM). Host (keeper, модуль
// `core.cloud.provisioned` state=resized) проверяет реализацию type-assertion-ом
// ДО вызова Resize: драйвер без [Resizable] получает default-deny — host
// возвращает внятный `resize.unsupported`, а НЕ сырой gRPC Unimplemented и не
// ложный «успех».
//
// Метод-маркер без аргументов: его наличие = декларация capability. [BaseDriver]
// его НЕ реализует СОЗНАТЕЛЬНО — драйвер на BaseDriver по умолчанию получает
// безопасный default-deny на resize без действий автора.
type Resizable interface {
	// Resizable — маркер; вызывается host-ом как type-assertion, тело не важно.
	Resizable()
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

// Resize — default-deny no-op: шлёт финальный ResizeEvent с failed=true и
// message resize.unsupported, НЕ панику и НЕ ложный «успех». В норме host до
// этого метода не доходит (BaseDriver не реализует [Resizable] → host применяет
// default-deny). Этот fallback защищает от прямого вызова в обход marker-check-а.
func (BaseDriver) Resize(_ *pluginv1.ResizeRequest, stream grpc.ServerStreamingServer[pluginv1.ResizeEvent]) error {
	return stream.Send(&pluginv1.ResizeEvent{
		Failed:  true,
		Message: "resize.unsupported: driver does not implement Resize (missing Resizable capability)",
	})
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

// Resize применяет default-deny по marker-интерфейсу [Resizable] (паттерн
// PlanReadSafe). Плагин живёт в отдельном процессе, поэтому host (keeper)
// не может проверить marker напрямую type-assertion-ом — проверка делается
// здесь, в serverAdapter: если impl НЕ реализует [Resizable], adapter
// возвращает resize.unsupported, НЕ вызывая impl.Resize. Это гарантирует, что
// драйвер на [BaseDriver] (или забывший объявить capability) получит внятный
// отказ, а не случайно выполнит no-op Resize.
func (a *serverAdapter) Resize(req *pluginv1.ResizeRequest, stream grpc.ServerStreamingServer[pluginv1.ResizeEvent]) error {
	if _, ok := a.impl.(Resizable); !ok {
		return stream.Send(&pluginv1.ResizeEvent{
			Failed:  true,
			Message: "resize.unsupported: driver does not declare Resizable capability",
		})
	}
	return a.impl.Resize(req, stream)
}
