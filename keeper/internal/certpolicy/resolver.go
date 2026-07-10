// Package certpolicy резолвит эффективную политику авто-ротации TLS-сертов
// инкарнации (NIM-99): читает incarnation → её пиновый снапшот сервиса → секцию
// `certificate_rotation:` манифеста. Общий вход для reaper (кого ротировать) и
// UI/coremod (виден ли и включён ли triangle-ротатор у сервиса).
package certpolicy

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Policy — эффективная cert-rotation-политика инкарнации. Present — есть ли
// секция `certificate_rotation:` в манифесте; Enabled — включена ли (enable:true).
// KnownScenarios — имена scenario/ снапшота (для валидации Scenario резолвером/UI).
type Policy struct {
	Service        string
	Present        bool
	Enabled        bool
	Scenario       string
	PKIRole        string
	Threshold      time.Duration
	KnownScenarios []string
}

// IncarnationReader — поверхность чтения incarnation для [incarnation.SelectByName].
// Совпадает с [incarnation.ExecQueryRower] (тот требует Exec/QueryRow/Query, а не
// один QueryRow); production даёт pgxpool.Pool, тесты — fake.
type IncarnationReader interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ServiceRefResolver резолвит git-координаты сервиса по имени.
// [scenario.ServiceRegistry] её удовлетворяет.
type ServiceRefResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// PolicyLister листингует cert-policy снапшота сервиса.
// [serviceregistry.CertPolicyCache] её удовлетворяет.
type PolicyLister interface {
	ListCertPolicy(ctx context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error)
}

// Resolver собирает [Policy] из БД-инкарнации + снапшота её пиновой версии.
type Resolver struct {
	db       IncarnationReader
	services ServiceRefResolver
	lister   PolicyLister
}

// NewResolver конструирует резолвер поверх БД, реестра сервисов и cert-policy-кеша.
func NewResolver(db IncarnationReader, services ServiceRefResolver, lister PolicyLister) *Resolver {
	return &Resolver{db: db, services: services, lister: lister}
}

// Resolve возвращает cert-rotation-политику инкарнации incarnationName.
//
// SelectByName → services.Resolve → ★ пин ref на inc.ServiceVersion (не
// реестровый ref) → ListCertPolicy. Нет секции (Rotation==nil) → Present/Enabled
// false без ошибки. Ошибка парса Threshold — проглатывается в 0 (порог пока
// информативен, не критичен).
func (r *Resolver) Resolve(ctx context.Context, incarnationName string) (Policy, error) {
	inc, err := incarnation.SelectByName(ctx, r.db, incarnationName)
	if err != nil {
		return Policy{}, fmt.Errorf("certpolicy: load incarnation %q: %w", incarnationName, err)
	}

	ref, ok := r.services.Resolve(inc.Service)
	if !ok {
		return Policy{}, fmt.Errorf("certpolicy: service %q not registered", inc.Service)
	}
	ref.Ref = inc.ServiceVersion // ★ пиновая версия инкарнации, НЕ реестровый ref

	info, err := r.lister.ListCertPolicy(ctx, inc.Service, ref.Git, ref.Ref)
	if err != nil {
		return Policy{}, fmt.Errorf("certpolicy: list cert policy for %q: %w", inc.Service, err)
	}

	p := Policy{Service: inc.Service, KnownScenarios: info.Scenarios}
	if info.Rotation == nil {
		return p, nil
	}
	p.Present = true
	p.Enabled = info.Rotation.Enable
	p.Scenario = info.Rotation.Scenario
	p.PKIRole = info.Rotation.PKIRole
	if info.Rotation.Threshold != "" {
		if d, perr := config.ParseDuration(info.Rotation.Threshold); perr == nil {
			p.Threshold = d
		}
	}
	return p, nil
}
