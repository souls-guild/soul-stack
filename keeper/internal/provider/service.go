package provider

import (
	"context"
	"errors"
	"fmt"
)

// Service — бизнес-логика CRUD реестра `providers` (ADR-017,
// docs/keeper/cloud.md). Один источник правды для HTTP-handler-ов и
// MCP-tool-handler-ов (симметрично pushprovider.Service): transport-фасад
// только декодирует input/кодирует output, инварианты живут здесь.
//
// В отличие от pushprovider.Service — БЕЗ Redis-publisher: Cloud-Provider
// читается on-demand на scenario-слое (`core.cloud.provisioned`), а не
// hot-reload-ится фоновым dispatcher-ом, поэтому invalidate-pub/sub не нужен.
//
// Секрет-гигиена: `credentials_ref` (= `vault:<path>`) хранится и отдаётся
// как ПУТЬ; сами credentials Service НЕ резолвит и НЕ возвращает (как
// jwt-signing-key-ref). Резолв vault-секрета — задача CloudDriver-вызова на
// scenario-слое, не CRUD.
type Service struct {
	pool ExecQueryRower
}

// NewService собирает service. pool обязателен.
func NewService(pool ExecQueryRower) (*Service, error) {
	if pool == nil {
		return nil, errors.New("provider: NewService: pool is nil")
	}
	return &Service{pool: pool}, nil
}

// CreateInput — параметры [Service.Create].
type CreateInput struct {
	Name           string
	Type           string
	Region         string
	CredentialsRef string
	// FQDNSuffix — опц. суффикс FQDN VM провайдера (self-onboard Вариант T,
	// ADR-017(h)). nil → self-onboard недоступен для провайдера.
	FQDNSuffix *string
	CallerAID  string
}

// Create вставляет новый Provider.
//
// Валидация name/type/region/credentials_ref выполняется в [Insert] (доменные
// инварианты CRUD-слоя). Возврат:
//   - [ErrProviderAlreadyExists] на UNIQUE по name;
//   - доменная ошибка валидации (invalid name/type/region/credentials_ref).
func (s *Service) Create(ctx context.Context, in CreateInput) (*Provider, error) {
	var createdBy *string
	if in.CallerAID != "" {
		aid := in.CallerAID
		createdBy = &aid
	}
	p := &Provider{
		Name:           in.Name,
		Type:           in.Type,
		Region:         in.Region,
		CredentialsRef: in.CredentialsRef,
		FQDNSuffix:     in.FQDNSuffix,
		CreatedByAID:   createdBy,
	}
	if err := Insert(ctx, s.pool, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Get читает один Provider по PK. [ErrProviderNotFound] при отсутствии.
func (s *Service) Get(ctx context.Context, name string) (*Provider, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("provider: invalid name %q (must match %s)", name, NamePattern)
	}
	return SelectByName(ctx, s.pool, name)
}

// Delete удаляет Provider по PK. [ErrProviderNotFound] при отсутствии,
// [ErrProviderHasProfiles] при зависимых Profile-ях (FK RESTRICT).
func (s *Service) Delete(ctx context.Context, name string) error {
	return Delete(ctx, s.pool, name)
}

// List возвращает страницу Provider-ов и общее количество.
func (s *Service) List(ctx context.Context, offset, limit int) ([]*Provider, int, error) {
	return SelectAll(ctx, s.pool, offset, limit)
}
