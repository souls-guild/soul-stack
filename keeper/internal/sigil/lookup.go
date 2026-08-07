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
//  1. the sha is searched among ACTIVE plugin_sigils grants whose kind is soul_module —
//     read from the grant's SIGNED schema bytes, not from the cache. The grant is what
//     was approved; the cache is what a resolver last wrote there;
//  2. the slot `<cacheRoot>/<alias>/current/` is re-read and its current BinarySHA256
//     must equal the requested sha — otherwise `current` has moved and the approved
//     bytes are gone (row skipped, fail-closed).
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
		if rec.SHA256 != sha {
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
		if slot.BinarySHA256 != sha {
			s.logger.Warn("sigil: slot artifact differs from allowed sha256 (current moved) — skip",
				slog.String("alias", rec.Alias),
				slog.String("allowed_sha256", sha),
				slog.String("slot_sha256", slot.BinarySHA256))
			continue
		}
		return slot.BinaryPath, nil
	}
	return "", fmt.Errorf("%w: %s", ErrModuleNotAllowed, sha)
}
