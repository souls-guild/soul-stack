// Package pg — pgx-pool инициализация для Keeper-а под ADR-005
// (Postgres — единственное холодное хранилище состояния).
//
// Owner-ship pool-а лежит на `keeper/cmd/keeper` (M0.4.2); каждая
// подсистема, нуждающаяся в БД (`shared/audit`, реестры souls/operators,
// Reaper), принимает уже инициализированный `*pgxpool.Pool` через
// конструктор — пакет не держит global state.
package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

// vaultDSNField — имя поля внутри Vault KV-secret, в котором лежит plain
// DSN для Postgres-а. Совпадает с конвенцией [docs/keeper/config.md] и
// дополнением [docs/dev/local-setup.md] (`vault kv put secret/keeper/postgres
// dsn="postgres://..."`).
const vaultDSNField = "dsn"

// ErrEmptyDSN возвращается, если `cfg.DSNRef` пустой. Caller может
// использовать `errors.Is(err, pg.ErrEmptyDSN)` для классификации
// (например, отличать «конфиг не дозаполнен» от «vault недоступен»).
var ErrEmptyDSN = errors.New("pg: empty dsn_ref")

// ErrVaultClientRequired возвращается, когда `dsn_ref` начинается с
// `vault:`, но vault-client не передан. Это инвариант caller-а
// (`keeper/cmd/keeper`): vault-client всегда поднимается до NewPool.
var ErrVaultClientRequired = errors.New("pg: vault client is required for vault:-ref")

// ErrDSNFieldMissing возвращается, если Vault KV по [vaultDSNField]
// отсутствует, пустой или имеет неподдерживаемый тип.
var ErrDSNFieldMissing = fmt.Errorf("pg: %q field missing or empty in Vault KV", vaultDSNField)

// NewPool открывает пул соединений к Postgres-у по `cfg.DSNRef` /
// `cfg.Pool`. Пул не пингует БД — это отдельный вызов [Ping] на старте
// keeper-а после полной инициализации зависимостей.
//
// `vc` (vault-client) обязателен только если `cfg.DSNRef` — vault-ref
// (`vault:<mount>/<path>`); для plain-DSN можно передать nil.
//
// Pool min/max берутся из `cfg.Pool.Min` / `cfg.Pool.Max`. Если оба
// нулевые (фикстуры без явного блока pool) — pgx использует свои
// дефолты (max=4); это поведение принято, чтобы не дублировать default
// pool-config между config-schema и кодом.
func NewPool(ctx context.Context, cfg config.KeeperPostgres, vc *keepervault.Client) (*pgxpool.Pool, error) {
	dsn, err := ResolveDSN(ctx, vc, cfg.DSNRef)
	if err != nil {
		return nil, err
	}
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse DSN: %w", err)
	}
	if cfg.Pool.Max > 0 {
		pcfg.MaxConns = int32(cfg.Pool.Max)
	}
	if cfg.Pool.Min > 0 {
		pcfg.MinConns = int32(cfg.Pool.Min)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("pg: new pool: %w", err)
	}
	return pool, nil
}

// ResolveDSN превращает `dsn_ref` из config-а в plain-DSN. Поддерживает:
//
//   - plain DSN: `postgres://...` / `postgresql://...` — возвращается as-is
//     (ctx/vc игнорируются).
//   - vault-ref: `vault:<mount>/<path>` — читает поле `dsn` из Vault KV.
//     `vc` обязателен; иначе [ErrVaultClientRequired].
//
// Пустой `ref` → [ErrEmptyDSN]; отсутствует/пустое поле `dsn` в Vault →
// [ErrDSNFieldMissing]; поле не string → error.
//
// Экспортирован, чтобы `keeper/internal/migrate.Apply` мог получить тот
// же DSN, что [NewPool], без двойной vault-resolve-логики.
func ResolveDSN(ctx context.Context, vc *keepervault.Client, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("%w", ErrEmptyDSN)
	}
	if !strings.HasPrefix(ref, "vault:") {
		return ref, nil
	}
	if vc == nil {
		return "", fmt.Errorf("%w: ref=%q", ErrVaultClientRequired, ref)
	}
	path, err := keepervault.ParseRef(ref)
	if err != nil {
		return "", fmt.Errorf("pg: dsn_ref: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return "", fmt.Errorf("pg: read vault %q: %w", path, err)
	}
	return extractDSN(kv)
}

// extractDSN достаёт поле `dsn` из Vault KV payload-а. PM-decision C:
// DSN — always plain string, без base64-fallback (он остаётся в
// bootstrap.extractSigningKey, специфика бинарного ключа).
func extractDSN(kv map[string]any) (string, error) {
	raw, ok := kv[vaultDSNField]
	if !ok {
		return "", ErrDSNFieldMissing
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("pg: vault %q has unsupported type %T (want string)", vaultDSNField, raw)
	}
	if s == "" {
		return "", ErrDSNFieldMissing
	}
	return s, nil
}
