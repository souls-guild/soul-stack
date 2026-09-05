package pluginhost

import (
	"context"
	"log/slog"

	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// SigilRecordSource is the surface for reading ONE active grant in verify-form, by the
// registration alias the verify path holds. Returns an already-projected
// [sharedhost.SigilRecord] (the single sigil.Sigil → SigilRecord mapping lives at the
// call-site — `keeper run`), so keeper/internal/pluginhost does not import
// keeper/internal/sigil: sigil already imports pluginhost (ReadSlot/SlotContents), and
// importing back would be a cycle.
//
// keeper reads plugin_sigils DIRECTLY from its DB (pool), unlike Soul, which receives
// grants broadcast over EventStream and keeps them in memory.
//
// By alias and not as a list (NIM-814). The verify path asks about exactly one
// registration, on the hot path of every Spawn; answering it with the whole active set
// meant a `SELECT … WHERE revoked_at IS NULL` per fork, carrying every grant's schema
// document — tens of KiB apiece — to then scan the slice for one alias. There is a
// unique index over the active rows of an alias (plugin_sigils_active_alias_idx), so
// the single-row read is exact, not "the first of possibly several".
//
// Absent is (nil, nil) and unreachable is (nil, err), and the difference is the point:
// see [sharedhost.SigilLookup].
type SigilRecordSource interface {
	GetActive(ctx context.Context, alias string) (*sharedhost.SigilRecord, error)
}

// SigilLookupAdapter bridges the keeper-side plugin_sigils registry (read from
// Postgres) to the verify contract of shared/pluginhost.SigilLookup. The keeper host
// verifies its OWN plugins (SshProvider / keeper-side SoulModule) against trust seals
// it signed itself (ADR-026(f)): the trust anchor is the keeper-Signer's public key,
// and the grants come from the same plugin_sigils registry distributed to Souls.
type SigilLookupAdapter struct {
	source SigilRecordSource
	logger *slog.Logger
}

// NewSigilLookupAdapter wraps a plugin_sigils reader into a shared-compatible
// SigilLookup. A nil source → the adapter always answers "no grant" (verify fails
// closed on no_sigil): protection against a nil dereference on incomplete wire-up
// (Sigil disabled). logger can be nil — then a read error is not logged, only
// returned.
func NewSigilLookupAdapter(source SigilRecordSource, logger *slog.Logger) *SigilLookupAdapter {
	return &SigilLookupAdapter{source: source, logger: logger}
}

// Get resolves the active grant for a registration ALIAS from the plugin_sigils
// registry, on the caller's context.
//
// The alias is the lookup key and is deliberately NOT in the signed block: it is the
// operator's local naming choice, so re-registering the same bytes under a second alias
// must not need a second signature, and forging an alias must not be able to reach a
// signature at all. What was SIGNED is (source, ref) plus the two digests; the alias
// only says which grant to check that signature against.
//
// At most one active grant may carry an alias (plugin_sigils_active_alias_idx,
// migration 113), so the row read here is the only one there could be.
//
// Schema is the byte-exact canonical schema document the signature covers (the
// call-site projects it from sigil.Sigil.Schema); verify hashes exactly those bytes via
// SchemaDigest (S3↔S6 invariant).
//
// A read failure is RETURNED rather than flattened into "no grant". Both refuse the
// spawn, so the gate is unchanged; what changes is what the operator is told. Reported
// as no_sigil, a Postgres outage reaches them as "plugin is not allowed; run
// keeper.plugin.allow …" — an instruction to widen a supply-chain gate in order to work
// around a database being down (NIM-814).
func (a *SigilLookupAdapter) Get(ctx context.Context, alias string) (*sharedhost.SigilRecord, error) {
	if a.source == nil {
		return nil, nil
	}
	rec, err := a.source.GetActive(ctx, alias)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("pluginhost: sigil lookup failed — verify fail-closed",
				slog.String("alias", alias),
				slog.Any("error", err),
			)
		}
		return nil, err
	}
	return rec, nil
}
