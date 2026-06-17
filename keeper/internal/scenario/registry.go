package scenario

import (
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// ServiceCatalog — источник записей реестра Service-ов по имени. Реализуется
// runtime-снимком [serviceregistry.Holder] (Resolve читает текущий atomic-снимок
// из БД, ADR-029). Объявлен интерфейсом, чтобы [ServiceRegistry] тестировался без
// Postgres-а (fake-каталог).
//
// Геттер СИНХРОННЫЙ (без ctx/error) — это контракт горячего пути: отсутствие
// Service-а = нормальный false, не сбой; снимок читается lock-free.
type ServiceCatalog interface {
	Resolve(name string) (serviceregistry.ServiceEntry, bool)
}

// ServiceRegistry резолвит git-координаты service-репо по имени сервиса. Реестр
// перенесён в Postgres (`service_registry`, ADR-029: version = git ref); записи
// читаются из runtime-снимка [ServiceCatalog] (serviceregistry.Holder), а не из
// статического keeper.yml. Safe for concurrent use — снимок lock-free.
//
// Реализует handlers.ServiceResolver (метод Resolve). Hot-reload реестра
// прозрачен: Holder свопает снимок по TTL-poll-у / pub/sub-инвалидации, Resolve
// видит свежие записи без пересборки ServiceRegistry.
type ServiceRegistry struct {
	catalog ServiceCatalog
}

// NewServiceRegistry оборачивает источник-каталог (serviceregistry.Holder).
func NewServiceRegistry(catalog ServiceCatalog) *ServiceRegistry {
	return &ServiceRegistry{catalog: catalog}
}

// Resolve возвращает ServiceRef сервиса service и true, если он есть в текущем
// снимке реестра; иначе zero-value и false. Маппинг ServiceEntry→ServiceRef
// берёт только git-координаты (Name/Git/Ref); audit-метаданные снимка для
// загрузки артефакта не нужны.
func (r *ServiceRegistry) Resolve(service string) (artifact.ServiceRef, bool) {
	e, ok := r.catalog.Resolve(service)
	if !ok {
		return artifact.ServiceRef{}, false
	}
	return artifact.ServiceRef{Name: e.Name, Git: e.Git, Ref: e.Ref}, true
}
