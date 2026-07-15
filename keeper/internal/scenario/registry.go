package scenario

import (
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// ServiceCatalog — the source of Service registry entries by name. Implemented by
// a runtime snapshot [serviceregistry.Holder] (Resolve reads the current atomic
// snapshot from the DB, ADR-029). Declared as an interface so [ServiceRegistry]
// can be tested without Postgres (fake catalog).
//
// The getter is SYNCHRONOUS (no ctx/error) — this is the hot-path contract: a
// missing Service is a normal false, not a failure; the snapshot is read
// lock-free.
type ServiceCatalog interface {
	Resolve(name string) (serviceregistry.ServiceEntry, bool)
}

// ServiceRegistry resolves a service repo's git coordinates by service name. The
// registry moved into Postgres (`service_registry`, ADR-029: version = git ref);
// entries are read from a runtime [ServiceCatalog] snapshot (serviceregistry.Holder),
// not from a static keeper.yml. Safe for concurrent use — the snapshot is lock-free.
//
// Implements handlers.ServiceResolver (method Resolve). Registry hot-reload is
// transparent: Holder swaps the snapshot on a TTL poll / pub/sub invalidation,
// Resolve sees fresh entries without rebuilding ServiceRegistry.
type ServiceRegistry struct {
	catalog ServiceCatalog
}

// NewServiceRegistry wraps a catalog source (serviceregistry.Holder).
func NewServiceRegistry(catalog ServiceCatalog) *ServiceRegistry {
	return &ServiceRegistry{catalog: catalog}
}

// Resolve returns the ServiceRef for service and true if it's present in the
// registry's current snapshot; otherwise a zero-value and false. The
// ServiceEntry→ServiceRef mapping takes only the git coordinates (Name/Git/Ref);
// the snapshot's audit metadata isn't needed for artifact loading.
func (r *ServiceRegistry) Resolve(service string) (artifact.ServiceRef, bool) {
	e, ok := r.catalog.Resolve(service)
	if !ok {
		return artifact.ServiceRef{}, false
	}
	return artifact.ServiceRef{Name: e.Name, Git: e.Git, Ref: e.Ref}, true
}
