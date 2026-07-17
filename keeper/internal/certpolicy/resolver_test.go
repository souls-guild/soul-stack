package certpolicy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeDB — IncarnationReader-stub: QueryRow returns the given row (Exec/Query are
// unused by SelectByName, but are needed for the method set).
type fakeDB struct{ row pgx.Row }

func (f *fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeDB) QueryRow(context.Context, string, ...any) pgx.Row { return f.row }
func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("certpolicy_test: Query unused")
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// staticRow — a pgx.Row stub handing out given values in scanIncarnation order.
type staticRow struct{ values []any }

func (r staticRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return errors.New("staticRow: len mismatch")
	}
	for i, d := range dest {
		assign(d, r.values[i])
	}
	return nil
}

func assign(dest, src any) {
	switch d := dest.(type) {
	case *string:
		*d = src.(string)
	case *int:
		*d = src.(int)
	case *time.Time:
		*d = src.(time.Time)
	case **string:
		if src == nil {
			*d = nil
		} else {
			s := src.(string)
			*d = &s
		}
	case *[]byte:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]byte)
		}
	case *[]string:
		if src == nil {
			*d = nil
		} else {
			*d = src.([]string)
		}
	case **time.Time:
		if src == nil {
			*d = nil
		} else {
			t := src.(time.Time)
			*d = &t
		}
	default:
		panic("certpolicy_test.assign: unsupported dest type")
	}
}

// incRow builds a staticRow with the 17 scanIncarnation columns; only
// service/service_version matter, the rest are valid placeholders.
func incRow(service, serviceVersion string) staticRow {
	now := time.Now()
	return staticRow{values: []any{
		"redis-prod", service, serviceVersion, 1,
		[]byte("{}"), []byte("{}"), "ready",
		[]byte(nil), any(nil), now, now, []string(nil),
		[]byte("{}"),
		any(nil), []byte(nil),
		any(nil), // created_scenario
		any(nil), // applying_apply_id
	}}
}

type fakeServices struct {
	ref        artifact.ServiceRef
	ok         bool
	gotService string
}

func (f *fakeServices) Resolve(service string) (artifact.ServiceRef, bool) {
	f.gotService = service
	return f.ref, f.ok
}

type fakeLister struct {
	info                    *artifact.CertPolicyInfo
	err                     error
	calls                   int
	gotName, gotGit, gotRef string
}

func (f *fakeLister) ListCertPolicy(_ context.Context, name, gitURL, ref string) (*artifact.CertPolicyInfo, error) {
	f.calls++
	f.gotName, f.gotGit, f.gotRef = name, gitURL, ref
	return f.info, f.err
}

func TestResolve_EnabledSection(t *testing.T) {
	db := &fakeDB{row: incRow("redis", "v9-pinned")}
	services := &fakeServices{ref: artifact.ServiceRef{Name: "redis", Git: "git://redis", Ref: "registry-ref"}, ok: true}
	lister := &fakeLister{info: &artifact.CertPolicyInfo{
		Rotation: &config.CertificateRotationConfig{
			Enable: true, Scenario: "rotate_tls", PKIRole: "redis-server", Threshold: "30d",
		},
		Scenarios: []string{"create", "rotate_tls"},
	}}
	r := NewResolver(db, services, lister)

	p, err := r.Resolve(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !p.Present || !p.Enabled {
		t.Errorf("Present/Enabled = %v/%v, want true/true", p.Present, p.Enabled)
	}
	if p.Service != "redis" {
		t.Errorf("Service = %q", p.Service)
	}
	if p.Scenario != "rotate_tls" || p.PKIRole != "redis-server" {
		t.Errorf("Scenario/PKIRole = %q/%q", p.Scenario, p.PKIRole)
	}
	if len(p.KnownScenarios) != 2 || p.KnownScenarios[0] != "create" || p.KnownScenarios[1] != "rotate_tls" {
		t.Errorf("KnownScenarios = %v", p.KnownScenarios)
	}
	if p.Threshold != 30*24*time.Hour {
		t.Errorf("Threshold = %v, want 720h", p.Threshold)
	}
	// ★ pinning: the lister is called with ref = inc.ServiceVersion, NOT the registry ref.
	if lister.gotRef != "v9-pinned" {
		t.Errorf("lister ref = %q, want pinned \"v9-pinned\" (NOT registry-ref)", lister.gotRef)
	}
	if lister.gotName != "redis" || lister.gotGit != "git://redis" {
		t.Errorf("lister name/git = %q/%q", lister.gotName, lister.gotGit)
	}
}

func TestResolve_NoRotationSection(t *testing.T) {
	db := &fakeDB{row: incRow("redis", "v1")}
	services := &fakeServices{ref: artifact.ServiceRef{Name: "redis", Git: "g"}, ok: true}
	lister := &fakeLister{info: &artifact.CertPolicyInfo{Rotation: nil, Scenarios: []string{"create"}}}
	r := NewResolver(db, services, lister)

	p, err := r.Resolve(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("Resolve: %v (nil section — not an error)", err)
	}
	if p.Present || p.Enabled {
		t.Errorf("Present/Enabled = %v/%v, want false/false", p.Present, p.Enabled)
	}
	if len(p.KnownScenarios) != 1 {
		t.Errorf("KnownScenarios = %v (must be populated even without the section)", p.KnownScenarios)
	}
}

func TestResolve_SectionDisabled(t *testing.T) {
	db := &fakeDB{row: incRow("redis", "v1")}
	services := &fakeServices{ref: artifact.ServiceRef{Name: "redis"}, ok: true}
	lister := &fakeLister{info: &artifact.CertPolicyInfo{
		Rotation: &config.CertificateRotationConfig{Enable: false, Scenario: "rotate_tls", PKIRole: "r"},
	}}
	r := NewResolver(db, services, lister)

	p, err := r.Resolve(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !p.Present {
		t.Errorf("Present = false, want true (section exists)")
	}
	if p.Enabled {
		t.Errorf("Enabled = true, want false (enable:false)")
	}
}

func TestResolve_IncarnationNotFound(t *testing.T) {
	db := &fakeDB{row: errRow{err: pgx.ErrNoRows}}
	services := &fakeServices{ok: true}
	lister := &fakeLister{}
	r := NewResolver(db, services, lister)

	if _, err := r.Resolve(context.Background(), "ghost"); err == nil {
		t.Fatal("expected error on missing incarnation")
	}
	if services.gotService != "" {
		t.Errorf("services.Resolve should not be called on a SelectByName error")
	}
	if lister.calls != 0 {
		t.Errorf("lister should not be called on a SelectByName error")
	}
}

func TestResolve_ServiceNotRegistered(t *testing.T) {
	db := &fakeDB{row: incRow("mystery", "v1")}
	services := &fakeServices{ok: false}
	lister := &fakeLister{}
	r := NewResolver(db, services, lister)

	if _, err := r.Resolve(context.Background(), "redis-prod"); err == nil {
		t.Fatal("expected error when service not registered")
	}
	if lister.calls != 0 {
		t.Errorf("lister should not be called when services.Resolve returns !ok")
	}
}

func TestResolve_ListerError(t *testing.T) {
	db := &fakeDB{row: incRow("redis", "v1")}
	services := &fakeServices{ref: artifact.ServiceRef{Name: "redis"}, ok: true}
	lister := &fakeLister{err: errors.New("git clone failed (transient)")}
	r := NewResolver(db, services, lister)

	if _, err := r.Resolve(context.Background(), "redis-prod"); err == nil {
		t.Fatal("expected transient error from lister")
	}
}
