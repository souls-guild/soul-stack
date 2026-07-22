package sigil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
)

// ErrModuleNotAllowed signals no active allow for kind=soul_module by sha256 or
// allowed bytes no longer in cache (current slot moved). Fail-closed:
// keeper distributes ONLY sigil-allowed bytes (epic core.module.installed,
// S2 FetchModule maps to NotFound).
var ErrModuleNotAllowed = errors.New("sigil: module sha256 has no active soul_module sigil")

// LookupModuleBinary resolves sha256 (hex) to SoulModule plugin binary path
// in host cache. Content-addressed guard:
//
//  1. sha searched among ACTIVE allows plugin_sigils with kind=soul_module
//     (kind from signed manifest bytes of allow, not cache);
//  2. slot `<cacheRoot>/<ns>-<name>/current/` re-read, its current
//     BinarySHA256 must match requested sha — else current moved and allowed
//     bytes gone (record skipped, fail-closed).
//
// Disallowed sha, revoked allow, wrong kind, missing/moved slot →
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
		m, _ := sharedplugin.LoadFromBytes("plugin_sigils.manifest_raw", rec.ManifestRaw)
		if m == nil || m.Kind != pluginhost.KindSoulModule {
			continue
		}
		slot, err := s.slots.ReadSlot(rec.Namespace, rec.Name)
		if err != nil {
			s.logger.Warn("sigil: allowed soul_module has no readable slot — skip",
				slog.String("namespace", rec.Namespace),
				slog.String("name", rec.Name),
				slog.Any("error", err))
			continue
		}
		if slot.BinarySHA256 != sha {
			s.logger.Warn("sigil: slot binary differs from allowed sha256 (current moved) — skip",
				slog.String("namespace", rec.Namespace),
				slog.String("name", rec.Name),
				slog.String("allowed_sha256", sha),
				slog.String("slot_sha256", slot.BinarySHA256))
			continue
		}
		return slot.BinaryPath, nil
	}
	return "", fmt.Errorf("%w: %s", ErrModuleNotAllowed, sha)
}
