package pluginhost

import (
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// sigilCache is the read surface of the Sigil runtime cache that the adapter
// needs. Implemented by *sigilcache.Cache; narrowing to an interface keeps
// soul/internal/pluginhost independent of the sigilcache package directly
// (the adapter is bound in cmd/soul at wire-up).
type sigilCache interface {
	Get(namespace, name string) *keeperv1.PluginSigil
}

// SigilLookupAdapter bridges Soul's Sigil runtime cache
// (*sigilcache.Cache, keyed by keeperv1.PluginSigil) to the verify contract
// shared/pluginhost.SigilLookup. This is the single mapping point
// keeperv1.PluginSigil → shared.SigilRecord: shared does NOT pull in
// keeper-proto (verify DTO), the proto dependency stays on the Soul side.
type SigilLookupAdapter struct {
	cache sigilCache
}

// NewSigilLookupAdapter wraps the cache in a shared-compatible SigilLookup.
// A nil cache → the adapter always returns a nil record (verify fails closed
// on no_sigil): guards against nil dereference on incomplete wire-up.
func NewSigilLookupAdapter(cache sigilCache) *SigilLookupAdapter {
	return &SigilLookupAdapter{cache: cache}
}

// Get resolves the active grant by (namespace, name) and projects
// keeperv1.PluginSigil into shared.SigilRecord. nil (grant didn't arrive) →
// nil (verify treats it as no_sigil).
//
// Manifest comes from PluginSigil.Manifest — the RAW manifest.yaml bytes
// from the transport (M1), which verify runs through NormalizeManifestBytes
// (S3↔S6 invariant: not the parsed form, not a file from disk).
func (a *SigilLookupAdapter) Get(namespace, name string) *sharedhost.SigilRecord {
	if a.cache == nil {
		return nil
	}
	sig := a.cache.Get(namespace, name)
	if sig == nil {
		return nil
	}
	return &sharedhost.SigilRecord{
		Namespace:       sig.GetNamespace(),
		Name:            sig.GetName(),
		Ref:             sig.GetRef(),
		BinarySHA256hex: sig.GetBinarySha256(),
		Signature:       sig.GetSignature(),
		Manifest:        sig.GetManifest(),
	}
}
