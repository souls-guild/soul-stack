package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/secretwrite"
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
// как ПУТЬ; сами credentials Service НЕ резолвит и НЕ возвращает. Dual-mode
// приём (ADR-064, NIM-11): оператор может передать credentials значением
// (plaintext map) вместо ref — keeper пишет их в Vault и хранит только ref.
type Service struct {
	pool ExecQueryRower
	// secretWriter/acceptPlaintext — dual-mode приём plaintext-credentials
	// (ADR-064). secretWriter=nil ИЛИ acceptPlaintext=false → plaintext
	// отвергается, принимается только credentials_ref (secure default).
	secretWriter    SecretWriter
	acceptPlaintext bool
}

// SecretWriter — узкая поверхность материализации plaintext-credentials в Vault
// (реализуется *secretwrite.Writer). nil → dual-mode plaintext недоступен.
type SecretWriter interface {
	WriteMap(ctx context.Context, domain, entity, field string, data map[string]any) (string, error)
}

// ServiceDeps — зависимости [Service]. Pool обязателен; SecretWriter/
// AcceptPlaintext — dual-mode приём plaintext-credentials (ADR-064), опц.
type ServiceDeps struct {
	Pool            ExecQueryRower
	SecretWriter    SecretWriter
	AcceptPlaintext bool
}

// NewService собирает service. Pool обязателен.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("provider: NewService: pool is nil")
	}
	return &Service{
		pool:            d.Pool,
		secretWriter:    d.SecretWriter,
		acceptPlaintext: d.AcceptPlaintext,
	}, nil
}

// ErrValidation — обёртка над service-валидацией credentials-входа (XOR,
// формат ref, plaintext-disabled). Handler маппит её в 422; public-detail
// безопасен (без plaintext — см. [PublicMessage]).
var ErrValidation = errors.New("provider: validation failed")

// ErrPlaintextDisabled — оператор передал plaintext-credentials, но приём выключен
// (ADR-064 митигация a: требуется TLS-фронт Operator API/MCP). Заворачивается в
// [ErrValidation] → 422.
var ErrPlaintextDisabled = errors.New("plaintext credential ingestion disabled (enable secret_ingest.accept_plaintext on a TLS-fronted Operator API, or provide credentials_ref)")

// IsValidationError — true, если err — service-валидация credentials-входа.
func IsValidationError(err error) bool { return errors.Is(err, ErrValidation) }

// PublicMessage возвращает безопасный для клиента текст валидационной ошибки
// (trim обёртки/pkg-префикса). Caller проверяет IsValidationError первым.
func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimPrefix(err.Error(), "provider: validation failed: ")
	return strings.TrimPrefix(msg, "provider: ")
}

// wrapValidation оборачивает валидационную ошибку в [ErrValidation] (двойной %w
// сохраняет errors.Is и до ErrValidation, и до вложенного sentinel-а).
func wrapValidation(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrValidation, err)
}

// CreateInput — параметры [Service.Create].
type CreateInput struct {
	Name           string
	Type           string
	Region         string
	CredentialsRef string
	// Credentials — опц. plaintext cloud-credentials (dual-mode, ADR-064): multi-
	// field map (напр. {access_key, secret_key}), XOR с CredentialsRef. keeper
	// пишет их в Vault (secret/provider/<name>/credentials) и заменяет на
	// внутренний ref; plaintext не персистится.
	Credentials map[string]any
	// FQDNSuffix — опц. суффикс FQDN VM провайдера (self-onboard Вариант T,
	// ADR-017(h)). nil → self-onboard недоступен для провайдера.
	FQDNSuffix *string
	CallerAID  string
}

// Create вставляет новый Provider.
//
// Dual-mode credentials (ADR-064): ровно один из Credentials(plaintext) /
// CredentialsRef. При Credentials keeper пишет их в Vault и хранит только ref;
// plaintext не персистится. Валидация name/type/region/credentials_ref — в
// [Insert] + [resolveCredentials]. Возврат:
//   - [ErrProviderAlreadyExists] на UNIQUE по name;
//   - [ErrValidation]-обёртка на битый credentials-вход (XOR/format/disabled).
func (s *Service) Create(ctx context.Context, in CreateInput) (*Provider, error) {
	credRef, written, err := s.resolveCredentials(ctx, in)
	if err != nil {
		return nil, err
	}

	var createdBy *string
	if in.CallerAID != "" {
		aid := in.CallerAID
		createdBy = &aid
	}
	p := &Provider{
		Name:           in.Name,
		Type:           in.Type,
		Region:         in.Region,
		CredentialsRef: credRef,
		FQDNSuffix:     in.FQDNSuffix,
		CreatedByAID:   createdBy,
	}
	if err := Insert(ctx, s.pool, p); err != nil {
		return nil, err
	}
	p.SecretWritten = written
	return p, nil
}

// resolveCredentials реализует dual-mode credentials (ADR-064): возвращает
// финальный credentials_ref (либо операторский, либо keeper-записанный),
// маркер записи и ошибку. plaintext → WriteMap(secret/provider/<name>/credentials).
func (s *Service) resolveCredentials(ctx context.Context, in CreateInput) (string, bool, error) {
	hasPlain := len(in.Credentials) > 0
	hasRef := in.CredentialsRef != ""

	if hasPlain && hasRef {
		return "", false, wrapValidation(errors.New("credentials and credentials_ref are mutually exclusive (provide exactly one)"))
	}
	if !hasPlain {
		if !hasRef {
			return "", false, wrapValidation(errors.New("one of credentials or credentials_ref is required"))
		}
		if !ValidCredentialsRef(in.CredentialsRef) {
			return "", false, wrapValidation(fmt.Errorf("credentials_ref must start with %q and carry a path", CredentialsRefPrefix))
		}
		return in.CredentialsRef, false, nil
	}

	// plaintext-режим.
	if !s.acceptPlaintext || s.secretWriter == nil {
		return "", false, wrapValidation(ErrPlaintextDisabled)
	}
	// entity=<name> обязан быть безопасным сегментом пути ДО записи в Vault.
	if !ValidName(in.Name) {
		return "", false, wrapValidation(fmt.Errorf("invalid name %q (must match %s)", in.Name, NamePattern))
	}
	ref, err := s.secretWriter.WriteMap(ctx, secretwrite.DomainProvider, in.Name, "credentials", in.Credentials)
	if err != nil {
		// err от secretwrite не несёт значений credentials; Vault-сбой — internal.
		return "", false, fmt.Errorf("provider: materialize credentials: %w", err)
	}
	return ref, true, nil
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
