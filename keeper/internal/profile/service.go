package profile

import (
	"context"
	"errors"
	"fmt"
)

// Service contains CRUD business logic for the `profiles` registry (ADR-017,
// docs/keeper/cloud.md). It is the single source of truth for HTTP handlers and
// MCP tool handlers, symmetric to provider.Service / pushprovider.Service.
//
// Profile is a VM spec on top of a Provider: `Params` (jsonb, freeform VM spec) +
// optional `CloudInit`. Params validation against CloudDriver.Schema belongs to
// the scenario layer (Cloud.CRUD.b), not CRUD. DB checks the `provider` FK to an
// existing Provider ([ErrProviderNotFound] -> 422).
type Service struct {
	pool ExecQueryRower
}

// NewService builds a service. pool is required.
func NewService(pool ExecQueryRower) (*Service, error) {
	if pool == nil {
		return nil, errors.New("profile: NewService: pool is nil")
	}
	return &Service{pool: pool}, nil
}

// CreateInput contains [Service.Create] parameters.
type CreateInput struct {
	Name string
	// Label is the optional display caption ([ADR-0085]): free text, set here at
	// registration and changed afterwards by [Service.SetLabel]. nil/blank stores
	// NULL and the consumer shows Name.
	Label     *string
	Provider  string
	Params    map[string]any
	CloudInit *string
	CallerAID string
}

// Create inserts a new Profile.
//
// name/provider validation is done in [Insert]. Returns:
//   - [ErrProfileAlreadyExists] on UNIQUE by name;
//   - [ErrProviderNotFound] on FK violation (reference to a missing Provider) ->
//     handler maps it to 422;
//   - domain validation error (invalid name/provider).
func (s *Service) Create(ctx context.Context, in CreateInput) (*Profile, error) {
	var createdBy *string
	if in.CallerAID != "" {
		aid := in.CallerAID
		createdBy = &aid
	}
	p := &Profile{
		Name:         in.Name,
		Label:        in.Label,
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

// Get reads one Profile by PK. [ErrProfileNotFound] when absent.
func (s *Service) Get(ctx context.Context, name string) (*Profile, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("profile: invalid name %q (must match %s)", name, NamePattern)
	}
	return SelectByName(ctx, s.pool, name)
}

// SetLabel replaces the display caption of one Profile and returns the row as it
// now reads ([ADR-0085], permission profile.label-set, audit
// profile.label_changed).
//
// This is the registry's ONLY mutation: a Profile's VM spec stays immutable
// (changing parameters means delete+create), and a caption is the one field for
// which that argument does not apply, because nothing reads it.
//
// [ErrProfileNotFound] when the row is absent.
func (s *Service) SetLabel(ctx context.Context, name string, label *string) (*Profile, error) {
	if err := UpdateLabel(ctx, s.pool, name, label); err != nil {
		return nil, err
	}
	return SelectByName(ctx, s.pool, name)
}

// Delete removes a Profile by PK. [ErrProfileNotFound] when absent.
func (s *Service) Delete(ctx context.Context, name string) error {
	return Delete(ctx, s.pool, name)
}

// List returns a page of Profiles and total count. Non-empty providerName filters
// by Provider (SelectByProvider).
func (s *Service) List(ctx context.Context, providerName string, offset, limit int) ([]*Profile, int, error) {
	if providerName != "" {
		return SelectByProvider(ctx, s.pool, providerName, offset, limit)
	}
	return SelectAll(ctx, s.pool, offset, limit)
}
