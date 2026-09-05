package sigil

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// rowStub replays one plugin_sigils row into the scan targets of [scanSigil], in the
// column order listActiveSQL selects. It exists to reach ONE case that the service can
// never produce and the DB CHECK refuses: a row whose `artifacts` column is not a list
// a signature could have been placed over.
type rowStub struct {
	alias     string
	artifacts []byte
}

func (r rowStub) Scan(dest ...any) error {
	vals := []any{
		int64(1), r.alias, "https://example.com/x.git", "v1", "git", r.artifacts,
		[]byte("sig"), []byte(`{"kind":"cloud_driver","protocol_version":1}`), "",
		"archon-a",
	}
	for i, v := range vals {
		switch d := dest[i].(type) {
		case *int64:
			*d = v.(int64)
		case *string:
			*d = v.(string)
		case *[]byte:
			*d = v.([]byte)
		}
	}
	return nil
}

// rowsStub is a pgx.Rows over a fixed list of rowStub.
type rowsStub struct {
	rows []rowStub
	i    int
}

func (r *rowsStub) Next() bool                                   { r.i++; return r.i <= len(r.rows) }
func (r *rowsStub) Scan(dest ...any) error                       { return r.rows[r.i-1].Scan(dest...) }
func (r *rowsStub) Close()                                       {}
func (r *rowsStub) Err() error                                   { return nil }
func (r *rowsStub) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *rowsStub) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rowsStub) Values() ([]any, error)                       { return nil, nil }
func (r *rowsStub) RawValues() [][]byte                          { return nil }
func (r *rowsStub) Conn() *pgx.Conn                              { return nil }

// rowsDB is an ExecQueryRower whose Query answers with a fixed row set.
type rowsDB struct{ rows []rowStub }

func (d rowsDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// QueryRow answers with the FIRST configured row, so a GetActive test exercises the
// artifacts column it was handed rather than a zero value that would fail for its own
// unrelated reason.
func (d rowsDB) QueryRow(context.Context, string, ...any) pgx.Row {
	if len(d.rows) == 0 {
		return rowStub{}
	}
	return d.rows[0]
}
func (d rowsDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &rowsStub{rows: d.rows}, nil
}

func mustArtifactsJSON(t *testing.T, rows []storedArtifact) []byte {
	t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// A row whose artifacts column is unusable is SKIPPED, and the rest of the allow-list
// still comes back.
//
// The reflex would be to fail the call. That is a fail-OPEN here, and the reason is
// what ListActive feeds: the SigilSnapshot every Soul applies as a ReplaceAll. An
// aborted list suppresses the snapshot, and the snapshot is the mechanism REVOCATION
// runs on — so one malformed row would keep every revoked grant alive on every
// connected Soul. Skipping keeps the blast radius at the row, which could never have
// verified anyway: the artifacts are half the signed block.
func TestListActive_SkipsAnUnusableRowAndKeepsTheRest(t *testing.T) {
	good := mustArtifactsJSON(t, []storedArtifact{{SHA256: strings.Repeat("a", 64)}})

	for name, broken := range map[string][]byte{
		"not json":           []byte("{"),
		"empty list":         []byte(`[]`),
		"digest is not hex":  mustArtifactsJSON(t, []storedArtifact{{SHA256: "nope"}}),
		"duplicate platform": mustArtifactsJSON(t, []storedArtifact{{OS: "linux", Arch: "amd64", SHA256: strings.Repeat("a", 64)}, {OS: "linux", Arch: "amd64", SHA256: strings.Repeat("b", 64)}}),
	} {
		t.Run(name, func(t *testing.T) {
			db := rowsDB{rows: []rowStub{
				{alias: "broken", artifacts: broken},
				{alias: "healthy", artifacts: good},
			}}
			got, err := ListActive(context.Background(), db)
			if err != nil {
				t.Fatalf("ListActive aborted on one bad row: %v — that suppresses the whole ReplaceAll snapshot", err)
			}
			if len(got) != 1 || got[0].Alias != "healthy" {
				t.Fatalf("got %d rows %+v, want only the healthy one", len(got), got)
			}
		})
	}
}

// GetActive, by contrast, still FAILS on such a row: it answers about ONE grant, and
// "this grant is broken" is the whole answer there — silently reporting no grant would
// be indistinguishable from "never approved".
func TestGetActive_FailsOnAnUnusableRow(t *testing.T) {
	db := rowsDB{rows: []rowStub{{alias: "broken", artifacts: []byte(`[]`)}}}
	if _, err := GetActive(context.Background(), db, "broken"); err == nil {
		t.Fatal("GetActive returned a grant whose artifacts column is not signable")
	}
}
