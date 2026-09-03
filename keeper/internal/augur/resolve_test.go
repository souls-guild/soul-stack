package augur

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/subject"
)

// --- fakes for Resolve reader interfaces ------------------------------

type fakeOmens struct {
	byName map[string]*Omen
	err    error
}

func (f fakeOmens) OmenByName(_ context.Context, name string) (*Omen, error) {
	if f.err != nil {
		return nil, f.err
	}
	o, ok := f.byName[name]
	if !ok {
		return nil, ErrOmenNotFound
	}
	return o, nil
}

type fakeRites struct {
	rites []*Rite
	err   error
	// gotHost captures the subject handed to RitesBySubject — the check that the
	// grant lookup runs against the AUTHORITATIVE host facts (registry), not
	// against anything the caller sent.
	gotHost subject.Host
}

func (f *fakeRites) RitesBySubject(_ context.Context, host subject.Host) ([]*Rite, error) {
	f.gotHost = host
	if f.err != nil {
		return nil, f.err
	}
	return f.rites, nil
}

// fakeHosts stands in for the souls registry. Covens are the common case, so
// they keep the terse literal; memberBySID is only set by the tests that care
// about incarnation membership (NIM-280's fourth dimension).
type fakeHosts struct {
	bySID       map[string][]string
	memberBySID map[string][]subject.Incarnation
	err         error
}

func (f fakeHosts) HostBySID(_ context.Context, sid string) (subject.Host, error) {
	if f.err != nil {
		return subject.Host{}, f.err
	}
	covens, ok := f.bySID[sid]
	if !ok {
		return subject.Host{}, ErrSubjectUnknown
	}
	return subject.Host{SID: sid, Covens: covens, Member: f.memberBySID[sid]}, nil
}

func vaultOmen(name string) *Omen {
	return &Omen{ID: name, SourceType: SourceVault, Endpoint: "https://vault:8200", AuthRef: "vault:secret/keeper/augur/" + name}
}

func promOmen(name string) *Omen {
	return &Omen{ID: name, SourceType: SourcePrometheus, Endpoint: "https://prom:9090", AuthRef: "vault:secret/keeper/" + name}
}

func elkOmen(name string) *Omen {
	return &Omen{ID: name, SourceType: SourceELK, Endpoint: "https://elk:9200", AuthRef: "vault:secret/keeper/" + name}
}

func allowPaths(paths ...string) json.RawMessage {
	b, _ := json.Marshal(allowVault{Paths: paths})
	return b
}

func allowQueries(queries ...string) json.RawMessage {
	b, _ := json.Marshal(allowPrometheus{Queries: queries})
	return b
}

func allowIndices(indices ...string) json.RawMessage {
	b, _ := json.Marshal(allowELK{Indices: indices})
	return b
}

func covenRite(omen, coven string, paths ...string) *Rite {
	return &Rite{ID: 1, Omen: omen, Coven: []string{coven}, Allow: allowPaths(paths...)}
}

func sidRite(omen, sid string, paths ...string) *Rite {
	return &Rite{ID: 2, Omen: omen, SID: []string{sid}, Allow: allowPaths(paths...)}
}

// --- tests ------------------------------------------------------------

func TestResolve_OmenNotFound_Denied(t *testing.T) {
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{}},
		&fakeRites{},
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "no-such", "secret/keeper/x",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied, got allowed")
	}
}

func TestResolve_NoRite_Denied(t *testing.T) {
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		&fakeRites{rites: nil}, // no rites at all
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "vault-prod", "secret/keeper/x",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied (no rite), got allowed")
	}
}

func TestResolve_AllowExactMatch_Pass(t *testing.T) {
	rites := &fakeRites{rites: []*Rite{covenRite("vault-prod", "prod", "secret/keeper/db")}}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		rites,
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "vault-prod", "secret/keeper/db",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("expected allowed, got denied: %s", dec.Reason)
	}
	if dec.Query != "secret/keeper/db" {
		t.Errorf("normalized query = %q, want secret/keeper/db", dec.Query)
	}
	if dec.Omen == nil || dec.Omen.ID != "vault-prod" {
		t.Errorf("decision omen not set: %+v", dec.Omen)
	}
}

func TestResolve_QueryNotInAllow_Denied(t *testing.T) {
	rites := &fakeRites{rites: []*Rite{covenRite("vault-prod", "prod", "secret/keeper/db")}}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		rites,
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "vault-prod", "secret/keeper/other",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied (query not in allow), got allowed")
	}
}

// TestResolve_DoubleSlashNormalized_Denied verifies that `secret//db` does NOT match
// allow `secret/keeper/db` after normalization (it is a different path), and also
// cannot bypass allow through unnormalized comparison. Here we check that
// `secret//other` (normalized to `secret/other`) does not match allow
// `secret/keeper/db` → denied.
func TestResolve_DoubleSlashNormalized_Denied(t *testing.T) {
	rites := &fakeRites{rites: []*Rite{covenRite("vault-prod", "prod", "secret/keeper/db")}}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		rites,
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "vault-prod", "secret//keeper/other",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied, got allowed")
	}
}

// TestResolve_DoubleSlashMatchesAfterNormalize verifies that `secret//keeper/db`
// normalizes to `secret/keeper/db` and SHOULD match allow `secret/keeper/db`
// (normalization is symmetric on both sides — without it `secret//keeper/db` would
// not match allow, but ReadKV would reduce it to a forbidden path: a bypass).
func TestResolve_DoubleSlashMatchesAfterNormalize(t *testing.T) {
	rites := &fakeRites{rites: []*Rite{covenRite("vault-prod", "prod", "secret/keeper/db")}}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		rites,
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "vault-prod", "secret//keeper/db",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("expected allowed (normalized match), got denied: %s", dec.Reason)
	}
	if dec.Query != "secret/keeper/db" {
		t.Errorf("normalized query = %q, want secret/keeper/db", dec.Query)
	}
}

// TestResolve_DotDotSegment_Denied verifies that the `..` segment is rejected by
// normalization (scope bypass vector), resolve → denied.
func TestResolve_DotDotSegment_Denied(t *testing.T) {
	rites := &fakeRites{rites: []*Rite{covenRite("vault-prod", "prod", "secret/keeper/db")}}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		rites,
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "vault-prod", "secret/keeper/../other/db",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied (.. segment), got allowed")
	}
}

// TestResolve_SubjectFromRegistryNotPayload verifies that the subject facts the
// grant lookup runs against come from [HostReader] (the registry), not from the
// request. Since NIM-280 that is the WHOLE host — covens and incarnation
// membership alike — because a Rite may be written on any of four dimensions:
// handing the reader a partial host would silently deny grants written on the
// dimension that was dropped.
func TestResolve_SubjectFromRegistryNotPayload(t *testing.T) {
	rites := &fakeRites{rites: []*Rite{covenRite("vault-prod", "prod", "secret/keeper/db")}}
	registryCovens := []string{"prod", "eu-west"}
	registryMember := []subject.Incarnation{{Service: "redis", Name: "redis-prod"}}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		rites,
		fakeHosts{
			bySID:       map[string][]string{"host.example.com": registryCovens},
			memberBySID: map[string][]subject.Incarnation{"host.example.com": registryMember},
		},
		"host.example.com", "vault-prod", "secret/keeper/db",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("expected allowed, got denied: %s", dec.Reason)
	}
	got := rites.gotHost
	if got.SID != "host.example.com" {
		t.Errorf("RitesBySubject sid = %q, want host.example.com", got.SID)
	}
	if len(got.Covens) != 2 || got.Covens[0] != "prod" || got.Covens[1] != "eu-west" {
		t.Errorf("RitesBySubject covens = %v, want registry %v", got.Covens, registryCovens)
	}
	if len(got.Member) != 1 || got.Member[0].Service != "redis" || got.Member[0].Name != "redis-prod" {
		t.Errorf("RitesBySubject membership = %v, want registry %v", got.Member, registryMember)
	}
}

func TestResolve_SubjectUnknown_Denied(t *testing.T) {
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		&fakeRites{},
		fakeHosts{bySID: map[string][]string{}}, // SID not in registry
		"unknown.example.com", "vault-prod", "secret/keeper/db",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied (subject unknown), got allowed")
	}
}

// TestResolve_Prometheus_AllowExactMatch_Pass verifies that promQL ∈ allow.queries (exact)
// → allowed; Query carries raw promQL (without vault normalization).
func TestResolve_Prometheus_AllowExactMatch_Pass(t *testing.T) {
	r := &Rite{ID: 10, Omen: "prom-main", Coven: []string{"prod"}, Allow: allowQueries("up", "rate(http_requests_total[5m])")}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"prom-main": promOmen("prom-main")}},
		&fakeRites{rites: []*Rite{r}},
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "prom-main", "up",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("expected allowed, got denied: %s", dec.Reason)
	}
	if dec.Query != "up" {
		t.Errorf("Query = %q, want raw promQL up", dec.Query)
	}
	if dec.Omen == nil || dec.Omen.SourceType != SourcePrometheus {
		t.Errorf("decision omen not prometheus: %+v", dec.Omen)
	}
}

// TestResolve_Prometheus_NotInAllow_Denied verifies that promQL outside allow.queries → denied.
func TestResolve_Prometheus_NotInAllow_Denied(t *testing.T) {
	r := &Rite{ID: 11, Omen: "prom-main", Coven: []string{"prod"}, Allow: allowQueries("up")}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"prom-main": promOmen("prom-main")}},
		&fakeRites{rites: []*Rite{r}},
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "prom-main", "node_load1",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied (promQL not in allow), got allowed")
	}
}

// TestResolve_ELK_AllowExactMatch_Pass verifies that index ∈ allow.indices (exact) → allowed.
func TestResolve_ELK_AllowExactMatch_Pass(t *testing.T) {
	r := &Rite{ID: 20, Omen: "elk-logs", Coven: []string{"prod"}, Allow: allowIndices("logs-app", "logs-audit")}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"elk-logs": elkOmen("elk-logs")}},
		&fakeRites{rites: []*Rite{r}},
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "elk-logs", "logs-app",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("expected allowed, got denied: %s", dec.Reason)
	}
	if dec.Query != "logs-app" {
		t.Errorf("Query = %q, want raw index logs-app", dec.Query)
	}
}

// TestResolve_ELK_NotInAllow_Denied verifies that index outside allow.indices → denied.
func TestResolve_ELK_NotInAllow_Denied(t *testing.T) {
	r := &Rite{ID: 21, Omen: "elk-logs", Coven: []string{"prod"}, Allow: allowIndices("logs-app")}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"elk-logs": elkOmen("elk-logs")}},
		&fakeRites{rites: []*Rite{r}},
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "elk-logs", "secret-index",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied (index not in allow), got allowed")
	}
}

// TestResolve_Prometheus_EmptyQuery_Denied verifies that empty promQL is rejected.
func TestResolve_Prometheus_EmptyQuery_Denied(t *testing.T) {
	r := &Rite{ID: 12, Omen: "prom-main", Coven: []string{"prod"}, Allow: allowQueries("up")}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"prom-main": promOmen("prom-main")}},
		&fakeRites{rites: []*Rite{r}},
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "prom-main", "",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied (empty query), got allowed")
	}
}

func TestResolve_DelegateTrue_Skipped_Denied(t *testing.T) {
	r := covenRite("vault-prod", "prod", "secret/keeper/db")
	r.Delegate = true
	rites := &fakeRites{rites: []*Rite{r}}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		rites,
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "vault-prod", "secret/keeper/db",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied (delegate=true skipped in slice B), got allowed")
	}
}

func TestResolve_SIDRiteMatch_Pass(t *testing.T) {
	rites := &fakeRites{rites: []*Rite{sidRite("vault-prod", "host.example.com", "secret/keeper/db")}}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		rites,
		fakeHosts{bySID: map[string][]string{"host.example.com": nil}},
		"host.example.com", "vault-prod", "secret/keeper/db",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("expected allowed (sid-rite), got denied: %s", dec.Reason)
	}
}

// TestResolve_RiteForOtherOmen_Denied verifies that a Rite for a different Omen must not
// authorize a request (RitesBySubject may return Rites for all Omens of a subject;
// filtering by omen in resolve is mandatory).
func TestResolve_RiteForOtherOmen_Denied(t *testing.T) {
	rites := &fakeRites{rites: []*Rite{covenRite("vault-other", "prod", "secret/keeper/db")}}
	dec, err := Resolve(context.Background(),
		fakeOmens{byName: map[string]*Omen{"vault-prod": vaultOmen("vault-prod")}},
		rites,
		fakeHosts{bySID: map[string][]string{"host.example.com": {"prod"}}},
		"host.example.com", "vault-prod", "secret/keeper/db",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("expected denied (rite for other omen), got allowed")
	}
}

func TestResolve_ReaderError_Propagated(t *testing.T) {
	boom := errors.New("pg down")
	_, err := Resolve(context.Background(),
		fakeOmens{err: boom},
		&fakeRites{},
		fakeHosts{bySID: map[string][]string{}},
		"host.example.com", "vault-prod", "secret/keeper/db",
	)
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped reader error, got %v", err)
	}
}
