package pushprovider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// ErrSensitiveNotVaultRef — попытка положить sensitive-ключ (secret_id /
// token / password / private_key) plain-строкой в params. Sentinel выделен
// для маппинга HTTP/MCP в 422 validation-failed.
var ErrSensitiveNotVaultRef = errors.New("pushprovider: sensitive param must be a vault-ref (vault:<path>)")

// sensitiveKeys — allow-list имён, считающихся секретами. Plaintext-
// значение по этим ключам отвергается; ожидается vault-ref `vault:<path>`.
//
// Список opaque (один источник правды — этот пакет); расширение —
// обычный PR. Long-term — capabilities manifest самого SSH-плагина
// (`sensitive_keys[]`), но это пост-MVP: пока allow-list жёсткий.
//
// Выбор: secret_id / token / password / private_key — типичные имена
// secret-поля у Vault AppRole / static-token / static-key / private-PEM
// провайдеров (soul-ssh-vault / soul-ssh-static / соответствующих
// community-плагинов).
var sensitiveKeys = map[string]struct{}{
	"secret_id":   {},
	"token":       {},
	"password":    {},
	"private_key": {},
}

const vaultRefPrefix = "vault:"

// IsSensitiveKey — true, если key — известный sensitive-ключ. Экспорт
// нужен handler-у для UI-подсказок (показать пользователю, какие ключи
// должны быть vault-ref).
func IsSensitiveKey(key string) bool {
	_, ok := sensitiveKeys[key]
	return ok
}

// ServiceDeps — зависимости [Service]. Все поля immutable после
// конструктора.
type ServiceDeps struct {
	Pool      ExecQueryRower
	Publisher RedisPublisher
	Logger    *slog.Logger
}

// Service — бизнес-логика CRUD push_providers + invalidation publish
// (ADR-032 amendment 2026-05-26, S7-2). Один источник правды для HTTP-
// handler-ов и MCP-tool-handler-ов; transport-фасад только декодирует
// input и кодирует output, бизнес-инварианты живут здесь.
type Service struct {
	pool      ExecQueryRower
	publisher RedisPublisher
	logger    *slog.Logger
}

// NewService собирает service. Publisher nil → подменяется [NopPublisher]
// (Redis выключен / unit-тест без брокера).
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("pushprovider: ServiceDeps.Pool is nil")
	}
	publisher := d.Publisher
	if publisher == nil {
		publisher = NopPublisher()
	}
	return &Service{
		pool:      d.Pool,
		publisher: publisher,
		logger:    d.Logger,
	}, nil
}

// CreateInput — параметры [Service.Create].
type CreateInput struct {
	Name      string
	Params    map[string]any
	CallerAID string
}

// Create вставляет запись Push-Provider-а + публикует invalidate.
//
// Контракт:
//   - валидация name / sensitive-полей выполняется до insert-а;
//   - [ErrPushProviderAlreadyExists] на UNIQUE;
//   - [ErrSensitiveNotVaultRef] при plaintext-значении sensitive-ключа.
//
// Publish — best-effort: ошибка логируется (если задан logger), но
// клиенту не возвращается — запись уже committed.
func (s *Service) Create(ctx context.Context, in CreateInput) (*PushProvider, error) {
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("pushprovider: invalid name %q (must match %s)", in.Name, NamePattern)
	}
	if in.CallerAID == "" {
		return nil, fmt.Errorf("pushprovider: caller AID is empty")
	}
	if err := validateSensitive(in.Params); err != nil {
		return nil, err
	}

	p := &PushProvider{
		Name:         in.Name,
		Params:       in.Params,
		CreatedByAID: in.CallerAID,
	}
	if err := Insert(ctx, s.pool, p); err != nil {
		return nil, err
	}
	s.publishChanged(ctx, in.Name)
	return p, nil
}

// UpdateInput — параметры [Service.Update].
type UpdateInput struct {
	Name      string
	Params    map[string]any
	CallerAID string
}

// Update заменяет params существующей записи (replace-семантика).
//
// Контракт:
//   - валидация sensitive-полей выполняется до update;
//   - [ErrPushProviderNotFound], если PK отсутствует;
//   - [ErrSensitiveNotVaultRef] на plaintext-секрете.
//
// Publish — best-effort после успешного update.
func (s *Service) Update(ctx context.Context, in UpdateInput) (*PushProvider, error) {
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("pushprovider: invalid name %q (must match %s)", in.Name, NamePattern)
	}
	if in.CallerAID == "" {
		return nil, fmt.Errorf("pushprovider: caller AID is empty")
	}
	if err := validateSensitive(in.Params); err != nil {
		return nil, err
	}
	if err := Update(ctx, s.pool, in.Name, in.Params, in.CallerAID); err != nil {
		return nil, err
	}
	updated, err := SelectByName(ctx, s.pool, in.Name)
	if err != nil {
		return nil, err
	}
	s.publishChanged(ctx, in.Name)
	return updated, nil
}

// Delete удаляет запись + публикует invalidate.
//
// [ErrPushProviderNotFound] при отсутствии записи.
func (s *Service) Delete(ctx context.Context, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("pushprovider: invalid name %q (must match %s)", name, NamePattern)
	}
	if err := Delete(ctx, s.pool, name); err != nil {
		return err
	}
	s.publishChanged(ctx, name)
	return nil
}

// Get читает одну запись по PK. [ErrPushProviderNotFound] при отсутствии.
func (s *Service) Get(ctx context.Context, name string) (*PushProvider, error) {
	return SelectByName(ctx, s.pool, name)
}

// List возвращает страницу записей и общее количество.
func (s *Service) List(ctx context.Context, f ListFilter, offset, limit int) ([]*PushProvider, int, error) {
	return SelectAll(ctx, s.pool, f, offset, limit)
}

// validateSensitive проверяет, что значения по sensitive-ключам — vault-ref.
// Не-string значения (number/bool/object) под sensitive-ключом тоже
// отвергаются: реальный секрет вне vault-ref-формата не имеет смысла.
func validateSensitive(params map[string]any) error {
	for key, value := range params {
		if !IsSensitiveKey(key) {
			continue
		}
		strValue, ok := value.(string)
		if !ok {
			return fmt.Errorf("%w: key %q has non-string value (%T)",
				ErrSensitiveNotVaultRef, key, value)
		}
		if !strings.HasPrefix(strValue, vaultRefPrefix) || len(strValue) <= len(vaultRefPrefix) {
			return fmt.Errorf("%w: key %q value must start with %q and carry a path",
				ErrSensitiveNotVaultRef, key, vaultRefPrefix)
		}
	}
	return nil
}

// publishChanged — best-effort wrap над publisher.PublishPushProvidersChanged.
// Ошибка логируется (если есть logger) и НЕ возвращается caller-у: мутация
// уже committed; потеря publish-а компенсируется ленивым re-spawn-ом плагина
// при следующем рестарте keeper-а.
func (s *Service) publishChanged(ctx context.Context, providerName string) {
	if err := s.publisher.PublishPushProvidersChanged(ctx, providerName); err != nil {
		if s.logger != nil {
			s.logger.Warn("pushprovider: publish push-providers:changed failed",
				slog.String("provider", providerName),
				slog.Any("error", err))
		}
	}
}
