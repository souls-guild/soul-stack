package push

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeTargetReadWriter — in-memory storage for targets with a swappable error.
type fakeTargetReadWriter struct {
	rows         map[string]*soul.SSHTarget // sid → target (nil = ssh_target IS NULL)
	missingSouls map[string]struct{}        // sid → ErrSoulNotFound on read
	selectErr    error
	updateErr    error
	selectCalls  int
	updateCalls  int
}

func newFakeTargetRW() *fakeTargetReadWriter {
	return &fakeTargetReadWriter{
		rows:         map[string]*soul.SSHTarget{},
		missingSouls: map[string]struct{}{},
	}
}

func (f *fakeTargetReadWriter) SelectSshTarget(_ context.Context, sid string) (*soul.SSHTarget, error) {
	f.selectCalls++
	if f.selectErr != nil {
		return nil, f.selectErr
	}
	if _, missing := f.missingSouls[sid]; missing {
		return nil, soul.ErrSoulNotFound
	}
	return f.rows[sid], nil
}

func (f *fakeTargetReadWriter) UpdateSshTarget(_ context.Context, sid string, target *soul.SSHTarget) error {
	f.updateCalls++
	if f.updateErr != nil {
		return f.updateErr
	}
	f.rows[sid] = target
	return nil
}

// fakeProviderReadWriter — in-memory storage for push_providers.
type fakeProviderReadWriter struct {
	rows        map[string]*pushprovider.PushProvider
	selectErr   error
	insertErr   error
	selectCalls int
	insertCalls int
}

func newFakeProviderRW() *fakeProviderReadWriter {
	return &fakeProviderReadWriter{rows: map[string]*pushprovider.PushProvider{}}
}

func (f *fakeProviderReadWriter) SelectByName(_ context.Context, name string) (*pushprovider.PushProvider, error) {
	f.selectCalls++
	if f.selectErr != nil {
		return nil, f.selectErr
	}
	p, ok := f.rows[name]
	if !ok {
		return nil, pushprovider.ErrPushProviderNotFound
	}
	return p, nil
}

func (f *fakeProviderReadWriter) Insert(_ context.Context, p *pushprovider.PushProvider) error {
	f.insertCalls++
	if f.insertErr != nil {
		return f.insertErr
	}
	if _, exists := f.rows[p.Name]; exists {
		return pushprovider.ErrPushProviderAlreadyExists
	}
	// Copy the value so the test sees the row as it arrived, without later
	// caller mutations (defense, mimicking a PG INSERT).
	row := *p
	f.rows[p.Name] = &row
	return nil
}

// fakeAuditor — collects all Write calls.
type fakeAuditor struct {
	events   []*audit.Event
	writeErr error
}

func (f *fakeAuditor) Write(_ context.Context, ev *audit.Event) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	// Copy so the caller's Payload doesn't shift after Write.
	cp := *ev
	if ev.Payload != nil {
		cp.Payload = make(map[string]any, len(ev.Payload))
		for k, v := range ev.Payload {
			cp.Payload[k] = v
		}
	}
	f.events = append(f.events, &cp)
	return nil
}

func newImporter(t *testing.T, tw *fakeTargetReadWriter, pw *fakeProviderReadWriter, au *fakeAuditor) *AutoImporter {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	imp, err := NewAutoImporter(AutoImporterDeps{
		TargetReader:   tw,
		TargetWriter:   tw,
		ProviderReader: pw,
		ProviderWriter: pw,
		Auditor:        au,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("NewAutoImporter: %v", err)
	}
	return imp
}

func TestAutoImporter_FlagsFalse_NoOp(t *testing.T) {
	tw := newFakeTargetRW()
	pw := newFakeProviderRW()
	au := &fakeAuditor{}
	imp := newImporter(t, tw, pw, au)

	cfg := config.KeeperPush{
		Targets:   []config.KeeperPushTarget{{SID: "soul-a.example.com"}},
		Providers: []config.KeeperPushProvider{{Name: "vault", Params: map[string]any{"role": "k"}}},
		// AutoImportLegacyTargets / AutoImportLegacyProviders = false.
	}
	if err := imp.ImportLegacyOnStart(context.Background(), cfg); err != nil {
		t.Fatalf("ImportLegacyOnStart: %v", err)
	}
	if tw.selectCalls != 0 || tw.updateCalls != 0 {
		t.Errorf("targets storage was touched: select=%d update=%d", tw.selectCalls, tw.updateCalls)
	}
	if pw.selectCalls != 0 || pw.insertCalls != 0 {
		t.Errorf("providers storage was touched: select=%d insert=%d", pw.selectCalls, pw.insertCalls)
	}
	if len(au.events) != 0 {
		t.Errorf("audit events emitted: %d, want 0", len(au.events))
	}
}

func TestAutoImporter_Targets_NullColumn_Imports(t *testing.T) {
	tw := newFakeTargetRW()
	tw.rows["soul-a.example.com"] = nil // ssh_target IS NULL
	pw := newFakeProviderRW()
	au := &fakeAuditor{}
	imp := newImporter(t, tw, pw, au)

	cfg := config.KeeperPush{
		AutoImportLegacyTargets: true,
		Targets: []config.KeeperPushTarget{
			{SID: "soul-a.example.com", SSHPort: 2222, SSHUser: "deploy", SoulPath: "/opt/bin/soul"},
		},
	}
	if err := imp.ImportLegacyOnStart(context.Background(), cfg); err != nil {
		t.Fatalf("ImportLegacyOnStart: %v", err)
	}
	got, ok := tw.rows["soul-a.example.com"]
	if !ok || got == nil {
		t.Fatalf("ssh_target not written for SID")
	}
	want := &soul.SSHTarget{SSHPort: 2222, SSHUser: "deploy", SoulPath: "/opt/bin/soul"}
	if *got != *want {
		t.Errorf("got = %+v, want = %+v", *got, *want)
	}
	if len(au.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(au.events))
	}
	ev := au.events[0]
	if ev.EventType != audit.EventSoulSshTargetImportedFromConfig {
		t.Errorf("event_type = %q", ev.EventType)
	}
	if ev.Source != audit.SourceConfigBootstrap {
		t.Errorf("source = %q, want config_bootstrap", ev.Source)
	}
	if ev.ArchonAID != "" {
		t.Errorf("archon_aid = %q, want empty (system-action)", ev.ArchonAID)
	}
	if ev.Payload["sid"] != "soul-a.example.com" || ev.Payload["ssh_port"] != 2222 {
		t.Errorf("payload mismatch: %+v", ev.Payload)
	}
}

func TestAutoImporter_Targets_Existing_Skip(t *testing.T) {
	tw := newFakeTargetRW()
	tw.rows["soul-a.example.com"] = &soul.SSHTarget{SSHPort: 22, SSHUser: "root", SoulPath: "/usr/local/bin/soul"}
	pw := newFakeProviderRW()
	au := &fakeAuditor{}
	imp := newImporter(t, tw, pw, au)

	cfg := config.KeeperPush{
		AutoImportLegacyTargets: true,
		Targets: []config.KeeperPushTarget{
			{SID: "soul-a.example.com", SSHPort: 9999, SSHUser: "override"},
		},
	}
	if err := imp.ImportLegacyOnStart(context.Background(), cfg); err != nil {
		t.Fatalf("ImportLegacyOnStart: %v", err)
	}
	if tw.updateCalls != 0 {
		t.Errorf("UpdateSshTarget called for an existing PG target -- should be skipped; calls = %d", tw.updateCalls)
	}
	// The PG row must not be overwritten.
	if tw.rows["soul-a.example.com"].SSHPort != 22 {
		t.Errorf("existing PG-row overwritten: %+v", tw.rows["soul-a.example.com"])
	}
	if len(au.events) != 0 {
		t.Errorf("audit emitted for skipped row: %d", len(au.events))
	}
}

func TestAutoImporter_Targets_MissingSoul_WarnSkip(t *testing.T) {
	tw := newFakeTargetRW()
	tw.missingSouls["soul-missing.example.com"] = struct{}{}
	pw := newFakeProviderRW()
	au := &fakeAuditor{}
	imp := newImporter(t, tw, pw, au)

	cfg := config.KeeperPush{
		AutoImportLegacyTargets: true,
		Targets:                 []config.KeeperPushTarget{{SID: "soul-missing.example.com"}},
	}
	if err := imp.ImportLegacyOnStart(context.Background(), cfg); err != nil {
		t.Fatalf("ImportLegacyOnStart: %v (must not be fatal)", err)
	}
	if tw.updateCalls != 0 {
		t.Errorf("UpdateSshTarget called for a missing souls row")
	}
	if len(au.events) != 0 {
		t.Errorf("audit emitted for skipped (missing souls row)")
	}
}

func TestAutoImporter_Targets_Idempotent(t *testing.T) {
	tw := newFakeTargetRW()
	tw.rows["soul-a.example.com"] = nil // first run: imports
	pw := newFakeProviderRW()
	au := &fakeAuditor{}
	imp := newImporter(t, tw, pw, au)

	cfg := config.KeeperPush{
		AutoImportLegacyTargets: true,
		Targets:                 []config.KeeperPushTarget{{SID: "soul-a.example.com", SSHPort: 22}},
	}
	if err := imp.ImportLegacyOnStart(context.Background(), cfg); err != nil {
		t.Fatalf("ImportLegacyOnStart run1: %v", err)
	}
	// Second run with the same data: the PG row is no longer NULL → no-op.
	updateCallsAfterRun1 := tw.updateCalls
	auditEventsAfterRun1 := len(au.events)

	if err := imp.ImportLegacyOnStart(context.Background(), cfg); err != nil {
		t.Fatalf("ImportLegacyOnStart run2: %v", err)
	}
	if tw.updateCalls != updateCallsAfterRun1 {
		t.Errorf("idempotency violated: extra UpdateSshTarget on run2 (%d → %d)",
			updateCallsAfterRun1, tw.updateCalls)
	}
	if len(au.events) != auditEventsAfterRun1 {
		t.Errorf("idempotency violated: extra audit on run2 (%d → %d)",
			auditEventsAfterRun1, len(au.events))
	}
}

func TestAutoImporter_Providers_NotInPG_Imports(t *testing.T) {
	tw := newFakeTargetRW()
	pw := newFakeProviderRW()
	au := &fakeAuditor{}
	imp := newImporter(t, tw, pw, au)

	cfg := config.KeeperPush{
		AutoImportLegacyProviders: true,
		Providers: []config.KeeperPushProvider{
			{Name: "vault-bastion", Params: map[string]any{
				"vault_addr": "https://vault.example",
				"role":       "keeper",
				"secret_id":  "vault:secret/keeper/approle/secret-id",
			}},
		},
	}
	if err := imp.ImportLegacyOnStart(context.Background(), cfg); err != nil {
		t.Fatalf("ImportLegacyOnStart: %v", err)
	}
	row, ok := pw.rows["vault-bastion"]
	if !ok {
		t.Fatalf("push_providers row not inserted")
	}
	if row.CreatedByAID != AutoImportSystemAID {
		t.Errorf("created_by_aid = %q, want %q", row.CreatedByAID, AutoImportSystemAID)
	}
	if len(au.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(au.events))
	}
	ev := au.events[0]
	if ev.EventType != audit.EventPushProviderImportedFromConfig {
		t.Errorf("event_type = %q", ev.EventType)
	}
	if ev.Source != audit.SourceConfigBootstrap {
		t.Errorf("source = %q", ev.Source)
	}
	keys, _ := ev.Payload["params_keys"].([]string)
	wantKeys := []string{"role", "secret_id", "vault_addr"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("params_keys = %v, want %v (sorted, values not leaked)", keys, wantKeys)
	}
	// Sanity: values for sensitive keys are NOT placed in the payload.
	for k := range ev.Payload {
		if k == "secret_id" || k == "password" || k == "token" || k == "private_key" {
			t.Errorf("audit payload leaks sensitive key value: %q", k)
		}
	}
}

func TestAutoImporter_Providers_Existing_Skip(t *testing.T) {
	tw := newFakeTargetRW()
	pw := newFakeProviderRW()
	pw.rows["vault"] = &pushprovider.PushProvider{Name: "vault", CreatedByAID: "archon-alice"}
	au := &fakeAuditor{}
	imp := newImporter(t, tw, pw, au)

	cfg := config.KeeperPush{
		AutoImportLegacyProviders: true,
		Providers:                 []config.KeeperPushProvider{{Name: "vault", Params: map[string]any{"role": "k"}}},
	}
	if err := imp.ImportLegacyOnStart(context.Background(), cfg); err != nil {
		t.Fatalf("ImportLegacyOnStart: %v", err)
	}
	if pw.insertCalls != 0 {
		t.Errorf("Insert called for an existing PG row (skip expected)")
	}
	if pw.rows["vault"].CreatedByAID != "archon-alice" {
		t.Errorf("existing PG-row created_by_aid changed: %+v", pw.rows["vault"])
	}
	if len(au.events) != 0 {
		t.Errorf("audit emitted for skipped row: %d", len(au.events))
	}
}

func TestAutoImporter_Targets_PGReadError_Propagates(t *testing.T) {
	tw := newFakeTargetRW()
	tw.selectErr = errors.New("pg unavailable")
	pw := newFakeProviderRW()
	au := &fakeAuditor{}
	imp := newImporter(t, tw, pw, au)

	cfg := config.KeeperPush{
		AutoImportLegacyTargets: true,
		Targets:                 []config.KeeperPushTarget{{SID: "soul-a.example.com"}},
	}
	err := imp.ImportLegacyOnStart(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error on PG read failure, got nil")
	}
}

func TestNewAutoImporter_NilDeps(t *testing.T) {
	logger := slog.Default()
	auditor := &fakeAuditor{}
	cases := []struct {
		name string
		deps AutoImporterDeps
	}{
		{"nil-target-reader", AutoImporterDeps{TargetWriter: newFakeTargetRW(), ProviderReader: newFakeProviderRW(), ProviderWriter: newFakeProviderRW(), Auditor: auditor, Logger: logger}},
		{"nil-target-writer", AutoImporterDeps{TargetReader: newFakeTargetRW(), ProviderReader: newFakeProviderRW(), ProviderWriter: newFakeProviderRW(), Auditor: auditor, Logger: logger}},
		{"nil-provider-reader", AutoImporterDeps{TargetReader: newFakeTargetRW(), TargetWriter: newFakeTargetRW(), ProviderWriter: newFakeProviderRW(), Auditor: auditor, Logger: logger}},
		{"nil-provider-writer", AutoImporterDeps{TargetReader: newFakeTargetRW(), TargetWriter: newFakeTargetRW(), ProviderReader: newFakeProviderRW(), Auditor: auditor, Logger: logger}},
		{"nil-auditor", AutoImporterDeps{TargetReader: newFakeTargetRW(), TargetWriter: newFakeTargetRW(), ProviderReader: newFakeProviderRW(), ProviderWriter: newFakeProviderRW(), Logger: logger}},
		{"nil-logger", AutoImporterDeps{TargetReader: newFakeTargetRW(), TargetWriter: newFakeTargetRW(), ProviderReader: newFakeProviderRW(), ProviderWriter: newFakeProviderRW(), Auditor: auditor}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewAutoImporter(c.deps); err == nil {
				t.Errorf("expected error on %s, got nil", c.name)
			}
		})
	}
}

func TestAutoImporter_AuditWriteFailure_NotFatal(t *testing.T) {
	tw := newFakeTargetRW()
	tw.rows["soul-a.example.com"] = nil
	pw := newFakeProviderRW()
	au := &fakeAuditor{writeErr: errors.New("audit pg down")}
	imp := newImporter(t, tw, pw, au)

	cfg := config.KeeperPush{
		AutoImportLegacyTargets: true,
		Targets:                 []config.KeeperPushTarget{{SID: "soul-a.example.com", SSHPort: 22}},
	}
	// Audit write failure must be best-effort: storage is already committed,
	// import continues without error.
	if err := imp.ImportLegacyOnStart(context.Background(), cfg); err != nil {
		t.Fatalf("ImportLegacyOnStart: %v (audit-fail must not be fatal)", err)
	}
	if tw.updateCalls != 1 {
		t.Errorf("UpdateSshTarget calls = %d, want 1", tw.updateCalls)
	}
}
