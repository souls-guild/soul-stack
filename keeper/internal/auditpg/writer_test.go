package auditpg

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// fakeExecer — in-memory execer для unit-тестов pgxWriter. Захватывает
// args каждого Exec; SQL не валидируется — это контракт ADR-022, не
// поведение pgxWriter.
type fakeExecer struct {
	calls   int
	lastSQL string
	args    []any
	err     error
}

func (f *fakeExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls++
	f.lastSQL = sql
	f.args = args
	if f.err != nil {
		return pgconn.CommandTag{}, f.err
	}
	return pgconn.CommandTag{}, nil
}

const maskedValue = "***MASKED***"

func TestWriter_WriteSuccess_FullEvent(t *testing.T) {
	fe := &fakeExecer{}
	w := NewWriter(fe)

	ts := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	ev := &audit.Event{
		AuditID:       "01HXYZABCDEFGHJKMNPQRSTVWX",
		EventType:     audit.EventConfigReloadSucceeded,
		Source:        audit.SourceSignal,
		ArchonAID:     "archon-alice",
		CorrelationID: "01HCOR0000000000000000000Z",
		Payload:       map[string]any{"path": "/etc/keeper.yml", "password": "p"},
		CreatedAt:     ts,
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fe.calls != 1 {
		t.Fatalf("calls = %d, want 1", fe.calls)
	}
	if !strings.Contains(fe.lastSQL, "INSERT INTO audit_log") {
		t.Errorf("SQL missing INSERT: %q", fe.lastSQL)
	}
	if len(fe.args) != 7 {
		t.Fatalf("args len = %d, want 7", len(fe.args))
	}
	if fe.args[0] != "01HXYZABCDEFGHJKMNPQRSTVWX" {
		t.Errorf("args[0] audit_id = %v", fe.args[0])
	}
	if fe.args[1] != ts {
		t.Errorf("args[1] created_at = %v, want %v", fe.args[1], ts)
	}
	if fe.args[2] != "config.reload_succeeded" {
		t.Errorf("args[2] event_type = %v", fe.args[2])
	}
	if fe.args[3] != "signal" {
		t.Errorf("args[3] source = %v", fe.args[3])
	}
	if fe.args[4] != "archon-alice" {
		t.Errorf("args[4] archon_aid = %v", fe.args[4])
	}
	if fe.args[5] != "01HCOR0000000000000000000Z" {
		t.Errorf("args[5] correlation_id = %v", fe.args[5])
	}
	payloadBytes, ok := fe.args[6].([]byte)
	if !ok {
		t.Fatalf("args[6] payload type = %T, want []byte", fe.args[6])
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload["password"] != maskedValue {
		t.Errorf("payload.password = %v, want masked (writer must run MaskSecrets)", payload["password"])
	}
	if payload["path"] != "/etc/keeper.yml" {
		t.Errorf("payload.path = %v, want passthrough", payload["path"])
	}
}

func TestWriter_AutoFillsAuditIDAndCreatedAt(t *testing.T) {
	fe := &fakeExecer{}
	w := NewWriter(fe)

	ev := &audit.Event{
		EventType: audit.EventConfigReloadFailed,
		Source:    audit.SourceAPI,
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}
	auditID, ok := fe.args[0].(string)
	if !ok || !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(auditID) {
		t.Errorf("auto AuditID = %v, want 26-char ULID", fe.args[0])
	}
	if fe.args[1] != nil {
		t.Errorf("zero CreatedAt should pass NULL (=nil arg), got %v", fe.args[1])
	}
}

func TestWriter_NullsForEmptyStrings(t *testing.T) {
	fe := &fakeExecer{}
	w := NewWriter(fe)

	ev := &audit.Event{
		EventType: audit.EventConfigReloadSucceeded,
		Source:    audit.SourceKeeperInternal,
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fe.args[4] != nil {
		t.Errorf("empty ArchonAID should pass NULL, got %v", fe.args[4])
	}
	if fe.args[5] != nil {
		t.Errorf("empty CorrelationID should pass NULL, got %v", fe.args[5])
	}
}

func TestWriter_NilPayloadProducesEmptyJSONB(t *testing.T) {
	fe := &fakeExecer{}
	w := NewWriter(fe)
	ev := &audit.Event{
		EventType: audit.EventConfigReloadSucceeded,
		Source:    audit.SourceSignal,
	}
	if err := w.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, ok := fe.args[6].([]byte)
	if !ok || string(b) != "{}" {
		t.Errorf("nil payload → args[6] = %q, want \"{}\"", fe.args[6])
	}
}

func TestWriter_RejectsInvalidSource(t *testing.T) {
	fe := &fakeExecer{}
	w := NewWriter(fe)
	ev := &audit.Event{
		EventType: audit.EventConfigReloadSucceeded,
		Source:    audit.Source("hax0r"),
	}
	err := w.Write(context.Background(), ev)
	if err == nil {
		t.Fatal("Write with invalid Source returned nil; want error")
	}
	if !strings.Contains(err.Error(), "invalid source") {
		t.Errorf("err = %v, want substring \"invalid source\"", err)
	}
	if fe.calls != 0 {
		t.Errorf("Exec called %d times on invalid event; want 0", fe.calls)
	}
}

func TestWriter_RejectsEmptyEventType(t *testing.T) {
	fe := &fakeExecer{}
	w := NewWriter(fe)
	ev := &audit.Event{Source: audit.SourceAPI}
	if err := w.Write(context.Background(), ev); err == nil {
		t.Fatal("Write with empty EventType returned nil; want error")
	}
	if fe.calls != 0 {
		t.Errorf("Exec called on empty event_type; want 0")
	}
}

func TestWriter_RejectsNilEvent(t *testing.T) {
	fe := &fakeExecer{}
	w := NewWriter(fe)
	if err := w.Write(context.Background(), nil); err == nil {
		t.Fatal("Write(nil) returned nil; want error")
	}
	if fe.calls != 0 {
		t.Errorf("Exec called on nil event; want 0")
	}
}

func TestWriter_PropagatesExecError(t *testing.T) {
	wantErr := errors.New("pg down")
	fe := &fakeExecer{err: wantErr}
	w := NewWriter(fe)
	ev := &audit.Event{EventType: audit.EventConfigReloadSucceeded, Source: audit.SourceSignal}
	err := w.Write(context.Background(), ev)
	if err == nil {
		t.Fatal("Write returned nil on Exec-error; want wrapped error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
}
