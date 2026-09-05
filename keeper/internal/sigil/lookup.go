package sigil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
)

// ErrModuleNotAllowed signals no active allow for kind=soul_module by sha256 or
// allowed bytes no longer in cache (current slot moved). Fail-closed:
// keeper distributes ONLY sigil-allowed bytes (epic core.module.installed,
// S2 FetchModule maps to NotFound).
var ErrModuleNotAllowed = errors.New("sigil: module sha256 has no active soul_module sigil")

// LookupModuleBinary resolves a sha256 (hex) to the path of a SoulModule artifact in
// the host cache. Content-addressed guard:
//
//  1. the sha is searched among the ARTIFACTS of ACTIVE plugin_sigils grants whose
//     kind is soul_module — read from the grant's SIGNED schema bytes, not from the
//     cache. The grant is what was approved; the cache is what a resolver last wrote
//     there. Since NIM-793 a grant approves a release, so the search is over its rows:
//     an amd64 Soul and an arm64 Soul ask for different digests under one grant, and
//     both are answered from the same approval;
//  2. the slot `<cacheRoot>/<alias>/current/` is re-read and must still hold an
//     artifact with exactly that digest — otherwise `current` has moved and the
//     approved bytes are gone (row skipped, fail-closed).
//
// The requested sha stays the whole key. Nothing here consults the CALLER's platform:
// the request is content-addressed, and matching it against what the Keeper thinks the
// caller runs would refuse a legitimate fetch on a disagreement about that, not about
// the bytes.
//
// A disallowed sha, a revoked grant, the wrong kind, a missing or moved slot →
// [ErrModuleNotAllowed].
func (s *Service) LookupModuleBinary(ctx context.Context, sha256Hex string) (string, error) {
	sha := strings.ToLower(sha256Hex)
	recs, err := s.store.ListActive(ctx)
	if err != nil {
		return "", fmt.Errorf("sigil: list active sigils: %w", err)
	}
	for _, rec := range recs {
		if !grantApproves(rec, sha) {
			continue
		}
		doc, diags := sharedplugin.ParseDocument(sharedplugin.SchemaFileName, rec.Schema)
		if doc == nil || diag.HasErrors(diags) || doc.Kind != pluginhost.KindSoulModule {
			continue
		}
		slot, err := s.slots.ReadSlot(rec.Alias)
		if err != nil {
			s.logger.Warn("sigil: allowed soul_module has no readable slot — skip",
				slog.String("alias", rec.Alias),
				slog.Any("error", err))
			continue
		}
		path, ok := slotArtifactPath(slot, sha)
		if !ok {
			s.logger.Warn("sigil: slot no longer holds the allowed sha256 (current moved) — skip",
				slog.String("alias", rec.Alias),
				slog.String("allowed_sha256", sha))
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("%w: %s", ErrModuleNotAllowed, sha)
}

// grantApproves reports whether any artifact of the grant carries this digest.
func grantApproves(rec *Sigil, sha string) bool {
	for _, a := range rec.Artifacts {
		if a.SHA256 == sha {
			return true
		}
	}
	return false
}

// slotArtifactPath finds the file in the slot whose digest is sha. ReadSlot has already
// re-derived every digest from disk, so a hit here is a file that currently hashes to
// the approved value and not one that used to.
func slotArtifactPath(slot *pluginhost.SlotContents, sha string) (string, bool) {
	for _, a := range slot.Artifacts {
		if a.SHA256 == sha {
			return a.BinaryPath, true
		}
	}
	return "", false
}
