package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/secretwrite"
)

// Service contains CRUD business logic for the `providers` registry (ADR-017,
// docs/keeper/cloud.md). It is the single source of truth for HTTP handlers and
// MCP tool handlers, symmetric to pushprovider.Service: transport facades only
// decode input / encode output, while invariants live here.
//
// Unlike pushprovider.Service, there is no Redis publisher: Cloud Provider is read
// on demand at the scenario layer (`core.cloud.provisioned`), not hot-reloaded by a
// background dispatcher, so invalidate pub/sub is unnecessary.
//
// Secret hygiene: `credentials_ref` (= `vault:<path>`) is stored and returned as a
// path; Service does not resolve or return credentials themselves. Dual-mode
// ingestion (ADR-064, NIM-11): operator may pass credentials by value (plaintext
// map) instead of ref; Keeper writes them to Vault and stores only the ref.
type Service struct {
	pool ExecQueryRower
	// secretWriter/acceptPlaintext control dual-mode plaintext credential
	// ingestion (ADR-064). secretWriter=nil or acceptPlaintext=false rejects
	// plaintext and accepts only credentials_ref (secure default).
	secretWriter    SecretWriter
	acceptPlaintext bool
}

// SecretWriter is the narrow surface for materializing plaintext credentials in
// Vault (implemented by *secretwrite.Writer). nil means dual-mode plaintext is
// unavailable.
type SecretWriter interface {
	WriteMap(ctx context.Context, domain, entity, field string, data map[string]any) (string, error)
}

// ServiceDeps are [Service] dependencies. Pool is required; SecretWriter/
// AcceptPlaintext are optional dual-mode plaintext credential ingestion
// dependencies (ADR-064).
type ServiceDeps struct {
	Pool            ExecQueryRower
	SecretWriter    SecretWriter
	AcceptPlaintext bool
}

// NewService builds a service. Pool is required.
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

// ErrValidation wraps service validation of credentials input (XOR, ref format,
// plaintext disabled). Handler maps it to 422; public detail is safe and contains
// no plaintext, see [PublicMessage].
var ErrValidation = errors.New("provider: validation failed")

// ErrPlaintextDisabled means the operator passed plaintext credentials while
// ingestion is disabled (ADR-064 mitigation a: TLS-fronted Operator API/MCP is
// required). Wrapped in [ErrValidation] -> 422.
var ErrPlaintextDisabled = errors.New("plaintext credential ingestion disabled (enable secret_ingest.accept_plaintext on a TLS-fronted Operator API, or provide credentials_ref)")

// IsValidationError is true when err is credentials-input service validation.
func IsValidationError(err error) bool { return errors.Is(err, ErrValidation) }

// PublicMessage returns client-safe validation error text by trimming wrapper/pkg
// prefixes. Caller checks IsValidationError first.
func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimPrefix(err.Error(), "provider: validation failed: ")
	return strings.TrimPrefix(msg, "provider: ")
}

// wrapValidation wraps a validation error in [ErrValidation]. Double %w preserves
// errors.Is for both ErrValidation and the nested sentinel.
func wrapValidation(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrValidation, err)
}

// CreateInput contains [Service.Create] parameters.
type CreateInput struct {
	Name           string
	Type           string
	Region         string
	CredentialsRef string
	// Credentials are optional plaintext cloud credentials (dual-mode, ADR-064):
	// a multi-field map, for example {access_key, secret_key}, XOR with
	// CredentialsRef. Keeper writes them to Vault
	// (secret/provider/<name>/credentials) and replaces them with an internal
	// ref; plaintext is not persisted.
	Credentials map[string]any
	// FQDNSuffix is an optional provider VM FQDN suffix (self-onboard option T,
	// ADR-017(h)). nil means self-onboard is unavailable for the provider.
	FQDNSuffix *string
	CallerAID  string
}

// Create inserts a new Provider.
//
// Dual-mode credentials (ADR-064): exactly one of Credentials(plaintext) /
// CredentialsRef. With Credentials, Keeper writes them to Vault and stores only
// ref; plaintext is not persisted. name/type/region/credentials_ref validation is
// in [Insert] + [resolveCredentials]. Returns:
//   - [ErrProviderAlreadyExists] on UNIQUE by name;
//   - [ErrValidation] wrapper for bad credentials input (XOR/format/disabled).
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

// resolveCredentials implements dual-mode credentials (ADR-064): returns the final
// credentials_ref (operator-provided or Keeper-written), write marker, and error.
// plaintext -> WriteMap(secret/provider/<name>/credentials).
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

	// Plaintext mode.
	if !s.acceptPlaintext || s.secretWriter == nil {
		return "", false, wrapValidation(ErrPlaintextDisabled)
	}
	// entity=<name> must be a safe path segment before writing to Vault.
	if !ValidName(in.Name) {
		return "", false, wrapValidation(fmt.Errorf("invalid name %q (must match %s)", in.Name, NamePattern))
	}
	ref, err := s.secretWriter.WriteMap(ctx, secretwrite.DomainProvider, in.Name, "credentials", in.Credentials)
	if err != nil {
		// secretwrite errors do not carry credential values; Vault failure is internal.
		return "", false, fmt.Errorf("provider: materialize credentials: %w", err)
	}
	return ref, true, nil
}

// Get reads one Provider by PK. [ErrProviderNotFound] when absent.
func (s *Service) Get(ctx context.Context, name string) (*Provider, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("provider: invalid name %q (must match %s)", name, NamePattern)
	}
	return SelectByName(ctx, s.pool, name)
}

// Delete removes a Provider by PK. [ErrProviderNotFound] when absent,
// [ErrProviderHasProfiles] with dependent Profiles (FK RESTRICT).
func (s *Service) Delete(ctx context.Context, name string) error {
	return Delete(ctx, s.pool, name)
}

// List returns a page of Providers and total count.
func (s *Service) List(ctx context.Context, offset, limit int) ([]*Provider, int, error) {
	return SelectAll(ctx, s.pool, offset, limit)
}
