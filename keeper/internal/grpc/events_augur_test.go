package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/subject"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// augurFakeDB — implements augurDB (augur.ExecQueryRower + soul.ExecQueryRower).
// Routes by SQL: SELECT ... FROM omens → an omen row; FROM souls → the subject
// facts ([subject.LoadHost]); FROM rites → a rite set.
//
// A label still exists only where an operator attached it (NIM-281): `hostCovens`
// is what is on the HOST, `member[].Covens` what is on the INCARNATION. The two
// are kept apart in the fixture so a guard can tell them apart — belonging to an
// incarnation lends the host nothing, while a label on the incarnation reaches
// its members (NIM-280), and those are different sentences.
type augurFakeDB struct {
	omenRow  func() pgx.Row // SelectOmenByName
	riteRows func() (pgx.Rows, error)

	// hostCovens / hostTraits — labels an operator attached to THIS host.
	hostCovens []string
	hostTraits []byte
	// member — the incarnations the host belongs to, carrying their own labels.
	member []subject.Incarnation
	// hostUnknown — the SID is not in the souls registry.
	hostUnknown bool

	// gotHostSID — the SID the subject load actually ran for.
	gotHostSID string
	// gotHost — the subject the resolve matched Rites against, rebuilt from the
	// query arguments (not from the fixture): a resolve that dropped a dimension
	// is then visibly missing it here, and the Rites on that dimension stop
	// matching instead of passing on a fixture's goodwill.
	gotHost subject.Host
}

func (f *augurFakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("augurFakeDB: Exec not used")
}

func (f *augurFakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "FROM omens") && f.omenRow != nil {
		return f.omenRow()
	}
	return augurErrRow{err: pgx.ErrNoRows}
}

func (f *augurFakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM souls"):
		f.gotHostSID, _ = args[0].(string)
		return f.hostRows(), nil
	case strings.Contains(sql, "FROM rites") && f.riteRows != nil:
		rows, err := f.riteRows()
		if err != nil {
			return nil, err
		}
		f.gotHost = augurHostFromArgs(args)
		return filterAugurRitesBySubject(rows, f.gotHost), nil
	}
	return &augurEmptyRows{}, nil
}

// hostRows answers [subject.LoadHost]: s.coven, s.traits, i.service, i.name,
// i.covens, i.traits — one row per membership, the host's own labels repeated in
// each, and a single NULL-incarnation row when the host belongs to nothing.
func (f *augurFakeDB) hostRows() pgx.Rows {
	if f.hostUnknown {
		return &augurRiteRows{}
	}
	if len(f.member) == 0 {
		return &augurRiteRows{rows: [][]any{{f.hostCovens, f.hostTraits, nil, nil, nil, nil}}}
	}
	rows := make([][]any, 0, len(f.member))
	for _, inc := range f.member {
		traits, _ := json.Marshal(inc.Traits)
		if inc.Traits == nil {
			traits = nil
		}
		rows = append(rows, []any{f.hostCovens, f.hostTraits, inc.Service, inc.Name, inc.Covens, traits})
	}
	return &augurRiteRows{rows: rows}
}

// augurHostFromArgs rebuilds the subject from SelectRitesBySubject's flattened
// arguments (sid, covens, incarnation services, incarnation names, trait pairs).
// Rebuilding from the ARGUMENTS rather than from the fixture is the point: it is
// what the query would actually have filtered on.
func augurHostFromArgs(args []any) subject.Host {
	h := subject.Host{}
	h.SID, _ = args[0].(string)
	h.Covens, _ = args[1].([]string)
	svcs, _ := args[2].([]string)
	names, _ := args[3].([]string)
	for i, name := range names {
		if i < len(svcs) {
			h.Member = append(h.Member, subject.Incarnation{Service: svcs[i], Name: name})
		}
	}
	pairs, _ := args[4].([]string)
	byKey := map[string][]any{}
	for _, p := range pairs {
		if k, v, ok := strings.Cut(p, "="); ok {
			byKey[k] = append(byKey[k], v)
		}
	}
	for k, vs := range byKey {
		if h.Traits == nil {
			h.Traits = map[string]any{}
		}
		// One value stays scalar, several become a list — the two shapes the
		// matcher distinguishes (a list trait matches by membership).
		if len(vs) == 1 {
			h.Traits[k] = vs[0]
			continue
		}
		h.Traits[k] = vs
	}
	return h
}

// filterAugurRitesBySubject applies the REAL matcher ([subject.Selector.Matches])
// to the fixture rows, rather than a second copy of the predicate written here.
// Without any filter the fake would hand back every Rite regardless of the
// subject the resolve computed, and no unit test could tell a correct subject
// resolution from a broken one — precisely how NIM-249 stayed invisible. Using
// the real matcher also means this fake cannot drift away from the rule it stands
// in for.
func filterAugurRitesBySubject(rows pgx.Rows, host subject.Host) pgx.Rows {
	src, ok := rows.(*augurRiteRows)
	if !ok {
		return rows
	}
	kept := make([][]any, 0, len(src.rows))
	for _, r := range src.rows {
		if augurRiteSelector(r).Matches(host) {
			kept = append(kept, r)
		}
	}
	return &augurRiteRows{rows: kept}
}

// augurRiteSelector reads the subject out of a fixture row, in riteColumns order:
// id, omen, sid, service, incarnation, coven, trait_key, trait_value, …
func augurRiteSelector(r []any) subject.Selector {
	str := func(v any) string {
		s, _ := v.(string)
		return s
	}
	sids, _ := r[2].([]string)
	covens, _ := r[5].([]string)
	return subject.Selector{
		SIDs:        sids,
		Service:     str(r[3]),
		Incarnation: str(r[4]),
		Covens:      covens,
		TraitKey:    str(r[6]),
		TraitValue:  str(r[7]),
	}
}

type augurErrRow struct{ err error }

func (r augurErrRow) Scan(_ ...any) error { return r.err }

// augurOmenRowVals — values in omenColumns order:
// name, source_type, endpoint, auth_ref, created_by_aid, created_at.
type augurValRow struct{ vals []any }

func (r augurValRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return errors.New("augurValRow: len mismatch")
	}
	for i, d := range dest {
		augurAssign(d, r.vals[i])
	}
	return nil
}

func augurAssign(dest, src any) {
	switch d := dest.(type) {
	case *string:
		*d = src.(string)
	case *int64:
		*d = src.(int64)
	case *bool:
		*d = src.(bool)
	case *time.Time:
		*d = src.(time.Time)
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
	case **string:
		if src == nil {
			*d = nil
		} else {
			s := src.(string)
			*d = &s
		}
	case **int:
		if src == nil {
			*d = nil
		} else {
			n := src.(int)
			*d = &n
		}
	case **time.Time:
		if src == nil {
			*d = nil
		} else {
			tm := src.(time.Time)
			*d = &tm
		}
	default:
		panic("augurAssign: unsupported dest")
	}
}

type augurEmptyRows struct{}

func (r *augurEmptyRows) Next() bool                                   { return false }
func (r *augurEmptyRows) Scan(...any) error                            { return nil }
func (r *augurEmptyRows) Err() error                                   { return nil }
func (r *augurEmptyRows) Close()                                       {}
func (r *augurEmptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *augurEmptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *augurEmptyRows) Values() ([]any, error)                       { return nil, nil }
func (r *augurEmptyRows) RawValues() [][]byte                          { return nil }
func (r *augurEmptyRows) Conn() *pgx.Conn                              { return nil }

// augurRiteRows — yields a set of rite rows in riteColumns order:
// id, omen, coven, sid, allow, delegate, token_ttl, token_num_uses,
// created_by_aid, created_at.
type augurRiteRows struct {
	rows [][]any
	idx  int
}

func (r *augurRiteRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}
func (r *augurRiteRows) Scan(dest ...any) error {
	return augurValRow{vals: r.rows[r.idx-1]}.Scan(dest...)
}
func (r *augurRiteRows) Err() error                                   { return nil }
func (r *augurRiteRows) Close()                                       {}
func (r *augurRiteRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *augurRiteRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *augurRiteRows) Values() ([]any, error)                       { return nil, nil }
func (r *augurRiteRows) RawValues() [][]byte                          { return nil }
func (r *augurRiteRows) Conn() *pgx.Conn                              { return nil }

var augurTestNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func augurAllowPaths(paths ...string) []byte {
	b, _ := json.Marshal(map[string][]string{"paths": paths})
	return b
}

// augurOmenRowVault — a vault-type omen row. nil = NULL column.
func augurOmenRowVault(name string) pgx.Row {
	return augurValRow{vals: []any{name, "vault", "https://vault:8200", "vault:secret/keeper/augur/" + name, nil, augurTestNow, nil}}
}

// augurRiteRow — a rite row in riteColumns order (id, omen, sid, service,
// incarnation, coven, trait_key, trait_value, allow, delegate, token_ttl,
// token_num_uses, created_by_aid, created_at). sel supplies the subject; every
// dimension it does not carry stays NULL.
func augurRiteRowAllow(id int, omen string, sel subject.Selector, allow []byte) []any {
	nilIfEmpty := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	return []any{
		int64(id), omen,
		sel.SIDs, nilIfEmpty(sel.Service), nilIfEmpty(sel.Incarnation), sel.Covens,
		nilIfEmpty(sel.TraitKey), nilIfEmpty(sel.TraitValue),
		allow, false, nil, nil, nil, augurTestNow,
	}
}

func augurRiteRow(id int, omen string, sel subject.Selector, paths ...string) []any {
	return augurRiteRowAllow(id, omen, sel, augurAllowPaths(paths...))
}

// augurCovenRite — the terse spelling of the dimension most of these cases use.
func augurCovenRite(id int, omen, coven string, paths ...string) []any {
	return augurRiteRow(id, omen, subject.Selector{Covens: []string{coven}}, paths...)
}

// stubKV — fake augur.KVReader.
type stubKV struct {
	data    map[string]any
	err     error
	gotPath string
}

func (s *stubKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	s.gotPath = path
	if s.err != nil {
		return nil, s.err
	}
	return s.data, nil
}

// newAugurHandler assembles a handler with Augur deps + StreamManager + Outbound
// and returns outCh, from which the sent AugurReply is read (the same stream).
func newAugurHandler(t *testing.T, db augurDB, kv augur.KVReader, aw audit.Writer, sid string) (*eventStreamHandler, <-chan *keeperv1.FromKeeper) {
	t.Helper()
	mgr := NewStreamManager(discardLogger(t))
	outCh := mgr.Register(sid)
	out, err := NewOutbound(OutboundDeps{Manager: mgr, AuditWriter: aw, Logger: discardLogger(t)})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		Manager:     mgr,
		Augur: &AugurDeps{
			DB:          db,
			Vault:       kv,
			Egress:      stubDoer{},
			AuditWriter: aw,
			Outbound:    out,
		},
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t)), outCh
}

// stubDoer — a stub augur.HTTPDoer for vault round-trip tests (prom/elk aren't
// touched). prom/elk-specific tests pass their own doer via
// newAugurHandlerEgress.
type stubDoer struct {
	resp func() (*http.Response, error)
}

func (d stubDoer) Do(*http.Request) (*http.Response, error) {
	if d.resp != nil {
		return d.resp()
	}
	return nil, errors.New("stubDoer: not configured")
}

// newAugurHandlerEgress — like newAugurHandler, but with an explicit egress
// doer and an optional concurrency limit (0 → default). For prom/elk and
// semaphore tests.
func newAugurHandlerEgress(t *testing.T, db augurDB, kv augur.KVReader, doer augur.HTTPDoer, aw audit.Writer, sid string, concurrency int) (*eventStreamHandler, <-chan *keeperv1.FromKeeper) {
	t.Helper()
	mgr := NewStreamManager(discardLogger(t))
	outCh := mgr.Register(sid)
	out, err := NewOutbound(OutboundDeps{Manager: mgr, AuditWriter: aw, Logger: discardLogger(t)})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	deps := EventStreamDeps{
		SeedDB:           &fakeSeedDB{},
		AuditWriter:      aw,
		KID:              "kid-test",
		Manager:          mgr,
		AugurConcurrency: concurrency,
		Augur: &AugurDeps{
			DB:          db,
			Vault:       kv,
			Egress:      doer,
			AuditWriter: aw,
			Outbound:    out,
		},
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t)), outCh
}

func recvReply(t *testing.T, outCh <-chan *keeperv1.FromKeeper) *keeperv1.AugurReply {
	t.Helper()
	select {
	case msg := <-outCh:
		reply := msg.GetAugurReply()
		if reply == nil {
			t.Fatalf("FromKeeper is not AugurReply: %T", msg.GetPayload())
		}
		return reply
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for AugurReply")
		return nil
	}
}

func TestAugur_RoundTrip_OK(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowVault("vault-prod") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurCovenRite(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"username": "svc", "password": "s3cr3t"}}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-1", ApplyId: "apply-1", OmenName: "vault-prod", Query: "secret/keeper/db",
	})

	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK (error=%q)", reply.GetStatus(), reply.GetError())
	}
	if reply.GetRequestId() != "req-1" {
		t.Errorf("request_id echo = %q, want req-1", reply.GetRequestId())
	}
	inline := reply.GetInlineData()
	if inline == nil {
		t.Fatalf("inline_data nil")
	}
	if inline.GetFields()["username"].GetStringValue() != "svc" {
		t.Errorf("inline username missing")
	}
	if kv.gotPath != "secret/keeper/db" {
		t.Errorf("ReadKV path = %q, want secret/keeper/db", kv.gotPath)
	}

	// audit: augur.fetch_brokered, without the secret value.
	evs := aw.snapshot()
	if len(evs) != 1 {
		t.Fatalf("audit events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.EventType != audit.EventAugurFetchBrokered {
		t.Errorf("event_type = %q, want augur.fetch_brokered", ev.EventType)
	}
	if ev.Source != audit.SourceSoulGRPC {
		t.Errorf("source = %q, want soul_grpc", ev.Source)
	}
	if ev.CorrelationID != "apply-1" {
		t.Errorf("correlation_id = %q, want apply-1", ev.CorrelationID)
	}
	assertNoSecretInPayload(t, ev.Payload)
	if ev.Payload["omen"] != "vault-prod" || ev.Payload["query"] != "secret/keeper/db" {
		t.Errorf("payload omen/query missing: %v", ev.Payload)
	}
}

func TestAugur_RoundTrip_Denied_NoRite(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowVault("vault-prod") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: nil}, nil // no Rites
		},
	}
	kv := &stubKV{}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-2", ApplyId: "apply-2", OmenName: "vault-prod", Query: "secret/keeper/db",
	})

	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("status = %v, want DENIED", reply.GetStatus())
	}
	if reply.GetInlineData() != nil {
		t.Errorf("denied reply must not carry inline_data")
	}
	if kv.gotPath != "" {
		t.Errorf("ReadKV must NOT be called on denied, got path %q", kv.gotPath)
	}
	evs := aw.snapshot()
	if len(evs) != 1 || evs[0].EventType != audit.EventAugurAccessDenied {
		t.Fatalf("expected one augur.access_denied event, got %+v", evs)
	}
}

// --- subject resolution reads the AUTHORITATIVE host facts (NIM-281 / NIM-280) ---

// augurMemberDB — an Omen + one Rite carrying `riteSel`, and a host whose own
// covens are `own` and whose memberships are `member`.
func augurMemberDB(riteSel subject.Selector, own []string, member ...subject.Incarnation) *augurFakeDB {
	return &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowVault("vault-prod") },
		hostCovens: own,
		member:     member,
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{
				augurRiteRow(1, "vault-prod", riteSel, "secret/keeper/db"),
			}}, nil
		},
	}
}

// augurAsk runs one request against the fixture and returns the reply.
func augurAsk(t *testing.T, db *augurFakeDB, reqID string) *keeperv1.AugurReply {
	t.Helper()
	const sid = "host.example.com"
	kv := &stubKV{data: map[string]any{"password": "s3cr3t"}}
	h, outCh := newAugurHandler(t, db, kv, &recordingAudit{}, sid)
	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: reqID, OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK && kv.gotPath != "" {
		t.Errorf("ReadKV must NOT be called on a non-OK reply, got path %q", kv.gotPath)
	}
	return reply
}

// TestAugur_OwnCovenTagAuthorizesRite — the positive baseline: a Rite scoped to
// `redis-prod` authorizes a host tagged `redis-prod`.
func TestAugur_OwnCovenTagAuthorizesRite(t *testing.T) {
	db := augurMemberDB(subject.Selector{Covens: []string{"redis-prod"}}, []string{"redis-prod"})

	reply := augurAsk(t, db, "req-m1")
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v (%q), want OK — the host carries the Rite's coven",
			reply.GetStatus(), reply.GetError())
	}
	if len(db.gotHost.Covens) == 0 {
		t.Fatal("resolve matched Rites against an empty coven set")
	}
}

// TestAugur_MembershipAloneDoesNotAuthorizeCovenRite — GUARD (NIM-281): a host
// bound to incarnation `redis-prod` that carries NO label, and not tagged
// `redis-prod` itself, does not match a Rite scoped to `redis-prod`. Belonging
// attaches nothing; what reaches a member is a label an operator actually put on
// the incarnation, and here there is none.
//
// This is a secret-reading path, so the direction matters: if belonging alone
// ever started counting, every host bound to an incarnation would silently gain
// that incarnation's Rites — a widening of who can read which Vault path,
// decided by a bind operation nobody read as a grant.
func TestAugur_MembershipAloneDoesNotAuthorizeCovenRite(t *testing.T) {
	db := augurMemberDB(
		subject.Selector{Covens: []string{"redis-prod"}},
		[]string{"db"}, // the host's own tag
		subject.Incarnation{Service: "redis", Name: "redis-prod"}, // unlabelled
	)

	reply := augurAsk(t, db, "req-m2")
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("status = %v, want DENIED (belonging to an incarnation is not a tag)", reply.GetStatus())
	}
}

// TestAugur_IncarnationCovenReachesMembers — NIM-280, and the declared reversal
// of the NIM-281-era rule that a label on an incarnation reached nobody. A coven
// an operator put ON THE INCARNATION reaches its members: the label is still only
// where it was attached, but a subject reads both levels.
//
// ⚠ The consequence is deliberate and worth stating on a secret-reading path:
// tagging an incarnation `cache` widens every existing `coven: [cache]` Rite to
// that incarnation's hosts, with no Rite edited. The label namespace is shared,
// so a tag is a grant-shaped act.
func TestAugur_IncarnationCovenReachesMembers(t *testing.T) {
	db := augurMemberDB(
		subject.Selector{Covens: []string{"cache"}},
		[]string{"db"}, // the host itself is NOT tagged `cache`
		subject.Incarnation{Service: "redis", Name: "redis-prod", Covens: []string{"cache"}},
	)

	reply := augurAsk(t, db, "req-m3")
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v (%q), want OK — a coven on the incarnation reaches its members",
			reply.GetStatus(), reply.GetError())
	}
}

// TestAugur_IncarnationSubjectReachesMembers — NIM-280's own dimension: a Rite
// addressed to `redis.redis-prod` covers the hosts that are MEMBERS of it, and
// membership is the only thing it reads.
func TestAugur_IncarnationSubjectReachesMembers(t *testing.T) {
	sel := subject.Selector{Service: "redis", Incarnation: "redis-prod"}

	member := augurMemberDB(sel, []string{"db"},
		subject.Incarnation{Service: "redis", Name: "redis-prod"})
	if reply := augurAsk(t, member, "req-m4"); reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v (%q), want OK — the host is a member",
			reply.GetStatus(), reply.GetError())
	}

	// The name is unique only within its service, so the service half is part of
	// the address: the same name elsewhere is a different incarnation.
	other := augurMemberDB(sel, []string{"db"},
		subject.Incarnation{Service: "valkey", Name: "redis-prod"})
	if reply := augurAsk(t, other, "req-m5"); reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("status = %v, want DENIED — same name, different service", reply.GetStatus())
	}

	// A host merely TAGGED with a coven spelled like the incarnation is not a
	// member: that conflation is the escalation NIM-281 closed.
	lookalike := augurMemberDB(sel, []string{"redis-prod"})
	if reply := augurAsk(t, lookalike, "req-m6"); reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("status = %v, want DENIED — a tag spelled like an incarnation is not membership", reply.GetStatus())
	}
}

func TestAugur_Denied_QueryNotInAllow(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowVault("vault-prod") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurCovenRite(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"x": "y"}}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-3", OmenName: "vault-prod", Query: "secret/keeper/other",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("status = %v, want DENIED", reply.GetStatus())
	}
	if kv.gotPath != "" {
		t.Errorf("ReadKV must not be called when query not in allow")
	}
}

// TestAugur_SIDFromMTLS — the handler takes the SID from its argument (mTLS
// peer cert), and covens are resolved for THAT SID, ignoring any sid inside
// AugurRequest (there isn't one in the proto anyway). Verifies: covens resolve
// uses the authoritative SID.
func TestAugur_SIDFromMTLS(t *testing.T) {
	const authoritativeSID = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowVault("vault-prod") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurCovenRite(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"k": "v"}}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, authoritativeSID)

	h.processAugurRequest(context.Background(), authoritativeSID, "sess", &keeperv1.AugurRequest{
		RequestId: "req-4", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK", reply.GetStatus())
	}
	if db.gotHostSID != authoritativeSID {
		t.Errorf("subject resolved for %q, want authoritative %q", db.gotHostSID, authoritativeSID)
	}
	// audit records the authoritative SID.
	if aw.snapshot()[0].Payload["sid"] != authoritativeSID {
		t.Errorf("audit sid = %v, want %q", aw.snapshot()[0].Payload["sid"], authoritativeSID)
	}
}

func TestAugur_VaultReadFail_Error(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowVault("vault-prod") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurCovenRite(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{err: errors.New("vault down")}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-5", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_ERROR {
		t.Fatalf("status = %v, want ERROR", reply.GetStatus())
	}
	// On ERROR (access granted, but the fetch didn't happen) there's no audit event.
	if len(aw.snapshot()) != 0 {
		t.Errorf("expected no audit on fetch error, got %d", len(aw.snapshot()))
	}
}

// TestAugur_GoroutinePath — handleAugurRequest starts processing in a goroutine;
// verifies the reply still arrives (full dispatch → goroutine path).
func TestAugur_GoroutinePath(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowVault("vault-prod") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurCovenRite(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"k": "v"}}
	aw := &recordingAudit{}
	h, outCh := newAugurHandler(t, db, kv, aw, sid)

	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "req-6", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK", reply.GetStatus())
	}
}

func TestAugur_NilDeps_NoPanic(t *testing.T) {
	deps := EventStreamDeps{SeedDB: &fakeSeedDB{}, AuditWriter: &recordingAudit{}, KID: "kid"}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	h := newEventStreamHandler(deps, discardLogger(t))
	// Augur=nil → warn + no-op, no panic.
	h.handleAugurRequest(context.Background(), "host", "sess", &keeperv1.AugurRequest{OmenName: "x"})
}

// assertNoSecretInPayload — asserts the payload contains no secret values (s3cr3t / svc).
func assertNoSecretInPayload(t *testing.T, payload map[string]any) {
	t.Helper()
	b, _ := json.Marshal(payload)
	for _, leak := range []string{"s3cr3t", "svc", "password", "username"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("audit payload leaks secret material %q: %s", leak, b)
		}
	}
}

// --- prom / elk round-trip + semaphore ----------------------------------

func augurOmenRowProm(name string) pgx.Row {
	return augurValRow{vals: []any{name, "prometheus", "https://prom.example.com:9090", "vault:secret/keeper/" + name, nil, augurTestNow, nil}}
}

func augurOmenRowELK(name string) pgx.Row {
	return augurValRow{vals: []any{name, "elk", "https://elk.example.com:9200", "vault:secret/keeper/" + name, nil, augurTestNow, nil}}
}

func augurRiteRowQueries(id int, omen, coven string, queries ...string) []any {
	b, _ := json.Marshal(map[string][]string{"queries": queries})
	return augurRiteRowAllow(id, omen, subject.Selector{Covens: []string{coven}}, b)
}

func augurRiteRowIndices(id int, omen, coven string, indices ...string) []any {
	b, _ := json.Marshal(map[string][]string{"indices": indices})
	return augurRiteRowAllow(id, omen, subject.Selector{Covens: []string{coven}}, b)
}

func jsonRespDoer(body string) augur.HTTPDoer {
	return stubDoer{resp: func() (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}}
}

func TestAugur_Prometheus_RoundTrip_OK(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowProm("prom-main") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowQueries(1, "prom-main", "prod", "up")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"token": "tkn"}}
	doer := jsonRespDoer(`{"status":"success","data":{"result":[]}}`)
	aw := &recordingAudit{}
	h, outCh := newAugurHandlerEgress(t, db, kv, doer, aw, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "p-1", ApplyId: "apply-p", OmenName: "prom-main", Query: "up",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK (error=%q)", reply.GetStatus(), reply.GetError())
	}
	if reply.GetInlineData().GetFields()["status"].GetStringValue() != "success" {
		t.Errorf("inline_data not carried: %v", reply.GetInlineData().AsMap())
	}
	evs := aw.snapshot()
	if len(evs) != 1 || evs[0].EventType != audit.EventAugurFetchBrokered {
		t.Fatalf("expected augur.fetch_brokered, got %+v", evs)
	}
}

func TestAugur_Prometheus_Denied_QueryNotInAllow(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowProm("prom-main") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowQueries(1, "prom-main", "prod", "up")}}, nil
		},
	}
	doer := jsonRespDoer(`{}`)
	aw := &recordingAudit{}
	h, outCh := newAugurHandlerEgress(t, db, &stubKV{}, doer, aw, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "p-2", OmenName: "prom-main", Query: "node_load1",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("status = %v, want DENIED", reply.GetStatus())
	}
}

func TestAugur_ELK_RoundTrip_OK(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowELK("elk-logs") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowIndices(1, "elk-logs", "prod", "logs-app")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"api_key": "ak"}}
	doer := jsonRespDoer(`{"took":1,"hits":{"hits":[]}}`)
	aw := &recordingAudit{}
	h, outCh := newAugurHandlerEgress(t, db, kv, doer, aw, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "e-1", ApplyId: "apply-e", OmenName: "elk-logs", Query: "logs-app",
	})
	reply := recvReply(t, outCh)
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatalf("status = %v, want OK (error=%q)", reply.GetStatus(), reply.GetError())
	}
	if int(reply.GetInlineData().GetFields()["took"].GetNumberValue()) != 1 {
		t.Errorf("inline_data not carried: %v", reply.GetInlineData().AsMap())
	}
}

// TestAugur_Semaphore_Overflow — with the semaphore full, a new AugurRequest
// gets ERROR without spawning processing. Limit=1, the first request blocks in
// the fetch (the doer waits for a signal) → the second hits the full semaphore.
func TestAugur_Semaphore_Overflow(t *testing.T) {
	const sid = "host.example.com"
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var once sync.Once
	blockingDoer := stubDoer{resp: func() (*http.Response, error) {
		once.Do(func() { started <- struct{}{} })
		<-release // keep the slot occupied
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	}}
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowProm("prom-main") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowQueries(1, "prom-main", "prod", "up")}}, nil
		},
	}
	aw := &recordingAudit{}
	h, outCh := newAugurHandlerEgress(t, db, &stubKV{data: map[string]any{}}, blockingDoer, aw, sid, 1)

	// First request — takes the only slot and hangs in the fetch.
	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "s-1", OmenName: "prom-main", Query: "up",
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not reach fetch (semaphore not held)")
	}

	// Second — semaphore full → immediate ERROR without spawning.
	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "s-2", OmenName: "prom-main", Query: "up",
	})
	reply := recvReply(t, outCh)
	if reply.GetRequestId() != "s-2" {
		t.Fatalf("expected reply on s-2 (overflow), got %q", reply.GetRequestId())
	}
	if reply.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_ERROR {
		t.Fatalf("overflow status = %v, want ERROR", reply.GetStatus())
	}
	if !strings.Contains(reply.GetError(), "concurrency") && !strings.Contains(reply.GetError(), "busy") {
		t.Errorf("overflow error = %q, want concurrency/busy", reply.GetError())
	}

	// Release the first — it should complete OK and free the slot.
	close(release)
	r1 := recvReply(t, outCh)
	if r1.GetRequestId() != "s-1" || r1.GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Errorf("first request: id=%q status=%v, want s-1/OK", r1.GetRequestId(), r1.GetStatus())
	}
}

// --- keeper_augur_* metrics wire-up ----------------------------------

// newAugurHandlerWithMetrics — like newAugurHandler, but with a registered
// keeper_augur_* descriptor in AugurDeps.Metrics; returns the Registry for
// scraping. concurrency=0 → default.
func newAugurHandlerWithMetrics(t *testing.T, db augurDB, kv augur.KVReader, doer augur.HTTPDoer, aw audit.Writer, sid string, concurrency int) (*eventStreamHandler, <-chan *keeperv1.FromKeeper, *obs.Registry) {
	t.Helper()
	mgr := NewStreamManager(discardLogger(t))
	outCh := mgr.Register(sid)
	out, err := NewOutbound(OutboundDeps{Manager: mgr, AuditWriter: aw, Logger: discardLogger(t)})
	if err != nil {
		t.Fatalf("NewOutbound: %v", err)
	}
	reg := obs.NewRegistry()
	deps := EventStreamDeps{
		SeedDB:           &fakeSeedDB{},
		AuditWriter:      aw,
		KID:              "kid-test",
		Manager:          mgr,
		AugurConcurrency: concurrency,
		Augur: &AugurDeps{
			DB:          db,
			Vault:       kv,
			Egress:      doer,
			AuditWriter: aw,
			Outbound:    out,
			Metrics:     augur.RegisterBrokerMetrics(reg),
		},
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t)), outCh, reg
}

// TestAugurMetrics_FetchOK — a successful broker call increments
// fetch_total{source=vault,decision=ok}.
func TestAugurMetrics_FetchOK(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowVault("vault-prod") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurCovenRite(1, "vault-prod", "prod", "secret/keeper/db")}}, nil
		},
	}
	kv := &stubKV{data: map[string]any{"k": "v"}}
	h, outCh, reg := newAugurHandlerWithMetrics(t, db, kv, stubDoer{}, &recordingAudit{}, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "m-1", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	if recvReply(t, outCh).GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Fatal("expected OK reply")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_augur_fetch_total{decision="ok",source="vault"} 1`) {
		t.Errorf("fetch_total ok/vault mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_augur_fetch_duration_seconds_count{source="vault"} 1`) {
		t.Errorf("fetch_duration vault count mismatch; got=\n%s", body)
	}
	// Secret/omen/query must not leak into metrics.
	for _, leak := range []string{"omen=", "query=", "sid=", "secret/keeper/db", "vault-prod"} {
		if strings.Contains(body, leak) {
			t.Errorf("augur metrics leak %q; got=\n%s", leak, body)
		}
	}
}

// TestAugurMetrics_Denied — a denied resolve increments
// fetch_total{decision=denied}.
func TestAugurMetrics_Denied(t *testing.T) {
	const sid = "host.example.com"
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowVault("vault-prod") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: nil}, nil // no Rites → denied
		},
	}
	h, outCh, reg := newAugurHandlerWithMetrics(t, db, &stubKV{}, stubDoer{}, &recordingAudit{}, sid, 0)

	h.processAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "m-2", OmenName: "vault-prod", Query: "secret/keeper/db",
	})
	if recvReply(t, outCh).GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatal("expected DENIED reply")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `decision="denied"`) {
		t.Errorf("fetch_total denied missing; got=\n%s", body)
	}
}

// TestAugurMetrics_SemaphoreOverflow — a concurrency-limit rejection is counted as
// fetch_total{source=unknown,decision=error}.
func TestAugurMetrics_SemaphoreOverflow(t *testing.T) {
	const sid = "host.example.com"
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var once sync.Once
	blockingDoer := stubDoer{resp: func() (*http.Response, error) {
		once.Do(func() { started <- struct{}{} })
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	}}
	db := &augurFakeDB{
		omenRow:    func() pgx.Row { return augurOmenRowProm("prom-main") },
		hostCovens: []string{"prod"},
		riteRows: func() (pgx.Rows, error) {
			return &augurRiteRows{rows: [][]any{augurRiteRowQueries(1, "prom-main", "prod", "up")}}, nil
		},
	}
	h, outCh, reg := newAugurHandlerWithMetrics(t, db, &stubKV{data: map[string]any{}}, blockingDoer, &recordingAudit{}, sid, 1)

	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "ms-1", OmenName: "prom-main", Query: "up",
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not reach fetch")
	}
	// Second — rejected by the semaphore.
	h.handleAugurRequest(context.Background(), sid, "sess", &keeperv1.AugurRequest{
		RequestId: "ms-2", OmenName: "prom-main", Query: "up",
	})
	if recvReply(t, outCh).GetStatus() != keeperv1.AugurStatus_AUGUR_STATUS_ERROR {
		t.Fatal("expected ERROR on overflow")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_augur_fetch_total{decision="error",source="unknown"} 1`) {
		t.Errorf("overflow error/unknown count mismatch; got=\n%s", body)
	}

	close(release)
	_ = recvReply(t, outCh) // drain the first request's completion
}
