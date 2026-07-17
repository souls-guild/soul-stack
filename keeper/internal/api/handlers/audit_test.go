package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// fakeAuditPool — a narrow mock of [auditpg.queryRower] for the AuditHandler unit test.
// Does not validate SQL exactly — relies on the two queries (COUNT + SELECT). Returns
// two detached cases via rows/total/err.
type fakeAuditPool struct {
	countSeen  bool
	selectSeen bool

	selectArgs []any // args of the SELECT query (captures the forwarded filter)

	total int
	rows  []auditRow

	queryErr error
	countErr error
}

type auditRow struct {
	id        string
	createdAt time.Time
	eventType string
	source    string
	aid       *string
	corr      *string
	payload   []byte
}

func (f *fakeAuditPool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "COUNT(*)") {
		f.countSeen = true
		if f.countErr != nil {
			return errRow{err: f.countErr}
		}
		return staticRow{values: []any{f.total}}
	}
	return errRow{err: errors.New("fakeAuditPool.QueryRow: unexpected SQL")}
}

func (f *fakeAuditPool) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.selectSeen = true
	f.selectArgs = args
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if !strings.Contains(sql, "audit_log") {
		return nil, errors.New("fakeAuditPool.Query: unexpected SQL")
	}
	return &auditRowsIter{rows: f.rows}, nil
}

type auditRowsIter struct {
	rows []auditRow
	idx  int
}

func (it *auditRowsIter) Next() bool {
	if it.idx >= len(it.rows) {
		return false
	}
	it.idx++
	return true
}

func (it *auditRowsIter) Scan(dest ...any) error {
	r := it.rows[it.idx-1]
	*dest[0].(*string) = r.id
	*dest[1].(*time.Time) = r.createdAt
	*dest[2].(*string) = r.eventType
	*dest[3].(*string) = r.source
	*dest[4].(**string) = r.aid
	*dest[5].(**string) = r.corr
	*dest[6].(*[]byte) = r.payload
	return nil
}

func (it *auditRowsIter) Err() error                                   { return nil }
func (it *auditRowsIter) Close()                                       {}
func (it *auditRowsIter) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (it *auditRowsIter) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (it *auditRowsIter) Values() ([]any, error)                       { return nil, nil }
func (it *auditRowsIter) RawValues() [][]byte                          { return nil }
func (it *auditRowsIter) Conn() *pgx.Conn                              { return nil }

func newAuditHandler(t *testing.T, pool *fakeAuditPool) *AuditHandler {
	t.Helper()
	return NewAuditHandler(auditpg.NewReader(pool), nil)
}

// auditProblemStatus extracts the HTTP status from a *problemError (T5d handler-native:
// ListTyped returns a domain error, the status comes from the problem table).
func auditProblemStatus(t *testing.T, err error) int {
	t.Helper()
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("expected *problemError, got: %v", err)
	}
	return d.Status
}

func TestAuditHandler_List_200(t *testing.T) {
	aid := "archon-alice"
	corr := "01JABCDEFGHJKMNPQRSTVWXYZ0"
	pool := &fakeAuditPool{
		total: 1,
		rows: []auditRow{{
			id:        "01JABCDEFGHJKMNPQRSTVWXYZ1",
			createdAt: time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC),
			eventType: string(audit.EventOperatorCreated),
			source:    string(audit.SourceAPI),
			aid:       &aid,
			corr:      &corr,
			payload:   []byte(`{"aid":"archon-bob"}`),
		}},
	}
	h := newAuditHandler(t, pool)

	page, err := h.ListTyped(context.Background(), AuditListFilter{Offset: 0, Limit: 50})
	if err != nil {
		t.Fatalf("ListTyped error: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("total = %d, want 1", page.Total)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	got := page.Items[0]
	if got.ID != "01JABCDEFGHJKMNPQRSTVWXYZ1" || got.Type != string(audit.EventOperatorCreated) {
		t.Errorf("item = %+v", got)
	}
	if got.ArchonAID == nil || *got.ArchonAID != aid {
		t.Errorf("archon_aid = %v", got.ArchonAID)
	}
	if got.Payload["aid"] != "archon-bob" {
		t.Errorf("payload = %v", got.Payload)
	}
}

func TestAuditHandler_List_FiltersForwarded(t *testing.T) {
	pool := &fakeAuditPool{total: 0}
	h := newAuditHandler(t, pool)

	filter := AuditListFilter{
		Types:         []string{"operator.created", "operator.revoked"},
		Sources:       []string{"api"},
		ArchonAID:     "archon-alice",
		CorrelationID: "01JABCDEFGHJKMNPQRSTVWXYZ0",
		PayloadHerald: "ops-slack",
		PayloadVoyage: "voy-77",
		StartedAfter:  time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		StartedBefore: time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC),
		Offset:        0,
		Limit:         50,
	}
	if _, err := h.ListTyped(context.Background(), filter); err != nil {
		t.Fatalf("ListTyped error: %v", err)
	}
	if !pool.countSeen || !pool.selectSeen {
		t.Errorf("count/select not called: count=%v select=%v", pool.countSeen, pool.selectSeen)
	}
	var heraldForwarded, voyageForwarded bool
	for _, a := range pool.selectArgs {
		if s, ok := a.(string); ok {
			switch s {
			case "ops-slack":
				heraldForwarded = true
			case "voy-77":
				voyageForwarded = true
			}
		}
	}
	if !heraldForwarded {
		t.Errorf("payload_herald=ops-slack not passed through in SELECT-args: %v", pool.selectArgs)
	}
	if !voyageForwarded {
		t.Errorf("payload_voyage=voy-77 not passed through in SELECT-args: %v", pool.selectArgs)
	}
}

func TestAuditHandler_List_InvalidSource_422(t *testing.T) {
	h := newAuditHandler(t, &fakeAuditPool{})
	_, err := h.ListTyped(context.Background(), AuditListFilter{Sources: []string{"hax0r"}, Limit: 50})
	if got := auditProblemStatus(t, err); got != 422 {
		t.Errorf("status = %d, want 422", got)
	}
}

func TestAuditHandler_List_InvalidPagination_400(t *testing.T) {
	h := newAuditHandler(t, &fakeAuditPool{})
	// limit=0 out of range [1,1000] → CheckPageBounds 400 (contract invariant
	// kept in ListTyped; bad-int (non-numeric) is now rejected by huma at bind).
	_, err := h.ListTyped(context.Background(), AuditListFilter{Offset: 0, Limit: 0})
	if got := auditProblemStatus(t, err); got != 400 {
		t.Errorf("status = %d, want 400", got)
	}
}

func TestAuditHandler_List_ReaderError_500(t *testing.T) {
	pool := &fakeAuditPool{queryErr: errors.New("pg connection refused")}
	h := newAuditHandler(t, pool)
	_, err := h.ListTyped(context.Background(), AuditListFilter{Offset: 0, Limit: 50})
	if got := auditProblemStatus(t, err); got != 500 {
		t.Errorf("status = %d, want 500", got)
	}
}
