package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// hostsTx — fake pgx.Tx для [UpdateHosts]: диспетчеризирует QueryRow / Query
// по SQL (SELECT FOR UPDATE → строка incarnation, UPDATE RETURNING →
// updated_at, SELECT FROM souls → набор существующих SID-ов).
//
// fakeTx из upgrade_test.go одноразовый по QueryRow (всегда возвращает
// selectRow), поэтому для multi-QueryRow-сценариев тут свой stub.
type hostsTx struct {
	// SELECT FOR UPDATE incarnation row (scanIncarnation columns).
	incRow pgx.Row

	// SELECT sid FROM souls WHERE sid = ANY($1): набор существующих SID-ов.
	soulsExists map[string]struct{}
	soulsErr    error

	// UPDATE RETURNING updated_at.
	updateErr error

	committed bool
	rolled    bool

	// Захваченные сайд-эффекты для assertions.
	updateCalled bool
	updateSpec   []byte
	soulsQueried []string
}

func (t *hostsTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("OK 1"), nil
}

func (t *hostsTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FOR UPDATE") {
		return t.incRow
	}
	if strings.Contains(sql, "UPDATE incarnation") && strings.Contains(sql, "spec") {
		t.updateCalled = true
		if len(args) >= 2 {
			if b, ok := args[1].([]byte); ok {
				t.updateSpec = b
			}
		}
		if t.updateErr != nil {
			return errRow{err: t.updateErr}
		}
		return staticRow{values: []any{time.Now().UTC()}}
	}
	return errRow{err: errors.New("hostsTx.QueryRow: unexpected SQL: " + sql)}
}

func (t *hostsTx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM souls WHERE sid = ANY") {
		if t.soulsErr != nil {
			return nil, t.soulsErr
		}
		sids, _ := args[0].([]string)
		t.soulsQueried = append([]string(nil), sids...)
		var rows []staticRow
		for _, sid := range sids {
			if _, ok := t.soulsExists[sid]; ok {
				rows = append(rows, staticRow{values: []any{sid}})
			}
		}
		return &fakeRows{rows: rows}, nil
	}
	return &fakeRows{}, nil
}

func (t *hostsTx) Commit(_ context.Context) error   { t.committed = true; return nil }
func (t *hostsTx) Rollback(_ context.Context) error { t.rolled = true; return nil }

func (t *hostsTx) Begin(context.Context) (pgx.Tx, error) { panic("hostsTx.Begin: unused") }
func (t *hostsTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("hostsTx.CopyFrom: unused")
}
func (t *hostsTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("hostsTx.SendBatch: unused")
}
func (t *hostsTx) LargeObjects() pgx.LargeObjects { panic("hostsTx.LargeObjects: unused") }
func (t *hostsTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("hostsTx.Prepare: unused")
}
func (t *hostsTx) Conn() *pgx.Conn { return nil }

type hostsPool struct{ tx *hostsTx }

func (p *hostsPool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return p.tx, nil
}

// makeHostsIncRow собирает scriptedRow под SELECT FOR UPDATE incarnation со
// spec, содержащим переданные hosts. status задаёт текущий статус.
func makeHostsIncRow(name, status string, hosts []SpecHost) pgx.Row {
	specMap := map[string]any{}
	if hosts != nil {
		arr := make([]any, 0, len(hosts))
		for _, h := range hosts {
			obj := map[string]any{"sid": h.SID}
			if h.Role != "" {
				obj["role"] = h.Role
			}
			arr = append(arr, obj)
		}
		specMap["hosts"] = arr
	}
	specBytes, _ := json.Marshal(specMap)
	now := time.Now().UTC()
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		specBytes, []byte("{}"), status,
		[]byte(nil), any(nil),
		now, now, []string(nil),
		any(nil), []byte(nil),
	}}
}

// soulsSet — sugar для построения map[string]struct{} из списка SID-ов.
func soulsSet(sids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(sids))
	for _, s := range sids {
		m[s] = struct{}{}
	}
	return m
}

// --- ValidHostsMode ---------------------------------------------------

func TestValidHostsMode(t *testing.T) {
	for _, m := range []UpdateHostsMode{ModeReplace, ModeAppend, ModeRemove} {
		if !ValidHostsMode(m) {
			t.Errorf("ValidHostsMode(%q) = false, want true", m)
		}
	}
	if ValidHostsMode(UpdateHostsMode("upsert")) {
		t.Error("ValidHostsMode(\"upsert\") = true, want false")
	}
	if ValidHostsMode(UpdateHostsMode("")) {
		t.Error("ValidHostsMode(\"\") = true, want false")
	}
}

// --- mergeHosts (pure) ------------------------------------------------

func TestMergeHosts_Replace(t *testing.T) {
	existing := []SpecHost{{SID: "a", Role: "master"}, {SID: "b", Role: "replica"}}
	payload := []SpecHost{{SID: "c", Role: "master"}}
	got := mergeHosts(existing, payload, ModeReplace)
	if len(got) != 1 || got[0].SID != "c" || got[0].Role != "master" {
		t.Errorf("Replace = %+v, want [c master]", got)
	}
}

func TestMergeHosts_Replace_EmptyClears(t *testing.T) {
	existing := []SpecHost{{SID: "a", Role: "master"}}
	got := mergeHosts(existing, nil, ModeReplace)
	if len(got) != 0 {
		t.Errorf("Replace nil = %+v, want []", got)
	}
}

func TestMergeHosts_Append_Updates(t *testing.T) {
	existing := []SpecHost{{SID: "a", Role: "master"}, {SID: "b", Role: "replica"}}
	payload := []SpecHost{{SID: "a", Role: "replica"}, {SID: "c", Role: "master"}}
	got := mergeHosts(existing, payload, ModeAppend)
	// a → role updated, b сохранён, c добавлен в конец.
	if len(got) != 3 {
		t.Fatalf("Append len = %d, want 3", len(got))
	}
	if got[0].SID != "a" || got[0].Role != "replica" {
		t.Errorf("got[0] = %+v, want {a replica}", got[0])
	}
	if got[1].SID != "b" || got[1].Role != "replica" {
		t.Errorf("got[1] = %+v, want {b replica}", got[1])
	}
	if got[2].SID != "c" || got[2].Role != "master" {
		t.Errorf("got[2] = %+v, want {c master}", got[2])
	}
}

func TestMergeHosts_Remove(t *testing.T) {
	existing := []SpecHost{{SID: "a", Role: "master"}, {SID: "b"}, {SID: "c", Role: "replica"}}
	payload := []SpecHost{{SID: "b"}, {SID: "c"}}
	got := mergeHosts(existing, payload, ModeRemove)
	if len(got) != 1 || got[0].SID != "a" {
		t.Errorf("Remove = %+v, want [a]", got)
	}
}

// --- readSpecHosts (pure) ---------------------------------------------

func TestReadSpecHosts_OK(t *testing.T) {
	spec := map[string]any{
		"hosts": []any{
			map[string]any{"sid": "a", "role": "master"},
			map[string]any{"sid": "b"},
		},
	}
	got := readSpecHosts(spec)
	if len(got) != 2 || got[0].SID != "a" || got[0].Role != "master" {
		t.Errorf("readSpecHosts = %+v", got)
	}
	if got[1].SID != "b" || got[1].Role != "" {
		t.Errorf("readSpecHosts second = %+v", got[1])
	}
}

func TestReadSpecHosts_NoKey(t *testing.T) {
	if got := readSpecHosts(map[string]any{"other": "x"}); got != nil {
		t.Errorf("readSpecHosts no key = %+v, want nil", got)
	}
}

func TestReadSpecHosts_MalformedSkipped(t *testing.T) {
	spec := map[string]any{
		"hosts": []any{
			"not-an-object",
			map[string]any{"sid": "a"},
			map[string]any{"role": "no-sid"},
		},
	}
	got := readSpecHosts(spec)
	if len(got) != 1 || got[0].SID != "a" {
		t.Errorf("malformed-skip = %+v", got)
	}
}

// --- UpdateHosts ------------------------------------------------------

func TestUpdateHosts_Replace_HappyPath(t *testing.T) {
	tx := &hostsTx{
		incRow:      makeHostsIncRow("redis-prod", "ready", []SpecHost{{SID: "old.example", Role: "master"}}),
		soulsExists: soulsSet("a.example", "b.example"),
	}
	pool := &hostsPool{tx: tx}
	res, err := UpdateHosts(context.Background(), pool, UpdateHostsInput{
		Name: "redis-prod",
		Hosts: []SpecHost{
			{SID: "a.example", Role: "master"},
			{SID: "b.example", Role: "replica"},
		},
		Mode: ModeReplace,
	})
	if err != nil {
		t.Fatalf("UpdateHosts: %v", err)
	}
	if !tx.committed {
		t.Error("tx not committed")
	}
	if !tx.updateCalled {
		t.Error("UPDATE not called")
	}
	if len(res.OldHosts) != 1 || res.OldHosts[0].SID != "old.example" {
		t.Errorf("OldHosts = %+v", res.OldHosts)
	}
	if len(res.NewHosts) != 2 {
		t.Fatalf("NewHosts len = %d, want 2", len(res.NewHosts))
	}
	if res.NewHosts[0].SID != "a.example" || res.NewHosts[1].SID != "b.example" {
		t.Errorf("NewHosts = %+v", res.NewHosts)
	}
	// Проверяем, что в spec действительно ушли новые hosts.
	var saved map[string]any
	if err := json.Unmarshal(tx.updateSpec, &saved); err != nil {
		t.Fatalf("unmarshal saved spec: %v", err)
	}
	arr, _ := saved["hosts"].([]any)
	if len(arr) != 2 {
		t.Errorf("saved spec.hosts len = %d, want 2", len(arr))
	}
}

func TestUpdateHosts_Append_UpdatesExistingRole(t *testing.T) {
	tx := &hostsTx{
		incRow: makeHostsIncRow("redis-prod", "ready", []SpecHost{
			{SID: "a.example", Role: "master"},
		}),
		soulsExists: soulsSet("a.example", "b.example"),
	}
	pool := &hostsPool{tx: tx}
	res, err := UpdateHosts(context.Background(), pool, UpdateHostsInput{
		Name: "redis-prod",
		Hosts: []SpecHost{
			{SID: "a.example", Role: "replica"}, // role-update
			{SID: "b.example", Role: "master"},  // new
		},
		Mode: ModeAppend,
	})
	if err != nil {
		t.Fatalf("UpdateHosts append: %v", err)
	}
	if len(res.NewHosts) != 2 {
		t.Fatalf("NewHosts len = %d, want 2", len(res.NewHosts))
	}
	if res.NewHosts[0].SID != "a.example" || res.NewHosts[0].Role != "replica" {
		t.Errorf("existing role not updated: %+v", res.NewHosts[0])
	}
	if res.NewHosts[1].SID != "b.example" {
		t.Errorf("new SID not appended: %+v", res.NewHosts[1])
	}
}

func TestUpdateHosts_Remove(t *testing.T) {
	tx := &hostsTx{
		incRow: makeHostsIncRow("redis-prod", "ready", []SpecHost{
			{SID: "a.example", Role: "master"},
			{SID: "b.example", Role: "replica"},
			{SID: "c.example"},
		}),
		soulsExists: soulsSet("b.example"),
	}
	pool := &hostsPool{tx: tx}
	res, err := UpdateHosts(context.Background(), pool, UpdateHostsInput{
		Name:  "redis-prod",
		Hosts: []SpecHost{{SID: "b.example"}},
		Mode:  ModeRemove,
	})
	if err != nil {
		t.Fatalf("UpdateHosts remove: %v", err)
	}
	if len(res.NewHosts) != 2 {
		t.Fatalf("NewHosts len = %d, want 2", len(res.NewHosts))
	}
	for _, h := range res.NewHosts {
		if h.SID == "b.example" {
			t.Errorf("b.example must be removed, NewHosts = %+v", res.NewHosts)
		}
	}
}

func TestUpdateHosts_NotFound(t *testing.T) {
	tx := &hostsTx{incRow: errRow{err: pgx.ErrNoRows}}
	pool := &hostsPool{tx: tx}
	_, err := UpdateHosts(context.Background(), pool, UpdateHostsInput{
		Name:  "absent",
		Hosts: []SpecHost{{SID: "a.example"}},
		Mode:  ModeReplace,
	})
	if !errors.Is(err, ErrIncarnationNotFound) {
		t.Fatalf("err = %v, want ErrIncarnationNotFound", err)
	}
	if tx.updateCalled {
		t.Error("UPDATE called on not-found")
	}
}

func TestUpdateHosts_DestroyingRejected(t *testing.T) {
	tx := &hostsTx{
		incRow: makeHostsIncRow("redis-prod", "destroying", nil),
	}
	pool := &hostsPool{tx: tx}
	_, err := UpdateHosts(context.Background(), pool, UpdateHostsInput{
		Name:  "redis-prod",
		Hosts: []SpecHost{{SID: "a.example"}},
		Mode:  ModeReplace,
	})
	if !errors.Is(err, ErrIncarnationNotEditable) {
		t.Fatalf("err = %v, want ErrIncarnationNotEditable", err)
	}
	if tx.updateCalled {
		t.Error("UPDATE called on destroying status")
	}
}

func TestUpdateHosts_UnknownSID(t *testing.T) {
	tx := &hostsTx{
		incRow:      makeHostsIncRow("redis-prod", "ready", nil),
		soulsExists: soulsSet("a.example"), // только a, b нет
	}
	pool := &hostsPool{tx: tx}
	_, err := UpdateHosts(context.Background(), pool, UpdateHostsInput{
		Name: "redis-prod",
		Hosts: []SpecHost{
			{SID: "a.example"},
			{SID: "b.example", Role: "master"},
		},
		Mode: ModeReplace,
	})
	var unk *ErrUnknownSouls
	if !errors.As(err, &unk) {
		t.Fatalf("err = %v, want *ErrUnknownSouls", err)
	}
	if len(unk.Missing) != 1 || unk.Missing[0] != "b.example" {
		t.Errorf("Missing = %v, want [b.example]", unk.Missing)
	}
	if tx.updateCalled {
		t.Error("UPDATE called when SID validation failed")
	}
}

func TestUpdateHosts_InvalidName(t *testing.T) {
	pool := &hostsPool{tx: &hostsTx{}}
	_, err := UpdateHosts(context.Background(), pool, UpdateHostsInput{
		Name:  "Bad_Name",
		Hosts: []SpecHost{{SID: "a.example"}},
		Mode:  ModeReplace,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Errorf("err = %v, want invalid name", err)
	}
}

func TestUpdateHosts_InvalidMode(t *testing.T) {
	pool := &hostsPool{tx: &hostsTx{}}
	_, err := UpdateHosts(context.Background(), pool, UpdateHostsInput{
		Name:  "redis-prod",
		Hosts: []SpecHost{{SID: "a.example"}},
		Mode:  UpdateHostsMode("upsert"),
	})
	if err == nil || !strings.Contains(err.Error(), "invalid hosts mode") {
		t.Errorf("err = %v, want invalid hosts mode", err)
	}
}

// TestUpdateHosts_ReplaceEmpty — replace с пустым payload очищает spec.hosts.
// SID validation skip-ается (len(payload)==0), UPDATE проходит.
func TestUpdateHosts_ReplaceEmpty(t *testing.T) {
	tx := &hostsTx{
		incRow: makeHostsIncRow("redis-prod", "ready", []SpecHost{{SID: "a.example", Role: "master"}}),
	}
	pool := &hostsPool{tx: tx}
	res, err := UpdateHosts(context.Background(), pool, UpdateHostsInput{
		Name:  "redis-prod",
		Hosts: nil,
		Mode:  ModeReplace,
	})
	if err != nil {
		t.Fatalf("UpdateHosts replace-empty: %v", err)
	}
	if len(res.NewHosts) != 0 {
		t.Errorf("NewHosts = %+v, want empty", res.NewHosts)
	}
	// Souls existence query НЕ должен дергаться (len(payload)==0).
	if len(tx.soulsQueried) != 0 {
		t.Errorf("soulsQueried = %v, want none", tx.soulsQueried)
	}
}
