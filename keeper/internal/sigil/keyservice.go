package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Файл keyservice.go — operator-facing бизнес-логика ротации trust-anchor-ключей
// подписи Sigil (ADR-026(h), R3-S7). Один источник правды для transport-фасадов
// (OpenAPI — handlers/sigil_key.go, MCP — mcp/sigil_key.go): handler декодирует
// input → KeyService-call → маппит sentinel-ы.
//
// KeyService живёт ОТДЕЛЬНО от [Service] (plugin_sigils allow-list): тот про
// допуски конкретных бинарей, этот — про ключи подписи этих допусков. Общий
// пакет — только потому что обе сущности про Sigil (как keys.go vs store.go).
//
// Инвариант безопасности (ADR-026(d)): приватник ed25519-ключа генерируется
// здесь, пишется в Vault KV и СРАЗУ забывается — он НИКОГДА не попадает в
// Postgres (туда едет только pubkey_pem + vault_ref), в ответ API/MCP и в
// логи. Подпись новыми ключами идёт через cluster-wide reload Signer-а (R3-S6),
// который читает приватник primary из Vault по vault_ref.

// VaultWriter — узкая поверхность записи Vault KV, нужная вводу ключа подписи.
// Реализуется [keepervault.Client.WriteKV]; сужение до интерфейса позволяет
// unit-тестировать key-gen+vault-write без реального Vault (симметрично KVReader).
type VaultWriter interface {
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// AnchorsPublisher — поверхность cluster-wide инвалидации набора trust-anchor-ов
// (ADR-026(h), R3-S6). После успешной мутации реестра ключей (Introduce / Retire /
// SetPrimary) [KeyService] публикует сигнал, по которому КАЖДАЯ Keeper-нода
// re-load-ит Signer/набор и re-broadcast-ит SigilTrustAnchors своим Soul-ам.
// Реализуется в `keeper run` адаптером поверх [keeperredis.PublishAnchorsChanged];
// в single-Keeper/dev (без Redis) не подключён — набор доедет на рестарте.
//
// Publish — best-effort: ошибку публикации [KeyService] НЕ возвращает (мутация
// уже зафиксирована в БД), реализация логирует и глотает.
type AnchorsPublisher interface {
	Publish(ctx context.Context)
}

// KeyServiceDeps — зависимости [KeyService]. Pool / Vault обязательны;
// VaultKeyMount — корень пути секрета приватника (см. [KeyService.vaultPath]).
type KeyServiceDeps struct {
	Pool          KeyStorePool
	Vault         VaultWriter
	VaultKeyMount string // корень секрета приватника, напр. "secret/keeper/sigil-keys"
	Logger        *slog.Logger
	Metrics       *KeyMetrics
}

// defaultSigilKeyMount — fallback для пустого [KeyServiceDeps.VaultKeyMount].
// Каждый ключ пишется в `<mount>/<key_id>` (key_id уникален, секреты не
// перетирают друг друга). Развязка с jwt-/одиночным sigil-signing-key
// (разные пути → раздельная ротация, decisions.md G-sigil-3).
const defaultSigilKeyMount = "secret/keeper/sigil-keys"

// KeyService — бизнес-логика ротации ключей подписи Sigil (introduce / retire /
// set-primary / list) поверх CRUD-а keys.go. Deps immutable; состояние не
// держит (атомарность — на уровне keys.go/PG, подпись — primary-приватником из
// Vault через R3-S6 reload).
type KeyService struct {
	pool      KeyStorePool
	vault     VaultWriter
	keyMount  string
	publisher AnchorsPublisher
	logger    *slog.Logger
	metrics   *KeyMetrics
}

// NewKeyService собирает service. Pool / Vault обязательны.
func NewKeyService(d KeyServiceDeps) (*KeyService, error) {
	if d.Pool == nil {
		return nil, errors.New("sigil: KeyServiceDeps.Pool is nil")
	}
	if d.Vault == nil {
		return nil, errors.New("sigil: KeyServiceDeps.Vault is nil")
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	mount := d.VaultKeyMount
	if mount == "" {
		mount = defaultSigilKeyMount
	}
	return &KeyService{
		pool:     d.Pool,
		vault:    d.Vault,
		keyMount: mount,
		logger:   logger,
		metrics:  d.Metrics,
	}, nil
}

// SetPublisher late-binding-ом подключает cluster-wide anchors-publisher (R3-S6).
// Вызывается из `keeper run` после подъёма Redis-клиента (паттерн
// [Service.SetInvalidator]). nil — снять publisher. Идемпотентно.
func (s *KeyService) SetPublisher(p AnchorsPublisher) { s.publisher = p }

// SetMetrics late-binding-ом подключает gauge active-ключей (R3-S7). Вызывается
// из `keeper run` в setupMetricsRegistry (obs.Registry поднимается ПОСЛЕ
// setupSigil). nil-safe (паттерн [vault.Client.SetMetrics]).
func (s *KeyService) SetMetrics(m *KeyMetrics) { s.metrics = m }

// IntroduceResult — итог [KeyService.Introduce]. Несёт ТОЛЬКО публичные данные:
// key_id, публичный ключ (SPKI PEM), флаги. Приватник в результат не входит
// (security-инвариант ADR-026(d)).
type IntroduceResult struct {
	KeyID        string
	PubkeyPEM    string
	IsPrimary    bool
	Status       string
	IntroducedAt time.Time
}

// Introduce генерирует новую ed25519-пару, пишет приватник в Vault KV и вводит
// публичную часть в реестр sigil_signing_keys как active trust-anchor (ADR-026(h),
// R3-S7).
//
// Шаги:
//  1. ed25519.GenerateKey — новая пара;
//  2. key_id = SHA-256(SPKI-DER публичного ключа), hex — стабильный id, не
//     зависит от PEM-обёртки;
//  3. WriteKV(`<keyMount>/<key_id>`, {signing_key: <PKCS#8 PEM приватника>}) —
//     приватник в Vault (НЕ в PG, НЕ в логах, НЕ в ответе);
//  4. keys.Introduce(key_id, pubkey_pem, vault_ref, makePrimary, callerAID).
//
// При фейле PG-вставки (4) запись в Vault (3) остаётся «висеть» — это безвредно
// (приватник без anchor-строки никем не используется), оператор может повторить
// Introduce (другой keypair → другой key_id). Reverse-cleanup Vault-секрета
// СОЗНАТЕЛЬНО не делается: удаление приватника на пути ошибки опаснее
// «висящего» неиспользуемого секрета.
//
// После успеха публикует anchors-changed (R3-S6 reload по кластеру) и обновляет
// gauge active-ключей.
func (s *KeyService) Introduce(ctx context.Context, makePrimary bool, callerAID string) (*IntroduceResult, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sigil: generate ed25519 key: %w", err)
	}

	keyID, err := keyIDFromPublic(pub)
	if err != nil {
		return nil, fmt.Errorf("sigil: derive key_id: %w", err)
	}
	pubPEM, err := publicKeyToPEM(pub)
	if err != nil {
		return nil, fmt.Errorf("sigil: encode public key PEM: %w", err)
	}
	privPEM, err := privateKeyToPEM(priv)
	if err != nil {
		return nil, fmt.Errorf("sigil: encode private key PEM: %w", err)
	}

	path := s.vaultPath(keyID)
	if err := s.vault.WriteKV(ctx, path, map[string]any{vaultSigningKeyField: string(privPEM)}); err != nil {
		// БЕЗОПАСНОСТЬ: WriteKV не кладёт значение секрета в текст ошибки; здесь
		// тоже логируем/возвращаем только key_id, не приватник.
		return nil, fmt.Errorf("sigil: write private key to vault (key_id=%s): %w", keyID, err)
	}

	var callerPtr *string
	if callerAID != "" {
		callerPtr = &callerAID
	}
	key, err := Introduce(ctx, s.pool, keyID, string(pubPEM), vaultRefForPath(path), makePrimary, callerPtr)
	if err != nil {
		return nil, err
	}

	s.afterMutation(ctx)
	s.logger.Info("sigil: signing key introduced",
		slog.String("key_id", keyID),
		slog.Bool("is_primary", key.IsPrimary),
		slog.String("by_aid", callerAID),
	)
	return &IntroduceResult{
		KeyID:        key.KeyID,
		PubkeyPEM:    key.PubkeyPEM,
		IsPrimary:    key.IsPrimary,
		Status:       key.Status,
		IntroducedAt: key.IntroducedAt,
	}, nil
}

// SetPrimary делает active-ключ primary (новые Sigil-ы пойдут им после R3-S6
// reload). После успеха публикует anchors-changed. Ошибки CRUD-а keys.go
// прокидываются как есть ([ErrKeyNotFound] / [ErrKeyRetired] / [ErrConcurrentPrimary]).
func (s *KeyService) SetPrimary(ctx context.Context, keyID, callerAID string) error {
	if err := SetPrimary(ctx, s.pool, keyID, callerAID); err != nil {
		return err
	}
	s.afterMutation(ctx)
	s.logger.Info("sigil: signing key set primary",
		slog.String("key_id", keyID), slog.String("by_aid", callerAID))
	return nil
}

// Retire выводит ключ из набора trust-anchor-ов (Soul забывает его при следующем
// SigilTrustAnchors). После успеха публикует anchors-changed. Инварианты keys.go
// (≥1 active, не primary) прокидываются как [ErrLastActiveKey] / [ErrRetirePrimary] /
// [ErrKeyNotFound].
//
// Retire БЕЗОПАСЕН только когда набор уже разошёлся по кластеру И bootstrap-
// источник живой (architect af7d): первое держит R3-S6 (PublishAnchorsChanged →
// reloadAnchors на каждой ноде), второе — живой TrustAnchorSource в bootstrap-
// reply (daemon-проводка). Без них новый Soul между bootstrap и connect получил
// бы устаревший набор и отверг подпись retired-якорем (или принял бы лишний).
func (s *KeyService) Retire(ctx context.Context, keyID, callerAID string) error {
	if err := Retire(ctx, s.pool, keyID, callerAID); err != nil {
		return err
	}
	s.afterMutation(ctx)
	s.logger.Warn("sigil: signing key retired — раздавшийся набор якорей сократился",
		slog.String("key_id", keyID), slog.String("by_aid", callerAID))
	return nil
}

// List возвращает active trust-anchor-ключи (primary первым). Read-only, без
// публикации/audit.
func (s *KeyService) List(ctx context.Context) ([]*SigningKey, error) {
	return ListActiveKeys(ctx, s.pool)
}

// afterMutation — общий хвост успешной мутации (Introduce/SetPrimary/Retire):
// (1) cluster-wide publish anchors-changed (R3-S6 reload), (2) обновление gauge
// active-ключей. Оба best-effort: publish глотает ошибку внутри реализации,
// gauge-refresh при ошибке чтения реестра лишь логирует (метрика останется на
// прежнем значении до следующей мутации/рестарта).
func (s *KeyService) afterMutation(ctx context.Context) {
	if s.publisher != nil {
		s.publisher.Publish(ctx)
	}
	s.refreshActiveGauge(ctx)
}

// PrimeActiveGauge проставляет стартовое значение gauge active-ключей (R3-S7).
// Вызывается один раз в `keeper run` после регистрации метрик, чтобы gauge нёс
// актуальное число ДО первой мутации (иначе остался бы 0 до первого
// Introduce/Retire). nil-metrics → no-op.
func (s *KeyService) PrimeActiveGauge(ctx context.Context) { s.refreshActiveGauge(ctx) }

// refreshActiveGauge перечитывает число active-ключей и проставляет gauge.
// nil-metrics → no-op. Ошибка чтения реестра не критична (метрика не authority).
func (s *KeyService) refreshActiveGauge(ctx context.Context) {
	if s.metrics == nil {
		return
	}
	keys, err := ListActiveKeys(ctx, s.pool)
	if err != nil {
		s.logger.Warn("sigil: refresh active-keys gauge failed", slog.Any("error", err))
		return
	}
	s.metrics.SetActive(len(keys))
}

// vaultPath собирает logical-path секрета приватника по key_id:
// `<keyMount>/<key_id>`. key_id — hex SHA-256 (closed charset, без `..`/слешей),
// безопасен как path-сегмент.
func (s *KeyService) vaultPath(keyID string) string {
	return s.keyMount + "/" + keyID
}

// vaultRefForPath оборачивает logical-path в `vault:`-ref, который ожидает
// [LoadSigningKey] / [keepervault.ParseRef] (формат vault_ref в реестре).
func vaultRefForPath(path string) string { return "vault:" + path }

// keyIDFromPublic вычисляет стабильный key_id = SHA-256(SPKI-DER публичного
// ключа), hex. Совпадает с конвенцией миграции 037 (key_id не зависит от
// PEM-обёртки: whitespace/перенос строки не влияют).
func keyIDFromPublic(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal SPKI: %w", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// privateKeyToPEM кодирует ed25519-приватник в PKCS#8 PEM-блок "PRIVATE KEY".
// Эту форму понимает [parseEd25519Key] (PEM → PKCS#8) при последующей загрузке
// приватника primary из Vault (R3-S6 reload).
func privateKeyToPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal PKCS#8: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
