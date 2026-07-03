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

// ErrModuleNotAllowed — по sha256 нет активного допуска kind=soul_module либо
// допущенных байтов уже нет в кеше (current-слот переехал). Fail-closed:
// keeper раздаёт ТОЛЬКО sigil-allowed байты (эпик core.module.installed,
// S2 FetchModule маппит в NotFound).
var ErrModuleNotAllowed = errors.New("sigil: module sha256 has no active soul_module sigil")

// LookupModuleBinary резолвит sha256 (hex) в путь к бинарю SoulModule-плагина
// в кеше host-а. Content-addressed guard:
//
//  1. sha ищется среди АКТИВНЫХ допусков plugin_sigils с kind=soul_module
//     (kind — из подписанных байтов manifest-а допуска, не из кеша);
//  2. слот `<cacheRoot>/<ns>-<name>/current/` перечитывается, его текущий
//     BinarySHA256 обязан совпасть с запрошенным sha — иначе current переехал
//     и допущенных байтов больше нет (запись скипается, fail-closed).
//
// Не-allowed sha, revoked-допуск, чужой kind, отсутствующий/уехавший слот →
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
