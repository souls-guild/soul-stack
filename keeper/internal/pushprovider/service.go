package pushprovider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// ErrSensitiveNotVaultRef is returned when trying to store a sensitive key
// (secret_id / token / password / private_key) as a plain string in params.
// Sentinel is distinguished for mapping HTTP/MCP to 422 validation-failed.
var ErrSensitiveNotVaultRef = errors.New("pushprovider: sensitive param must be a vault-ref (vault:<path>)")

// sensitiveKeys is an allow-list of names considered secrets. Plaintext
// values for these keys are rejected; vault-ref (vault:<path>) is expected.
//
// List is opaque (single source of truth is this package); expansion is
// via ordinary PR. Long-term—capabilities manifest from the SSH plugin itself
// (sensitive_keys[])—but that is post-MVP; for now the allow-list is fixed.
//
// Choice: secret_id / token / password / private_key are typical secret field
// names for Vault AppRole / static-token / static-key / private-PEM
// providers (soul-ssh-vault / soul-ssh-static / and corresponding
// community plugins).
var sensitiveKeys = map[string]struct{}{
	"secret_id":   {},
	"token":       {},
	"password":    {},
	"private_key": {},
}

const vaultRefPrefix = "vault:"

// IsSensitiveKey returns true if key is a known sensitive key. Exported
// for handler UI hints (show the user which keys should be vault-ref).
func IsSensitiveKey(key string) bool {
	_, ok := sensitiveKeys[key]
	return ok
}

// ServiceDeps are the dependencies for [Service]. All fields are immutable after construction.
type ServiceDeps struct {
	Pool      ExecQueryRower
	Publisher RedisPublisher
	Logger    *slog.Logger
}

// Service implements business logic for CRUD of push_providers and invalidation publishing
// (ADR-032 amendment 2026-05-26, S7-2). Single source of truth for HTTP
// handlers and MCP tool handlers; transport facade only decodes input and encodes output;
// business invariants live here.
type Service struct {
	pool      ExecQueryRower
	publisher RedisPublisher
	logger    *slog.Logger
}

// NewService constructs a Service. If Publisher is nil, it is replaced with [NopPublisher]
// (Redis disabled / unit test without broker).
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

// CreateInput are the parameters for [Service.Create].
type CreateInput struct {
	Name      string
	Params    map[string]any
	CallerAID string
}

// Create inserts a PushProvider record and publishes invalidation.
//
// Contract:
//   - Validation of name and sensitive fields occurs before insert.
//   - Returns [ErrPushProviderAlreadyExists] on UNIQUE constraint.
//   - Returns [ErrSensitiveNotVaultRef] on plaintext sensitive key value.
//
// Publishing is best-effort: errors are logged (if logger is set) but
// not returned to the caller—the record is already committed.
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

// UpdateInput are the parameters for [Service.Update].
type UpdateInput struct {
	Name      string
	Params    map[string]any
	CallerAID string
}

// Update replaces params of an existing record (replace semantics).
//
// Contract:
//   - Validation of sensitive fields occurs before update.
//   - Returns [ErrPushProviderNotFound] if PK does not exist.
//   - Returns [ErrSensitiveNotVaultRef] on plaintext secret value.
//
// Publishing is best-effort after successful update.
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

// Delete removes a record and publishes invalidation.
//
// Returns [ErrPushProviderNotFound] if the record does not exist.
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

// Get reads a single record by PK. Returns [ErrPushProviderNotFound] if not found.
func (s *Service) Get(ctx context.Context, name string) (*PushProvider, error) {
	return SelectByName(ctx, s.pool, name)
}

// List returns a page of records and the total count.
func (s *Service) List(ctx context.Context, f ListFilter, offset, limit int) ([]*PushProvider, int, error) {
	return SelectAll(ctx, s.pool, f, offset, limit)
}

// validateSensitive checks that values for sensitive keys are vault-refs.
// Non-string values (number/bool/object) under a sensitive key are also rejected:
// a real secret outside vault-ref format makes no sense.
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

// publishChanged is a best-effort wrapper around publisher.PublishPushProvidersChanged.
// Errors are logged (if logger is set) and not returned to the caller: the mutation
// is already committed; loss of publish is compensated by lazy plugin respawn
// on the next keeper restart.
func (s *Service) publishChanged(ctx context.Context, providerName string) {
	if err := s.publisher.PublishPushProvidersChanged(ctx, providerName); err != nil {
		if s.logger != nil {
			s.logger.Warn("pushprovider: publish push-providers:changed failed",
				slog.String("provider", providerName),
				slog.Any("error", err))
		}
	}
}
