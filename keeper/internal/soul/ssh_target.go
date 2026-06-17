package soul

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SSHTarget — типизированная форма jsonb-колонки souls.ssh_target (ADR-032
// amendment 2026-05-26, S7-1; расширено amendment 2026-05-27, P2 W-1). Хранит
// per-host SSH-реквизиты push-flow прямо в реестре souls, заменяя pilot-форму
// `keeper.yml::push.targets[]` (S6).
//
// Дефолты опущенных полей (port 22 / user root / soul-path /usr/local/bin/soul)
// резолвятся НЕ здесь, а в keeper/internal/push.PGFallbackTargetResolver:
// storage хранит ТОЛЬКО то, что явно задал оператор (NULL/0/"" → дефолт при
// резолве). Это позволяет в будущем менять дефолты централизованно без
// миграции данных.
//
// `SSHProvider` (P2 W-1) — optional per-SID explicit выбор SshProvider-плагина
// (Level 1 в 3-tier resolve: per-SID → per-coven → cluster-default). nil →
// routing идёт по уровням 2/3 (см. keeper/internal/push/router.go::PGRouter).
// Формат имени тот же, что у push_providers.name (regex `^[a-z][a-z0-9-]{0,62}$`),
// валидируется CHECK `souls_ssh_target_shape` (миграция 056).
type SSHTarget struct {
	SSHPort     int     `json:"ssh_port"`
	SSHUser     string  `json:"ssh_user"`
	SoulPath    string  `json:"soul_path"`
	SSHProvider *string `json:"ssh_provider,omitempty"`
}

const updateSshTargetSQL = `
UPDATE souls
SET ssh_target = $2::jsonb
WHERE sid = $1
RETURNING sid
`

const selectSshTargetSQL = `
SELECT ssh_target
FROM souls
WHERE sid = $1
`

// UpdateSshTarget обновляет souls.ssh_target по sid. target==nil → пишет
// NULL в колонку (снять настроенный target → fallback на keeper.yml под
// флагом push.allow_legacy_push_targets).
//
// Audit-event и RBAC-permission проверяются caller-ом (handler / MCP-tool) —
// слой soul.* остаётся storage-нейтральным к authz, симметрично UpdateCoven /
// UpdateStatus.
//
// Возвращает [ErrSoulNotFound], если SID не существует. Невалидный shape
// jsonb отвергается PG-CHECK souls_ssh_target_shape (миграция 053) — придёт
// CHECK violation, обёрнутая в общий error; handler-сторона матчит на текст
// constraint, но в нормальном flow невалидный shape невозможен (caller
// маршалит из типизированного SSHTarget).
func UpdateSshTarget(ctx context.Context, db ExecQueryRower, sid string, target *SSHTarget) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q (must match %s)", sid, SIDPattern)
	}
	var payload any
	if target != nil {
		b, err := json.Marshal(target)
		if err != nil {
			return fmt.Errorf("soul: marshal ssh_target: %w", err)
		}
		payload = b
	}
	var actualSID string
	err := db.QueryRow(ctx, updateSshTargetSQL, sid, payload).Scan(&actualSID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSoulNotFound
	}
	if err != nil {
		return fmt.Errorf("soul: update ssh_target: %w", err)
	}
	return nil
}

// SelectSshTarget читает souls.ssh_target по SID. Возвращает:
//
//   - (target, nil) — запись Soul-а есть, ssh_target проставлен;
//   - (nil, nil) — запись Soul-а есть, ssh_target IS NULL (target не настроен —
//     fallback на keeper.yml в PGFallbackTargetResolver);
//   - (nil, ErrSoulNotFound) — записи Soul-а нет в реестре;
//   - (nil, fmt.Errorf-wrapped) — ошибка БД или битый JSONB в колонке
//     (CHECK souls_ssh_target_shape делает это невозможным, но guard оставляем).
//
// Это hot-path метод (зовётся из SshDispatcher.SendApply на каждом push-е),
// поэтому без round-trip-ов сверх одного SELECT.
func SelectSshTarget(ctx context.Context, db ExecQueryRower, sid string) (*SSHTarget, error) {
	if !ValidSID(sid) {
		return nil, fmt.Errorf("soul: invalid SID %q (must match %s)", sid, SIDPattern)
	}
	var payload []byte
	err := db.QueryRow(ctx, selectSshTargetSQL, sid).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSoulNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("soul: select ssh_target: %w", err)
	}
	if len(payload) == 0 {
		return nil, nil
	}
	var target SSHTarget
	if err := json.Unmarshal(payload, &target); err != nil {
		return nil, fmt.Errorf("soul: unmarshal ssh_target: %w", err)
	}
	return &target, nil
}
