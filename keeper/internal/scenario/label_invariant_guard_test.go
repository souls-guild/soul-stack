package scenario

// THE INVARIANT, guarded at the ONE place a `service_registry` row turns into an
// artifact address ([ADR-0085], NIM-728): a Service's `label` is not part of it.
//
// [ServiceRegistry.Resolve] is the whole mapping from the registry row to
// [artifact.ServiceRef], and that Ref's `Name` becomes the on-disk cache
// directory `<cacheRoot>/<name>/<sha1>` (keeper/internal/artifact/cache.go). It
// is the last point where a caption and an identifier are both in scope: from
// here on the artifact layer sees strings and cannot tell which it was handed.
// So a caption substituted here would move every snapshot directory on every
// label edit — a silent re-clone at best, and a cache that never hits at worst.
//
// The same Ref.Name also becomes segment 2 of every derived secret path
// ([ADR-0083] §1), so this one substitution reaches both the disk and Vault.
//
// HOW TO BREAK IT ON PURPOSE (the mutation this file exists to catch):
// in ServiceRegistry.Resolve, return `artifact.ServiceRef{Name: derefLabel(e.Label), …}`
// instead of `e.Name`. Both tests below go red.
//
// The fixture caption is deliberately a VALID service identifier — kebab, and
// accepted by artifact's own `reCacheName` — so a naive substitution produces a
// perfectly usable directory name and no validation anywhere can be what fails.

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

const (
	// guardServiceID is the identifier: the cache directory and Vault segment 2.
	guardServiceID = "redis"
	// guardServiceLabel is the caption. Different, and itself a well-formed
	// service identifier — see the file header.
	guardServiceLabel = "redis-billing"
)

// guardCatalog is a one-entry [ServiceCatalog] whose row carries both a
// identifier and a caption.
type guardCatalog struct{ entry serviceregistry.ServiceEntry }

func (c guardCatalog) Resolve(name string) (serviceregistry.ServiceEntry, bool) {
	if name != c.entry.Name {
		return serviceregistry.ServiceEntry{}, false
	}
	return c.entry, true
}

func guardCatalogWith(label *string) guardCatalog {
	return guardCatalog{entry: serviceregistry.ServiceEntry{
		Name:  guardServiceID,
		Label: label,
		Git:   "https://git.example.test/redis.git",
		Ref:   "v1.2.3",
	}}
}

// TestServiceLabel_NotTheArtifactAddress drives the real resolver with a row
// whose caption differs from its identifier and pins that the artifact address —
// which becomes the snapshot directory — is the identifier.
func TestServiceLabel_NotTheArtifactAddress(t *testing.T) {
	label := guardServiceLabel
	reg := NewServiceRegistry(guardCatalogWith(&label))

	ref, ok := reg.Resolve(guardServiceID)
	if !ok {
		t.Fatal("Resolve did not find the fixture service — the guard would pass vacuously")
	}
	if ref.Name != guardServiceID {
		t.Errorf("resolved ServiceRef.Name = %q, want the IDENTIFIER %q\n"+
			"ADR-0085: a Service's label participates in nothing derived. This Name becomes "+
			"the snapshot directory <cacheRoot>/<name>/<sha1> AND segment 2 of every derived "+
			"secret path — a caption here moves both on every label edit.",
			ref.Name, guardServiceID)
	}
	if strings.Contains(ref.Name, guardServiceLabel) {
		t.Errorf("the CAPTION %q reached the artifact address %q", guardServiceLabel, ref.Name)
	}
	// Git/Ref are the other two coordinates; a caption in either would point the
	// loader at a different repository or revision entirely.
	if strings.Contains(ref.Git, guardServiceLabel) || strings.Contains(ref.Ref, guardServiceLabel) {
		t.Errorf("the CAPTION %q reached the git coordinates (%q @ %q)",
			guardServiceLabel, ref.Git, ref.Ref)
	}
}

// TestServiceLabel_ArtifactAddressIgnoresCaptionChanges is the invariant as the
// operator experiences it: re-caption the service, and the artifact address is
// byte-identical, so nothing is re-cloned and no secret path moves.
func TestServiceLabel_ArtifactAddressIgnoresCaptionChanges(t *testing.T) {
	resolve := func(t *testing.T, label *string) string {
		t.Helper()
		ref, ok := NewServiceRegistry(guardCatalogWith(label)).Resolve(guardServiceID)
		if !ok {
			t.Fatal("Resolve did not find the fixture service")
		}
		return ref.Name
	}

	first := guardServiceLabel
	second := "Redis — Billing (production)"
	before := resolve(t, &first)
	after := resolve(t, &second)
	none := resolve(t, nil)

	if before != after || before != none {
		t.Errorf("the artifact address moved when the caption changed: %q → %q (and %q with no caption).\n"+
			"ADR-0085: \"I changed the label and nothing moved\" must be a guarantee, not a hope.",
			before, after, none)
	}
}
