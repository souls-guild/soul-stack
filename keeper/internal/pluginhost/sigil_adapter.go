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

// Get resolves active permission for (namespace, name) from plugin_sigils registry.
// nil (permission absent / read error) → nil (verify interprets as no_sigil,
// fail-closed).
//
// Single-slot per pair (ADR-026(g), Variant C): (namespace, name) has exactly
// one allowed binary, ref is operator-asserted label within record, not used
// in lookup. Partial unique index allows multiple active records with
// different refs per pair; on collision newest chosen (ListActive
// sorts allowed_at DESC, id DESC — first match = last allow).
//
// Manifest is byte-exact RAW manifest.yaml bytes (call-site projects from
// sigil.Sigil.ManifestRaw, NOT JSONB projection); verify passes them through
// NormalizeManifestBytes (S3↔S6 invariant).
func (a *SigilLookupAdapter) Get(namespace, name string) *sharedhost.SigilRecord {
	if a.lister == nil {
		return nil
	}
	recs, err := a.lister.ListActive(context.Background())
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("pluginhost: sigil lookup failed — verify fail-closed",
				slog.String("namespace", namespace),
				slog.String("name", name),
				slog.Any("error", err),
			)
		}
		return nil
	}
	for _, rec := range recs {
		if rec.Namespace == namespace && rec.Name == name {
			return rec
		}
	}
	return nil
}
