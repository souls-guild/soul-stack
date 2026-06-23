package redis

// OIDCFlowStore — короткоживущий server-side store состояния OIDC code-flow
// (ADR-058(b), стадия 2). Между /auth/oidc/login и /auth/oidc/callback Keeper
// должен помнить per-flow секреты, которые НЕ кладутся в URL/браузер:
//
//   - nonce — anti-replay id_token (сверяется с claim `nonce`);
//   - code_verifier — PKCE-секрет (S256-challenge ушёл IdP в login-URL, verifier
//     остаётся на сервере и предъявляется при code-exchange).
//
// Ключ — opaque `state` (CSRF-токен, единственное, что уходит в браузер и
// возвращается на callback). Store cluster-shared (любой Keeper-инстанс может
// принять callback после login на другом — stateless-кластер ADR-002): Redis,
// а не in-memory map.
//
// Single-use: Consume атомарно читает И удаляет запись (GETDEL). Повторный
// callback с тем же state ничего не найдёт → отказ. Это закрывает replay
// authorization-code и double-submit. TTL (~5 мин) ограничивает окно
// незавершённого flow.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrOIDCFlowNotFound — Consume не нашёл записи по state: либо неизвестный/
// подделанный state (CSRF), либо уже потреблённый (replay), либо истёкший TTL.
// Все три наружу неразличимы (anti-oracle) — endpoint маппит в общий отказ.
var ErrOIDCFlowNotFound = errors.New("redis: oidc flow state not found")

// oidcFlowKeyPrefix — namespace ключей flow-state. Отделён от lease/heartbeat,
// чтобы не пересекаться с другими координационными ключами.
const oidcFlowKeyPrefix = "oidc:flow:"

// OIDCFlowState — server-side секреты одного code-flow. Сериализуется в Redis
// как JSON под ключом state. Ни одно поле НЕ покидает сервер в браузер.
type OIDCFlowState struct {
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"code_verifier"`
}

// OIDCFlowStore — Redis-backed реализация store-а. nil-safe не нужен: OIDC-
// endpoint монтируется только при наличии Redis (ADR-006/ADR-053), без Redis
// flow невозможен (cluster-shared требование).
type OIDCFlowStore struct {
	client *Client
	ttl    time.Duration
}

// NewOIDCFlowStore конструирует store поверх Redis-клиента. ttl > 0 обязателен
// (нулевой/отрицательный → программная ошибка caller-а, как у Acquire).
func NewOIDCFlowStore(c *Client, ttl time.Duration) (*OIDCFlowStore, error) {
	if c == nil {
		return nil, errors.New("redis.NewOIDCFlowStore: nil client")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("redis.NewOIDCFlowStore: ttl must be > 0, got %v", ttl)
	}
	return &OIDCFlowStore{client: c, ttl: ttl}, nil
}

// Save кладёт состояние под state с TTL. SET NX: если ключ уже существует
// (коллизия 256-битного state практически невозможна, но защищаемся от
// перезаписи активного flow) — ошибка. state непустой (caller генерит crypto/rand).
func (s *OIDCFlowStore) Save(ctx context.Context, state string, fs OIDCFlowState) error {
	if state == "" {
		return errors.New("redis.OIDCFlowStore.Save: empty state")
	}
	payload, err := json.Marshal(fs)
	if err != nil {
		return fmt.Errorf("redis.OIDCFlowStore.Save: marshal: %w", err)
	}
	ok, err := s.client.underlying().SetNX(ctx, oidcFlowKeyPrefix+state, payload, s.ttl).Result()
	if err != nil {
		return fmt.Errorf("redis.OIDCFlowStore.Save: SETNX: %w", err)
	}
	if !ok {
		return errors.New("redis.OIDCFlowStore.Save: state collision")
	}
	return nil
}

// Consume атомарно читает И удаляет запись по state (GETDEL). Запись не найдена
// → [ErrOIDCFlowNotFound]. Single-use: повторный Consume того же state вернёт
// ErrOIDCFlowNotFound (anti-replay).
func (s *OIDCFlowStore) Consume(ctx context.Context, state string) (OIDCFlowState, error) {
	if state == "" {
		return OIDCFlowState{}, ErrOIDCFlowNotFound
	}
	raw, err := s.client.underlying().GetDel(ctx, oidcFlowKeyPrefix+state).Bytes()
	if err != nil {
		// redis.Nil — ключа нет (неизвестный/потреблённый/истёкший state);
		// отличаем от сетевой ошибки (паттерн SoulLeaseOwner).
		if errors.Is(err, redis.Nil) {
			return OIDCFlowState{}, ErrOIDCFlowNotFound
		}
		return OIDCFlowState{}, fmt.Errorf("redis.OIDCFlowStore.Consume: GETDEL: %w", err)
	}
	var fs OIDCFlowState
	if err := json.Unmarshal(raw, &fs); err != nil {
		return OIDCFlowState{}, fmt.Errorf("redis.OIDCFlowStore.Consume: unmarshal: %w", err)
	}
	return fs, nil
}
