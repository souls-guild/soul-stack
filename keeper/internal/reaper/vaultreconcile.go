package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// vaultreconcile.go — исполнитель cross-store Reaper-правила
// `reap_orphan_vault_keys` (report-only, ADR-026(h), GATE-2). В отличие от
// [Purger] (чистый pgx-DELETE), это reconcile двух хранилищ: Vault KV (имена
// приватников подписи Sigil) против Postgres-реестра `sigil_signing_keys`
// (авторитетный набор живых ключей). Поэтому отдельный тип, а не метод Purger-а.
//
// Сирота = секрет `secret/keeper/sigil-keys/<key_id>` в Vault, для которого НЕТ
// строки в `sigil_signing_keys` НИ в одном статусе (active И retired считаются
// живыми — retired-приватник нужен для verify старых Sigil-ов). Возникает,
// например, если Introduce записал приватник в Vault, а PG-вставка следом упала:
// keyservice сознательно НЕ делает reverse-cleanup, поэтому Vault-секрет
// остаётся брошенным.
//
// БЕЗОПАСНОСТЬ (инвариант правила):
//   - report-only: правило ТОЛЬКО считает/метрит/логирует — ничего из Vault НЕ
//     удаляет.
//   - приватник НИКОГДА не читается: используются только LIST имён и metadata
//     (created_time), data-путь секрета не запрашивается вовсе.
//   - scope-prefix [orphanScanPrefix] ЗАШИТ в коде, НЕ берётся из конфига —
//     правило физически не может сканировать другой Vault-путь.

// orphanScanPrefix — единственный Vault-prefix, который сканирует правило.
// ЗАШИТ в коде (scope-guard): сирота осмысленна ТОЛЬКО для приватников подписи
// Sigil. relative-форма (mount подставляет vault-client).
const orphanScanPrefix = "keeper/sigil-keys"

// orphanLogCap — сколько key_id осиротевших секретов логировать поимённо.
// Остаток сворачивается в «and N more», чтобы Warn-лог не разрастался при
// массовом дрейфе. key_id = SHA-256 SPKI hex — публичный идентификатор,
// логировать безопасно (приватник в правило не попадает).
const orphanLogCap = 20

// VaultKVLister — узкое подмножество vault-клиента, нужное reconcile-у: LIST
// имён под prefix + read metadata (created_time) для grace. Реальный
// [*keepervault.Client] удовлетворяет автоматически; тесты подставляют fake.
// Намеренно НЕ включает ReadKV(data) — приватник не читаем (security-инвариант).
// Экспортируется ради wiring-а в keeper/cmd/keeper (typed-nil guard).
type VaultKVLister interface {
	ListKV(ctx context.Context, prefix string) ([]string, error)
	ReadKVMetadata(ctx context.Context, path string) (time.Time, error)
}

// liveKeyIDsReader — резолв авторитетного набора живых key_id из Postgres
// (`sigil.ListAllKeyIDs` над pool, все статусы). Узкий интерфейс для подмены
// в unit-тестах без поднятия PG.
type liveKeyIDsReader interface {
	ListAllKeyIDs(ctx context.Context) (map[string]struct{}, error)
}

// VaultReconciler — исполнитель `reap_orphan_vault_keys`. Один экземпляр на
// Keeper-процесс; конкурентно безопасен (vault-client и pool потокобезопасны,
// собственного изменяемого состояния нет).
type VaultReconciler struct {
	vault VaultKVLister
	keys  liveKeyIDsReader
	log   *slog.Logger
	now   func() time.Time
}

// NewVaultReconciler собирает исполнитель. vault может быть nil (Vault не
// настроен в конфиге) — тогда [VaultReconciler.ReportOrphanVaultKeys] вернёт
// (0, error) с понятным сообщением, и runner залогирует fail и пойдёт дальше
// (правило по умолчанию выключено, типовой deploy его не зовёт).
//
// now — источник времени для grace-сравнения; nil → [time.Now] (тесты
// подменяют фиксированным временем).
func NewVaultReconciler(vault VaultKVLister, keys liveKeyIDsReader, log *slog.Logger, now func() time.Time) *VaultReconciler {
	if now == nil {
		now = time.Now
	}
	return &VaultReconciler{vault: vault, keys: keys, log: log, now: now}
}

// ReportOrphanVaultKeys (report-only) находит осиротевшие приватники подписи
// Sigil в Vault и возвращает их количество. НИЧЕГО не удаляет.
//
// Алгоритм:
//  1. LIST имён под [orphanScanPrefix] (Vault metadata-путь, без чтения данных).
//  2. ListAllKeyIDs — set живых из Postgres (все статусы).
//  3. candidates = имена из Vault, которых нет в set.
//  4. Для каждого кандидата (не более batchSize metadata-round-trip-ов за
//     прогон) ReadKVMetadata → если created_time старше now()-grace, это сирота.
//     grace отсекает гонку с Introduce (write-before-PG-commit): свежий секрет
//     ещё может получить PG-строку.
//  5. Возврат count сирот + Warn-лог с key_id (cap [orphanLogCap]).
//
// grace передаётся runner-ом из rule.MaxAge (MaxAge-as-grace, как у
// purge_apply_task_register). batchSize ограничивает число metadata-чтений за
// один прогон.
func (vr *VaultReconciler) ReportOrphanVaultKeys(ctx context.Context, grace time.Duration, batchSize int) (int64, error) {
	if vr.vault == nil {
		return 0, errors.New("reaper: reap_orphan_vault_keys requires a Vault client (vault not configured)")
	}

	names, err := vr.vault.ListKV(ctx, orphanScanPrefix)
	if err != nil {
		return 0, fmt.Errorf("reaper: list vault sigil-keys: %w", err)
	}
	if len(names) == 0 {
		// Сирот нет (или подпапка пуста/отсутствует) — выходим без обращения к PG.
		return 0, nil
	}

	live, err := vr.keys.ListAllKeyIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("reaper: list live sigil key ids: %w", err)
	}

	// candidates — в Vault, но не в PG (ни active, ни retired).
	candidates := make([]string, 0)
	for _, keyID := range names {
		if _, ok := live[keyID]; ok {
			continue
		}
		candidates = append(candidates, keyID)
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	cutoff := vr.now().Add(-grace)
	var orphans []string
	var checked int
	for _, keyID := range candidates {
		// batchSize ограничивает число metadata-round-trip-ов за прогон.
		if batchSize > 0 && checked >= batchSize {
			break
		}
		checked++

		created, err := vr.vault.ReadKVMetadata(ctx, orphanScanPrefix+"/"+keyID)
		if err != nil {
			// Секрет мог быть удалён между LIST и read, либо транспортный сбой.
			// Не валим весь прогон — пропускаем кандидата с Warn-ом.
			vr.log.Warn("reaper: read vault metadata for orphan candidate failed, skipping",
				slog.String("rule", "reap_orphan_vault_keys"),
				slog.String("key_id", keyID),
				slog.Any("error", err),
			)
			continue
		}
		// grace: молодой секрет может ещё получить PG-строку (Introduce
		// write-before-commit) — не считаем сиротой.
		if created.After(cutoff) {
			continue
		}
		orphans = append(orphans, keyID)
	}

	if len(orphans) > 0 {
		logged := orphans
		extra := 0
		if len(logged) > orphanLogCap {
			extra = len(logged) - orphanLogCap
			logged = logged[:orphanLogCap]
		}
		attrs := []any{
			slog.String("rule", "reap_orphan_vault_keys"),
			slog.Int("orphan_count", len(orphans)),
			slog.Any("orphan_key_ids", logged),
		}
		if extra > 0 {
			attrs = append(attrs, slog.Int("orphan_key_ids_omitted", extra))
		}
		// report-only: фиксируем находку, оператор разбирается вручную.
		vr.log.Warn("reaper: orphan vault sigil-keys detected (report-only, nothing deleted)", attrs...)
	}

	return int64(len(orphans)), nil
}
