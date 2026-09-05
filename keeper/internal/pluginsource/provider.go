// Package pluginsource — resolution of `keeper.yml::plugins.*` catalog entries into
// immutable host-cache slots, and the boundary between the KINDS of source a plugin
// can come from (NIM-793).
//
// # Why an interface exists now
//
// Until this package there was exactly one resolver ([plugingit.Resolver]) and no
// boundary worth drawing: one implementation is not an abstraction, it is a package.
// A second kind is what makes the boundary real, so it is drawn here rather than
// retrofitted later around whatever the second one happened to need.
//
// What the boundary is FOR, precisely: everything downstream of resolve — the slot
// layout, `plugin.allow`, the signature, the broadcast — must not learn a third time
// where bytes came from. A provider's whole job is to turn "an operator declared this"
// into "these verified bytes sit in this immutable slot", and the shape it hands back
// ([Resolved]) is the same whichever kind produced it.
//
// # What does NOT move behind the interface
//
// ★ Keeper stays the only thing that resolves a `ref` into concrete bytes and signs
// them. Moving that to the Soul would mean two Souls asking one registry for one tag
// and legitimately receiving different bytes — at which point an allow-list keyed on a
// hash stops meaning anything. That is the reason Keeper is in this path at all, and
// no provider gets to skip it.
package pluginsource

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
)

// Resolved is one catalog entry turned into a populated immutable slot: the identity
// the grant will be signed under, the files that are now on disk, and the disclosure
// they carry.
//
// Source is the single "where these bytes came from" value, whichever catalog key
// spelled it — the git remote or the publication base URL. It is what gets signed, so
// the two keys never travel past this package.
type Resolved struct {
	// Alias is the REGISTRATION alias, address level 1, taken from the catalog entry
	// (`name:`) and from nowhere else. The source has no say in it: the artifact
	// carries no self-name, so what the operator declared is the only identity the
	// slot can have.
	Alias string
	// Kind is the source kind that produced the slot.
	Kind string
	// Source / Ref are the artifact's signed identity.
	Source string
	Ref    string
	// Origin is the provenance marker for audit, OUTSIDE the signature: the resolved
	// commit for the git kind, empty for the artifact kind, whose provenance is
	// already the signed (source, ref) plus the per-file digests. Empty means "this
	// kind has none", never "we failed to read it" — a git resolve that cannot pin a
	// commit fails instead of returning an empty Origin.
	Origin string
	// SlotDir is the absolute path of the immutable slot.
	SlotDir string
	// Contents is the slot read back after materialization, by the same code
	// `plugin.allow` reads it with.
	Contents *pluginhost.SlotContents
}

// Provider resolves the catalog entries of ONE source kind.
//
// A provider owns everything between "the operator declared this" and "these bytes are
// in this immutable slot": reaching the source, enforcing whatever bound applies to
// that transport, and writing the slot atomically. It does NOT sign, does not consult
// the grant registry, and does not execute what it fetched — the artifact it just
// wrote is precisely the thing that is not yet approved.
type Provider interface {
	// Kind is the `kind:` value this provider answers for.
	Kind() string
	// Resolve populates the slot for one entry. Every failure is per-entry: the
	// caller turns it into a warning and moves on, because one unreachable source
	// must not stop a Keeper from starting with the plugins it can resolve.
	Resolve(ctx context.Context, e config.PluginCatalogEntry) (Resolved, error)
}

// Catalog dispatches catalog entries to the provider for their kind.
//
// It also does the one check that belongs to no kind: the entry's `name` must be a
// usable registration alias. That is settled BEFORE any provider runs and therefore
// before a byte of egress — a name that cannot be registered is not worth reaching a
// network for, and it is the same rule for a git remote and a published release.
type Catalog struct {
	providers map[string]Provider
	logger    *slog.Logger
}

// NewCatalog builds the dispatcher. A duplicate kind is an error rather than a
// last-one-wins: two providers for one kind means the catalog's meaning depends on
// wire-up order.
func NewCatalog(logger *slog.Logger, providers ...Provider) (*Catalog, error) {
	if logger == nil {
		logger = slog.Default()
	}
	byKind := make(map[string]Provider, len(providers))
	for _, p := range providers {
		if p == nil {
			return nil, fmt.Errorf("pluginsource: nil provider")
		}
		k := p.Kind()
		if !sharedplugin.ValidSourceKind(k) {
			return nil, fmt.Errorf("pluginsource: provider declares unknown kind %q", k)
		}
		if _, dup := byKind[k]; dup {
			return nil, fmt.Errorf("pluginsource: two providers for kind %q", k)
		}
		byKind[k] = p
	}
	return &Catalog{providers: byKind, logger: logger}, nil
}

// ResolveCatalog resolves the whole catalog — ssh_providers + soul_modules —
// dispatching each entry by its kind.
//
// There is no `cloud_drivers` list: NIM-761 removed the CloudDriver contract, and with
// it that catalog key.
//
// Per-entry errors become warnings (fail-closed per entry): a broken entry is skipped
// and Keeper still starts, because the alternative is one typo in one plugin taking
// down a cluster. Returns (the slots that resolved, warnings, fatal error); fatal is
// only what breaks resolve IN PRINCIPLE. nil plugins → empty result.
func (c *Catalog) ResolveCatalog(ctx context.Context, plugins *config.KeeperPlugins) ([]Resolved, []string, error) {
	if plugins == nil {
		return nil, nil, nil
	}
	entries := make([]config.PluginCatalogEntry, 0,
		len(plugins.SSHProviders)+len(plugins.SoulModules))
	entries = append(entries, plugins.SSHProviders...)
	entries = append(entries, plugins.SoulModules...)

	var (
		slots    []Resolved
		warnings []string
	)
	for _, e := range entries {
		slot, err := c.ResolveEntry(ctx, e)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"plugin %q (kind=%s source=%q ref=%q): %v",
				e.Name, e.ResolvedKind(), e.SourceURL(), e.Ref, err))
			continue
		}
		slots = append(slots, slot)
	}
	return slots, warnings, nil
}

// ResolveEntry resolves one catalog entry: alias check, then the provider for its kind.
//
// An entry naming a kind nothing is wired for is an error and not a skip. "Nobody
// resolves this" and "this resolved" must not look the same to an operator reading the
// startup log, because the second one is what they will assume.
func (c *Catalog) ResolveEntry(ctx context.Context, e config.PluginCatalogEntry) (Resolved, error) {
	// The catalog's `name` IS the registration alias (until NIM-437 builds the real
	// registry). The same call the resolvers make — one list, one shape, no second
	// opinion — and made here so it costs no egress.
	if err := ValidateAlias(e.Name); err != nil {
		return Resolved{}, err
	}
	kind := e.ResolvedKind()
	p, ok := c.providers[kind]
	if !ok {
		return Resolved{}, fmt.Errorf("%w: no provider for kind %q", ErrKindUnsupported, kind)
	}
	return p.Resolve(ctx, e)
}
