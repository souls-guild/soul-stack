package pluginhost

import (
	"context"
	"log/slog"

	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// SigilRecordLister is the surface for reading active permissions in verify-form,
// needed by adapter. Returns already-projected [sharedhost.SigilRecord]
// (single source of sigil.Sigil → SigilRecord mapping held by call-site —
// `keeper run`), so keeper/internal/pluginhost doesn't import
// keeper/internal/sigil: sigil already imports pluginhost (ReadSlot/SlotContents),
// direct import back would create import cycle.
//
// keeper reads plugin_sigils DIRECTLY from its DB (pool), unlike Soul
// which receives permissions broadcast via EventStream and keeps in-memory cache.
type SigilRecordLister interface {
	ListActive(ctx context.Context) ([]*sharedhost.SigilRecord, error)
}

// SigilLookupAdapter bridges keeper-side plugin_sigils registry (read from
// Postgres) to verify-contract of shared/pluginhost.SigilLookup. keeper-host itself
// verifies its OWN plugins (CloudDriver / SshProvider) against trust seals
// that it signed itself (ADR-026(f)): trust-anchor is public key of
// keeper-Signer, source of permissions is same plugin_sigils registry that
// is distributed to Souls.
type SigilLookupAdapter struct {
	lister SigilRecordLister
	logger *slog.Logger
}

// NewSigilLookupAdapter wraps plugin_sigils registry lister into
// shared-compatible SigilLookup. nil-lister → adapter always returns
// nil-record (verify fail-closed on no_sigil): protection from nil-dereference at
// incomplete wire-up (Sigil disabled). logger can be nil — then read
// errors silently swallowed (fail-closed verify protects anyway).
func NewSigilLookupAdapter(lister SigilRecordLister, logger *slog.Logger) *SigilLookupAdapter {
	return &SigilLookupAdapter{lister: lister, logger: logger}
}

// Get resolves the active grant for a registration ALIAS from the plugin_sigils
// registry. nil (no grant / read error) → nil (verify reads it as no_sigil,
// fail-closed).
//
// The alias is the lookup key and is deliberately NOT in the signed block: it is the
// operator's local naming choice, so re-registering the same bytes under a second alias
// must not need a second signature, and forging an alias must not be able to reach a
// signature at all. What was SIGNED is (source, ref) plus the two digests; the alias
// only says which grant to check that signature against.
//
// At most one active grant may carry an alias (plugin_sigils_active_alias_idx,
// migration 113), so the first match is the only match — the answer does not depend on
// row order.
//
// Schema is the byte-exact canonical schema document the signature covers (the
// call-site projects it from sigil.Sigil.Schema); verify hashes exactly those bytes via
// SchemaDigest (S3↔S6 invariant).
func (a *SigilLookupAdapter) Get(alias string) *sharedhost.SigilRecord {
	if a.lister == nil {
		return nil
	}
	recs, err := a.lister.ListActive(context.Background())
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("pluginhost: sigil lookup failed — verify fail-closed",
				slog.String("alias", alias),
				slog.Any("error", err),
			)
		}
		return nil
	}
	for _, rec := range recs {
		if rec.Alias == alias {
			return rec
		}
	}
	return nil
}
