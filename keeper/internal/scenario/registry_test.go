package scenario

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// fakeCatalog — in-memory [ServiceCatalog] для тестов (без Postgres-снимка).
type fakeCatalog map[string]serviceregistry.ServiceEntry

func (c fakeCatalog) Resolve(name string) (serviceregistry.ServiceEntry, bool) {
	e, ok := c[name]
	return e, ok
}

func TestServiceRegistry_Resolve(t *testing.T) {
	reg := NewServiceRegistry(fakeCatalog{
		"redis": {Name: "redis", Git: "https://git/redis.git", Ref: "v2.0.0"},
		"noop":  {Name: "noop", Git: "file:///srv/noop", Ref: "main"},
	})

	ref, ok := reg.Resolve("redis")
	if !ok {
		t.Fatal("redis not resolved")
	}
	if ref.Git != "https://git/redis.git" || ref.Ref != "v2.0.0" || ref.Name != "redis" {
		t.Errorf("ref = %+v", ref)
	}

	if _, ok := reg.Resolve("unknown"); ok {
		t.Error("unknown service resolved, want false")
	}
}

func TestServiceRegistry_Empty(t *testing.T) {
	reg := NewServiceRegistry(fakeCatalog{})
	if _, ok := reg.Resolve("anything"); ok {
		t.Error("empty registry resolved something")
	}
}
