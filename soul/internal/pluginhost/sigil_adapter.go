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
	Get(alias string) *keeperv1.PluginSigil
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

// Get resolves the active grant by registration alias and projects
// keeperv1.PluginSigil into shared.SigilRecord. nil (grant didn't arrive) →
// nil (verify treats it as no_sigil).
//
// Schema comes from PluginSigil.Schema — the canonical schema-document bytes from the
// transport (M1), which verify hashes with SchemaDigest (S3↔S6 invariant: not the
// parsed form, not the trailer read from disk).
//
// The WHOLE artifact list is carried across, unfiltered (NIM-793). The signature is
// over the whole list, so dropping the rows for other platforms here would leave a
// record that cannot verify; selecting this host's row is a separate step, done where
// the bytes are checked.
func (a *SigilLookupAdapter) Get(alias string) *sharedhost.SigilRecord {
	if a.cache == nil {
		return nil
	}
	sig := a.cache.Get(alias)
	if sig == nil {
		return nil
	}
	artifacts := make([]sharedhost.SigilArtifact, 0, len(sig.GetArtifacts()))
	for _, a := range sig.GetArtifacts() {
		artifacts = append(artifacts, sharedhost.SigilArtifact{
			OS: a.GetOs(), Arch: a.GetArch(), Path: a.GetPath(), SHA256: a.GetSha256(),
		})
	}
	return &sharedhost.SigilRecord{
		Alias:     sig.GetAlias(),
		Source:    sig.GetSource(),
		Ref:       sig.GetRef(),
		Kind:      sig.GetKind(),
		Artifacts: artifacts,
		Signature: sig.GetSignature(),
		Schema:    sig.GetSchema(),
	}
}
