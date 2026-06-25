package profile

import (
	"context"
	"errors"
	"fmt"
)

// Service — бизнес-логика CRUD реестра `profiles` (ADR-017,
// docs/keeper/cloud.md). Один источник правды для HTTP-handler-ов и
// MCP-tool-handler-ов (симметрично provider.Service / pushprovider.Service).
//
// Profile — VM-spec поверх Provider-а: `Params` (jsonb, freeform VM-spec) +
// optional `CloudInit`. Валидация Params против CloudDriver.Schema —
// scenario-слой (Cloud.CRUD.b), не CRUD. `provider`-FK на существующий
// Provider проверяет БД ([ErrProviderNotFound] → 422).
type Service struct {
	pool ExecQueryRower
}

// NewService собирает service. pool обязателен.
func NewService(pool ExecQueryRower) (*Service, error) {
	if pool == nil {
		return nil, errors.New("profile: NewService: pool is nil")
	}
	return &Service{pool: pool}, nil
}

// CreateInput — параметры [Service.Create].
type CreateInput struct {
	Name      string
	Provider  string
	Params    map[string]any
	CloudInit *string
	CallerAID string
}

// Create вставляет новый Profile.
//
// Валидация name/provider выполняется в [Insert]. Возврат:
//   - [ErrProfileAlreadyExists] на UNIQUE по name;
//   - [ErrProviderNotFound] на FK-violation (ссылка на несуществующий
//     Provider) → handler маппит в 422;
//   - доменная ошибка валидации (invalid name/provider).
func (s *Service) Create(ctx context.Context, in CreateInput) (*Profile, error) {
	var createdBy *string
	if in.CallerAID != "" {
		aid := in.CallerAID
		createdBy = &aid
	}
	p := &Profile{
		Name:         in.Name,
		Provider:     in.Provider,
		Params:       in.Params,
		CloudInit:    in.CloudInit,
		CreatedByAID: createdBy,
	}
	if err := Insert(ctx, s.pool, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Get читает один Profile по PK. [ErrProfileNotFound] при отсутствии.
func (s *Service) Get(ctx context.Context, name string) (*Profile, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("profile: invalid name %q (must match %s)", name, NamePattern)
	}
	return SelectByName(ctx, s.pool, name)
}

// Delete удаляет Profile по PK. [ErrProfileNotFound] при отсутствии.
func (s *Service) Delete(ctx context.Context, name string) error {
	return Delete(ctx, s.pool, name)
}

// List возвращает страницу Profile-ей и общее количество. providerName
// непуст → фильтр по Provider-у (SelectByProvider).
func (s *Service) List(ctx context.Context, providerName string, offset, limit int) ([]*Profile, int, error) {
	if providerName != "" {
		return SelectByProvider(ctx, s.pool, providerName, offset, limit)
	}
	return SelectAll(ctx, s.pool, offset, limit)
}
