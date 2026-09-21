package pluginsource

import (
	"errors"
	"fmt"
	"strings"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// Sentinels every provider shares. They are here rather than in each provider because
// an operator reading a startup warning should not have to learn which resolver
// produced it to know what went wrong.
var (
	// ErrAliasInvalid — the catalog's `name` is not a usable registration alias:
	// malformed ([sharedplugin.AliasPattern]) or on the closed reserved list
	// ([sharedplugin.IsReserved]). Fail-closed per entry — a reserved alias would
	// shadow an engine address such as `core.file.present`.
	ErrAliasInvalid = errors.New("pluginsource: invalid registration alias")

	// ErrKindUnsupported — the entry names a source kind no provider is wired for.
	// An error and not a skip: "nobody resolves this" must not read like "resolved".
	ErrKindUnsupported = errors.New("pluginsource: unsupported source kind")

	// ErrEntryInvalid — the entry is well-formed yaml but not a resolvable
	// declaration: a missing ref, a missing artifact list, a path that leaves the
	// source. The schema phase reports these with a yaml path; this is the same rule
	// enforced where the bytes are actually fetched, for a catalog that reached a
	// provider without passing through it.
	ErrEntryInvalid = errors.New("pluginsource: invalid catalog entry")

	// ErrSourceUnavailable — the source could not be reached or did not answer with
	// what was declared (transport failure, auth, timeout, HTTP status, a digest that
	// does not match).
	ErrSourceUnavailable = errors.New("pluginsource: source unavailable")

	// ErrSchemaUnreadable — the artifact carries no readable schema document: no
	// trailer, a malformed trailer, or a document that does not validate. The schema
	// is the disclosure an operator approves, so an artifact that cannot state what
	// it offers is refused here rather than cached and approved blind.
	ErrSchemaUnreadable = errors.New("pluginsource: artifact carries no readable schema document")

	// ErrArtifactTooLarge — a fetched artifact exceeds `plugins.max_artifact_size_mb`
	// (ADR-026(g) hardening): an adversarial or simply huge artifact must not fill the
	// keeper-host cache. Fail-closed — the slot is not created.
	ErrArtifactTooLarge = errors.New("pluginsource: artifact exceeds size limit")
)

// ValidateAlias checks a catalog entry's `name` as a REGISTRATION ALIAS: well-formed
// ([sharedplugin.AliasPattern]) and not on the closed reserved list.
//
// Both halves matter for a different reason. The shape keeps an alias usable as a
// directory name and as address level 1 (no dots, no slashes, no uppercase). The
// reserved list keeps an operator from naming a plugin `core`, which would let
// `core.file.present` in a diff mean somebody's plugin instead of the engine — the
// reason the list exists at all now that an operator, not the artifact, picks the word.
//
// This is the one definition; the providers call it rather than each carrying a copy,
// so a change to the alias rule cannot reach one kind of source and miss the other.
func ValidateAlias(alias string) error {
	switch {
	case alias == "":
		return fmt.Errorf("%w: empty alias", ErrAliasInvalid)
	case !sharedplugin.ValidAlias(alias):
		return fmt.Errorf("%w: %q must match %s", ErrAliasInvalid, alias, sharedplugin.AliasPattern)
	case sharedplugin.IsReserved(alias):
		return fmt.Errorf("%w: %q is reserved (%s)", ErrAliasInvalid, alias,
			strings.Join(sharedplugin.ReservedNames(), ", "))
	}
	return nil
}
