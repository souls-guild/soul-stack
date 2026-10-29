package voyage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// claimFake scripts the statements of one ClaimNext: the peek answers with
// peeked (one row per call, then no rows), the claim with the claimed flag, the
// deferral and the refusal with their row counts. Every statement is recorded.
type claimFake struct {
	peeked     []*Voyage
	now        time.Time
	claimMiss  bool // the claim CAS finds the row taken
	deferMiss  bool
	refuseMiss bool

	sqls      []string
	claimArgs []any
	deferArgs []any
	refuseArg []any
}

func (f *claimFake) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sqls = append(f.sqls, sql)
	if sql == deferByIDSQL {
		f.deferArgs = args
		if f.deferMiss {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return pgconn.CommandTag{}, errors.New("claimFake: unexpected Exec: " + sql)
}

func (f *claimFake) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.sqls = append(f.sqls, sql)
	switch sql {
	case peekClaimableSQL:
		if len(f.peeked) == 0 {
			return errRow{err: pgx.ErrNoRows}
		}
		v := f.peeked[0]
		f.peeked = f.peeked[1:]
		return voyageValuesRow{values: append(voyageValues(v), f.now)}
	case claimByIDSQL:
		f.claimArgs = args
		if f.claimMiss {
			return errRow{err: pgx.ErrNoRows}
		}
		return voyageValuesRow{values: voyageValues(&Voyage{VoyageID: args[2].(string), Kind: KindScenario, Status: StatusRunning, Attempt: 1})}
	case refuseByIDSQL:
		f.refuseArg = args
		n := 1
		if f.refuseMiss {
			n = 0
		}
		return voyageValuesRow{values: []any{n}}
	}
	return errRow{err: errors.New("claimFake: unexpected QueryRow: " + sql)}
}

func (f *claimFake) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("claimFake: Query not expected")
}

func (f *claimFake) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("claimFake: CopyFrom not expected")
}

// voyageValues lays v out in selectColumns order, the shape scanVoyage reads.
func voyageValues(v *Voyage) []any {
	scn := "deploy"
	resolved := v.TargetResolved
	if resolved == nil {
		resolved = []byte(`["inc-a","inc-b"]`)
	}
	return []any{
		v.VoyageID, string(v.Kind), &scn, (*string)(nil), []byte(`{}`),
		[]byte(resolved), []byte(nil),
		(*int)(nil), (*int)(nil), (*string)(nil), false,
		v.ScheduleAt, (*float64)(nil), (*string)(nil),
		1, 0, string(v.Status),
		(*string)(nil), (*time.Time)(nil), (*time.Time)(nil), v.Attempt,
		"archon-alice", time.Now().UTC(), v.StartedAt, (*time.Time)(nil), []byte(nil),
		(*int)(nil), (*int)(nil), (*float64)(nil), (*bool)(nil),
		(*string)(nil), v.InMaintenanceWindow, v.ConfirmedFreezeIDs,
	}
}

// voyageValuesRow scans values positionally into dest by reflection.
type voyageValuesRow struct{ values []any }

func (r voyageValuesRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return errors.New("voyageValuesRow: dest/values length mismatch")
	}
	for i, v := range r.values {
		dv := reflect.ValueOf(dest[i]).Elem()
		if v == nil {
			dv.Set(reflect.Zero(dv.Type()))
			continue
		}
		rv := reflect.ValueOf(v)
		if rv.Type().ConvertibleTo(dv.Type()) {
			dv.Set(rv.Convert(dv.Type()))
			continue
		}
		return errors.New("voyageValuesRow: column " + dv.Type().String() + " cannot take " + rv.Type().String())
	}
	return nil
}

// scriptedGate answers verdicts in order and records which rows it was asked about.
type scriptedGate struct {
	verdicts []GateVerdict
	asked    []string
	at       []time.Time
}

func (g *scriptedGate) Decide(_ context.Context, _ ExecQueryRower, v *Voyage, now time.Time) (GateVerdict, error) {
	g.asked = append(g.asked, v.VoyageID)
	g.at = append(g.at, now)
	if len(g.verdicts) == 0 {
		return GateVerdict{}, nil
	}
	out := g.verdicts[0]
	g.verdicts = g.verdicts[1:]
	return out, nil
}

func TestClaimNext_ValidationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, _, err := ClaimNext(ctx, &claimFake{}, "", time.Minute, &scriptedGate{}); err == nil || !strings.Contains(err.Error(), "empty kid") {
		t.Errorf("empty kid: %v", err)
	}
	if _, _, err := ClaimNext(ctx, &claimFake{}, "kid", 0, &scriptedGate{}); err == nil || !strings.Contains(err.Error(), "non-positive lease") {
		t.Errorf("zero lease: %v", err)
	}
	if _, _, err := ClaimNext(ctx, &claimFake{}, "kid", time.Minute, nil); err == nil || !strings.Contains(err.Error(), "nil start gate") {
		t.Errorf("nil gate: %v", err)
	}
}

func TestClaimNext_NothingClaimable(t *testing.T) {
	t.Parallel()
	v, held, err := ClaimNext(context.Background(), &claimFake{}, "kid-1", time.Minute, &scriptedGate{})
	if err != nil || v != nil || len(held) != 0 {
		t.Fatalf("ClaimNext = %v, %v, %v; want nothing", v, held, err)
	}
}

func TestClaimNext_SQLShape(t *testing.T) {
	t.Parallel()
	for _, frag := range []string{
		"status = 'pending' OR (status = 'scheduled' AND schedule_at <= NOW())",
		"ORDER BY created_at ASC",
		", NOW()\nFROM voyages",
	} {
		if !strings.Contains(peekClaimableSQL, frag) {
			t.Errorf("peekClaimableSQL missing %q", frag)
		}
	}
	for _, frag := range []string{
		"c.voyage_id = $3",
		"c.status = 'pending' OR (c.status = 'scheduled' AND c.schedule_at <= NOW())",
		"FOR UPDATE SKIP LOCKED",
		"status           = 'running'",
		"attempt          = v.attempt + 1",
		"started_at       = COALESCE(v.started_at, NOW())",
	} {
		if !strings.Contains(claimByIDSQL, frag) {
			t.Errorf("claimByIDSQL missing %q", frag)
		}
	}
	// A held-back row must not look started: neither statement touches the
	// start stamp or the attempt (ADR-0093 §3).
	for name, sql := range map[string]string{"defer": deferByIDSQL, "refuse": refuseByIDSQL} {
		if strings.Contains(sql, "attempt") || strings.Contains(sql, "started_at =") || strings.Contains(sql, "started_at=") {
			t.Errorf("%s statement writes attempt or started_at:\n%s", name, sql)
		}
		if !strings.Contains(sql, "started_at IS NULL") {
			t.Errorf("%s statement does not require an unstarted row:\n%s", name, sql)
		}
	}
}

func TestClaimNext_OpenGateClaimsThePeekedRow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := &claimFake{peeked: []*Voyage{{VoyageID: "v1", Kind: KindScenario, Status: StatusPending}}, now: now}
	g := &scriptedGate{}
	v, held, err := ClaimNext(context.Background(), f, "kid-7", 90*time.Second, g)
	if err != nil || v == nil || v.VoyageID != "v1" || len(held) != 0 {
		t.Fatalf("ClaimNext = %v, %v, %v; want v1 claimed", v, held, err)
	}
	if len(g.asked) != 1 || !g.at[0].Equal(now) {
		t.Errorf("gate asked %v at %v, want v1 at the database clock %v", g.asked, g.at, now)
	}
	if f.claimArgs[0] != "kid-7" || f.claimArgs[2] != "v1" {
		t.Errorf("claim args = %v, want kid-7 … v1", f.claimArgs)
	}
}

func TestClaimNext_GateIsNotAskedForCommandOrStartedRows(t *testing.T) {
	t.Parallel()
	started := time.Now()
	for name, row := range map[string]*Voyage{
		"command":   {VoyageID: "c1", Kind: KindCommand, Status: StatusPending},
		"reclaimed": {VoyageID: "s1", Kind: KindScenario, Status: StatusPending, StartedAt: &started, Attempt: 1},
	} {
		f := &claimFake{peeked: []*Voyage{row}}
		g := &scriptedGate{verdicts: []GateVerdict{{DeferTo: time.Now().Add(time.Hour)}}}
		v, _, err := ClaimNext(context.Background(), f, "kid", time.Minute, g)
		if err != nil || v == nil {
			t.Fatalf("%s: ClaimNext = %v, %v; want claimed", name, v, err)
		}
		if len(g.asked) != 0 {
			t.Errorf("%s: the gate was asked about %v", name, g.asked)
		}
	}
}

func TestClaimNext_ClosedGateDefersAndTriesTheNextRow(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 2, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	f := &claimFake{peeked: []*Voyage{
		{VoyageID: "frozen", Kind: KindScenario, Status: StatusScheduled},
		{VoyageID: "open", Kind: KindScenario, Status: StatusPending},
	}}
	g := &scriptedGate{verdicts: []GateVerdict{{DeferTo: at}, {}}}
	v, held, err := ClaimNext(context.Background(), f, "kid", time.Minute, g)
	if err != nil || v == nil || v.VoyageID != "open" {
		t.Fatalf("ClaimNext = %v, %v; want the second row claimed", v, err)
	}
	if len(held) != 1 || held[0].Voyage.VoyageID != "frozen" || !held[0].Verdict.DeferTo.Equal(at) {
		t.Fatalf("held = %+v, want frozen deferred to %v", held, at)
	}
	if f.deferArgs[0] != "frozen" || !f.deferArgs[1].(time.Time).Equal(at) || f.deferArgs[1].(time.Time).Location() != time.UTC {
		t.Errorf("defer args = %v, want frozen at %v in UTC", f.deferArgs, at)
	}
	if f.claimArgs[2] != "open" {
		t.Errorf("claimed %v, want open", f.claimArgs[2])
	}
}

func TestClaimNext_NoStartFailsTheRowWithItsReason(t *testing.T) {
	t.Parallel()
	f := &claimFake{peeked: []*Voyage{{VoyageID: "never", Kind: KindScenario, Status: StatusScheduled}}}
	g := &scriptedGate{verdicts: []GateVerdict{{NoStart: true, Reason: "no moment"}}}
	v, held, err := ClaimNext(context.Background(), f, "kid", time.Minute, g)
	if err != nil || v != nil {
		t.Fatalf("ClaimNext = %v, %v; want nothing claimed", v, err)
	}
	if len(held) != 1 || !held[0].Verdict.NoStart {
		t.Fatalf("held = %+v, want the row failed", held)
	}
	if f.refuseArg[0] != "never" || f.refuseArg[2] != TargetErrorNoCommonWindow || f.refuseArg[3] != "no moment" {
		t.Errorf("refuse args = %v", f.refuseArg)
	}
	if !strings.Contains(string(f.refuseArg[1].([]byte)), `"cancelled":2`) {
		t.Errorf("summary = %s, want both targets cancelled", f.refuseArg[1])
	}
}

func TestClaimNext_ALostRaceIsNotReportedAsHeld(t *testing.T) {
	t.Parallel()
	f := &claimFake{peeked: []*Voyage{{VoyageID: "taken", Kind: KindScenario, Status: StatusPending}}, deferMiss: true}
	g := &scriptedGate{verdicts: []GateVerdict{{DeferTo: time.Now().Add(time.Hour)}}}
	v, held, err := ClaimNext(context.Background(), f, "kid", time.Minute, g)
	if err != nil || v != nil || len(held) != 0 {
		t.Fatalf("ClaimNext = %v, %v, %v; want nothing claimed and nothing held", v, held, err)
	}
	for _, sql := range f.sqls {
		if sql == claimByIDSQL {
			t.Fatal("a row the gate closed was claimed after its deferral missed")
		}
	}
}
