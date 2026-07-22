package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// mockWriter is a test [audit.Writer] for checking audit emission from
// [Store.Reload]. Captures events under a mutex for safe reads from tests with
// goroutines (WatchSIGHUP).
type mockWriter struct {
	mu     sync.Mutex
	events []*audit.Event
	err    error
}

func (m *mockWriter) Write(_ context.Context, ev *audit.Event) error {
	m.mu.Lock()
	m.events = append(m.events, ev)
	m.mu.Unlock()
	return m.err
}

func (m *mockWriter) snapshot() []*audit.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*audit.Event, len(m.events))
	copy(out, m.events)
	return out
}

func (m *mockWriter) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

// fixtureKeeperPath copies the golden keeper.yml into a temp file the test can
// edit. Returns the absolute path.
func fixtureKeeperPath(t *testing.T) string {
	t.Helper()
	src := filepath.FromSlash("../../examples/keeper/keeper.yml")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "keeper.yml")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dst
}

func TestLoadKeeperStore_GoldenInitial(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, diags, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	if diag.HasErrors(diags) {
		for _, d := range diags {
			t.Logf("%s [%s] %s", d.Code, d.Phase, d.Message)
		}
		t.Fatalf("expected 0 errors, got %d diagnostics", len(diags))
	}
	cfg := store.Get()
	if cfg == nil {
		t.Fatal("Get() == nil after successful initial load")
	}
	if cfg.KID != "keeper-eu-west-01" {
		t.Errorf("KID: got %q, want keeper-eu-west-01", cfg.KID)
	}
	if store.Document() == nil {
		t.Error("Document() == nil")
	}
	if store.Path() != path {
		t.Errorf("Path() = %q, want %q", store.Path(), path)
	}
}

func TestStore_ReloadNoOp(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	before := store.Get()

	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false on no-op reload, diags=%+v", res.Diagnostics)
	}
	if res.Source != ReloadSourceAPI {
		t.Errorf("Source = %q, want api", res.Source)
	}
	if res.Phase != "" {
		t.Errorf("Phase = %q on success, want empty", res.Phase)
	}
	if res.CorrelationID == "" {
		t.Error("CorrelationID empty on success")
	}
	if res.Timestamp.IsZero() {
		t.Error("Timestamp zero")
	}

	after := store.Get()
	if after == nil {
		t.Fatal("Get() nil after reload")
	}
	if after.KID != before.KID {
		t.Errorf("KID changed: before=%q after=%q", before.KID, after.KID)
	}
	// A swap happened anyway (re-parsed) — the pointer is new.
	if after == before {
		t.Log("note: same pointer after reload — possible but not required")
	}
}

func TestStore_ReloadAfterExternalEdit(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	if store.Get().KID != "keeper-eu-west-01" {
		t.Fatalf("initial KID wrong: %q", store.Get().KID)
	}

	// A direct write to disk simulates an external editor / API-edit; an AST Patch
	// is not needed to check the Reload contract.
	newSrc := []byte(`kid: keeper-eu-west-99
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /a, key: /b } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /a, key: /b, ca: /c } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 5, max: 50 }
redis:
  addr: "redis:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://vault:8200"
  auth: { method: token }
  pki_mount: "pki/soulstack"
logging:
  level: info
  format: json
`)
	if err := os.WriteFile(path, newSrc, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		for _, d := range res.Diagnostics {
			t.Logf("diag: %s [%s/%s] %s", d.Code, d.Phase, d.Level, d.Message)
		}
		t.Fatalf("Swapped=false on valid edit")
	}
	if got := store.Get().KID; got != "keeper-eu-west-99" {
		t.Errorf("KID after reload = %q, want keeper-eu-west-99", got)
	}
}

func TestStore_ReloadFailureKeepsOldSnapshot(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	prev := store.Get()
	if prev == nil {
		t.Fatal("initial Get() nil")
	}
	prevKID := prev.KID

	// Broken YAML — a guaranteed parse-fail.
	bad := []byte("kid: [\n  unterminated\n")
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if res.Swapped {
		t.Fatalf("Swapped=true on broken YAML; want false")
	}
	if res.Phase != diag.PhaseParse {
		t.Errorf("Phase = %q, want %q", res.Phase, diag.PhaseParse)
	}
	if !diag.HasErrors(res.Diagnostics) {
		t.Errorf("no error diagnostics in failure result")
	}
	after := store.Get()
	if after == nil {
		t.Fatal("Get() nil after failed reload — must keep old snapshot")
	}
	if after.KID != prevKID {
		t.Errorf("KID changed on failed reload: was %q, now %q", prevKID, after.KID)
	}
}

func TestStore_ConcurrentReadersDuringReload(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					cfg := store.Get()
					if cfg == nil {
						t.Errorf("Get() returned nil under concurrent reload")
						return
					}
					if cfg.KID == "" {
						t.Errorf("Get() returned config with empty KID")
						return
					}
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			res := store.Reload(context.Background(), ReloadSourceAPI)
			if !res.Swapped {
				t.Errorf("Reload not swapped at iteration %d", i)
				return
			}
		}
	}()

	// Give the reloader 50 iterations.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestStore_CorrelationIDUnique(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	a := store.Reload(context.Background(), ReloadSourceAPI)
	b := store.Reload(context.Background(), ReloadSourceAPI)
	if a.CorrelationID == "" || b.CorrelationID == "" {
		t.Fatal("CorrelationID empty")
	}
	if a.CorrelationID == b.CorrelationID {
		t.Errorf("CorrelationID collision: %q", a.CorrelationID)
	}
	// ULID — 26 Crockford base32 chars (ADR-022(c)).
	ulidRe := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	if !ulidRe.MatchString(a.CorrelationID) {
		t.Errorf("CorrelationID %q does not match ULID pattern %s", a.CorrelationID, ulidRe)
	}
	if !ulidRe.MatchString(b.CorrelationID) {
		t.Errorf("CorrelationID %q does not match ULID pattern %s", b.CorrelationID, ulidRe)
	}
}

func TestWatchSIGHUP_FireAndDelivery(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := WatchSIGHUP(ctx, store)

	// Give the watcher time to register signal.Notify.
	time.Sleep(20 * time.Millisecond)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("kill SIGHUP: %v", err)
	}

	select {
	case res, ok := <-out:
		if !ok {
			t.Fatal("channel closed before delivering result")
		}
		if res.Source != ReloadSourceSignal {
			t.Errorf("Source = %q, want signal", res.Source)
		}
		if !res.Swapped {
			t.Errorf("Swapped=false on golden fixture reload")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no ReloadResult received within timeout")
	}
}

func TestWatchSIGHUP_ContextCancelClosesChannel(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	out := WatchSIGHUP(ctx, store)
	time.Sleep(20 * time.Millisecond)

	cancel()

	select {
	case _, ok := <-out:
		if ok {
			// One last result may arrive — that's allowed, but the next read
			// must get closed.
			select {
			case _, ok2 := <-out:
				if ok2 {
					t.Fatal("channel not closed after ctx cancel")
				}
			case <-time.After(time.Second):
				t.Fatal("channel not closed after ctx cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed within timeout after ctx cancel")
	}
}

func TestWatchSIGHUP_BufferedChannelDoesntBlock(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := WatchSIGHUP(ctx, store)
	time.Sleep(20 * time.Millisecond)

	// Intentionally do NOT read from out. Send SIGHUP several times — the handler
	// must not get stuck. If it blocked, the ctx.Cancel() below would not close
	// the channel in reasonable time.
	for i := 0; i < 5; i++ {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatalf("kill SIGHUP: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Read the single buffered result.
	select {
	case res, ok := <-out:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		if res.Source != ReloadSourceSignal {
			t.Errorf("Source = %q, want signal", res.Source)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result in channel — handler likely deadlocked")
	}

	// Extra SIGHUPs passed by the full channel — checking the watcher is alive
	// and the channel closes correctly on cancel.
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-out:
			if !ok {
				return // success — channel closed
			}
		case <-deadline:
			t.Fatal("channel not closed after ctx cancel — handler stuck")
		}
	}
}

// fixtureSoulPath is the analog of `fixtureKeeperPath` for the golden `soul.yml`.
func fixtureSoulPath(t *testing.T) string {
	t.Helper()
	src := filepath.FromSlash("../../examples/soul/soul.yml")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "soul.yml")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dst
}

func TestLoadSoulStore_GoldenInitial(t *testing.T) {
	path := fixtureSoulPath(t)
	store, diags, err := LoadSoulStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulStore: %v", err)
	}
	if diag.HasErrors(diags) {
		for _, d := range diags {
			t.Logf("%s [%s] %s", d.Code, d.Phase, d.Message)
		}
		t.Fatalf("expected 0 errors, got %d diagnostics", len(diags))
	}
	cfg := store.Get()
	if cfg == nil {
		t.Fatal("Get() == nil after successful initial load")
	}
	// SID is optional (auto-detected via FQDN); the fixture does not set it, we
	// check that the endpoints list arrived.
	if got := len(cfg.Keeper.Endpoints); got != 5 {
		t.Errorf("Keeper.Endpoints len = %d, want 5", got)
	}
	if got := cfg.Keeper.Endpoints[0].EventStreamAddr(); got != "k1.dc1.example:9443" {
		t.Errorf("Keeper.Endpoints[0].EventStreamAddr() = %q, want k1.dc1.example:9443", got)
	}
	if got := cfg.Keeper.Endpoints[0].BootstrapAddr(); got != "k1.dc1.example:9442" {
		t.Errorf("Keeper.Endpoints[0].BootstrapAddr() = %q, want k1.dc1.example:9442", got)
	}
	if store.Document() == nil {
		t.Error("Document() == nil")
	}
	if store.Path() != path {
		t.Errorf("Path() = %q, want %q", store.Path(), path)
	}
}

// Reload on a deleted file — I/O error, snapshot preserved.
func TestStore_ReloadOnDeletedFile(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	prev := store.Get()
	if prev == nil {
		t.Fatal("initial Get() nil")
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if res.Swapped {
		t.Fatalf("Swapped=true on deleted file; want false")
	}
	if res.Phase != diag.PhaseParse {
		t.Errorf("Phase = %q, want %q", res.Phase, diag.PhaseParse)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != "io_error" {
		t.Errorf("want single io_error diagnostic, got %+v", res.Diagnostics)
	}
	if got := store.Get(); got == nil || got.KID != prev.KID {
		t.Errorf("snapshot lost on failed reload: prev=%v got=%v", prev, got)
	}
}

// Reload on a schema error (unknown top-level key) — snapshot preserved,
// Phase = schema_validate.
func TestStore_ReloadFailsOnSchemaError(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	// Take the golden and append an unknown top-level key.
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	corrupt := append(orig, []byte("\nzzz_unknown_top_level: 1\n")...)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if res.Swapped {
		t.Fatalf("Swapped=true on schema error; want false, diags=%+v", res.Diagnostics)
	}
	if res.Phase != diag.PhaseSchemaValidate {
		t.Errorf("Phase = %q, want %q", res.Phase, diag.PhaseSchemaValidate)
	}
	if !diag.HasErrors(res.Diagnostics) {
		t.Errorf("no error diagnostics in failure result")
	}
}

// Reload on a semantic error (invalid KID) — snapshot preserved,
// Phase = semantic_validate.
func TestStore_ReloadFailsOnSemanticError(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// KID must be `^[a-z][a-z0-9-]{0,62}$`, uppercase is forbidden.
	corrupt := bytes.Replace(orig, []byte("kid: keeper-eu-west-01"), []byte("kid: KEEPER_BAD"), 1)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if res.Swapped {
		t.Fatalf("Swapped=true on semantic error; want false, diags=%+v", res.Diagnostics)
	}
	if res.Phase != diag.PhaseSemanticValidate {
		t.Errorf("Phase = %q, want %q", res.Phase, diag.PhaseSemanticValidate)
	}
}

// Initial load with a validation error returns a live Store: Get()==nil,
// Document()!=nil, and after fixing the file Reload restores Get().
func TestLoadKeeperStore_InitialValidationErrorKeepsStoreLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keeper.yml")
	// Broken: only the postgres marker, the other required blocks are absent.
	bad := []byte("postgres: {}\n")
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	store, diags, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	if store == nil {
		t.Fatal("Store == nil on validation-error initial load (must be live for Reload)")
	}
	if !diag.HasErrors(diags) {
		t.Fatal("expected error diagnostics on broken initial load")
	}
	if store.Get() != nil {
		t.Error("Get() != nil on broken initial load")
	}
	if store.Document() == nil {
		t.Error("Document() == nil on broken initial load (must be non-nil for re-Reload)")
	}

	// Swap the file for the golden, do Reload — Get() must come alive.
	goldenSrc, err := os.ReadFile(filepath.FromSlash("../../examples/keeper/keeper.yml"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if err := os.WriteFile(path, goldenSrc, 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false after fixing file, diags=%+v", res.Diagnostics)
	}
	if store.Get() == nil {
		t.Fatal("Get() == nil after successful repair-Reload")
	}
}

// Document() after a failed Reload — the old AST, not nil and not new.
func TestStore_DocumentAfterFailedReloadStaysOld(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	docBefore := store.Document()
	if docBefore == nil {
		t.Fatal("Document() nil after successful load")
	}

	bad := []byte("kid: [\n  unterminated\n")
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if res.Swapped {
		t.Fatalf("Swapped=true on broken YAML; want false")
	}
	docAfter := store.Document()
	if docAfter == nil {
		t.Fatal("Document() nil after failed reload")
	}
	if docAfter != docBefore {
		t.Errorf("Document() changed on failed reload: was %p, now %p", docBefore, docAfter)
	}
}

// Empty-file Reload — must flag a parse error with code empty_document and not
// panic (regression for qa.1 blocker bug 1).
func TestStore_ReloadOnEmptyFileDoesNotPanic(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	for _, content := range [][]byte{nil, []byte(""), []byte("   \n\t  \n")} {
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		res := store.Reload(context.Background(), ReloadSourceSignal)
		if res.Swapped {
			t.Fatalf("Swapped=true on empty file content=%q", content)
		}
		if res.Phase != diag.PhaseParse {
			t.Errorf("Phase = %q, want %q (content=%q)", res.Phase, diag.PhaseParse, content)
		}
		var foundCode string
		for _, d := range res.Diagnostics {
			if d.Level == diag.LevelError {
				foundCode = d.Code
				break
			}
		}
		if foundCode != "empty_document" {
			t.Errorf("want diagnostic code empty_document on content=%q, got %q (all=%+v)",
				content, foundCode, res.Diagnostics)
		}
	}
}

// nil-store in WatchSIGHUP — panic in the caller's stack, before the goroutine
// starts (regression for qa.1 blocker bug 2).
func TestWatchSIGHUP_NilStorePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("WatchSIGHUP(nil) did not panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not string: %T %v", r, r)
		}
		if !strings.Contains(msg, "store is nil") {
			t.Errorf("panic message = %q, want substring %q", msg, "store is nil")
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = WatchSIGHUP[KeeperConfig](ctx, nil)
}

// TestStore_ReloadEmitsAuditOnSuccess — a successful Reload produces one
// audit event of type `config.reload_succeeded` with the correct Source and the
// same CorrelationID as in [ReloadResult].
func TestStore_ReloadEmitsAuditOnSuccess(t *testing.T) {
	path := fixtureKeeperPath(t)
	w := &mockWriter{}
	store, _, err := LoadKeeperStoreWithAudit(path, ValidateOptions{}, w)
	if err != nil {
		t.Fatalf("LoadKeeperStoreWithAudit: %v", err)
	}

	for _, src := range []ReloadSource{ReloadSourceAPI, ReloadSourceMCP, ReloadSourceSignal} {
		res := store.Reload(context.Background(), src)
		if !res.Swapped {
			t.Fatalf("Swapped=false on success reload (source=%s)", src)
		}
		evs := w.snapshot()
		if len(evs) == 0 {
			t.Fatalf("no audit event emitted for source=%s", src)
		}
		last := evs[len(evs)-1]
		if last.EventType != audit.EventConfigReloadSucceeded {
			t.Errorf("EventType = %q, want %q", last.EventType, audit.EventConfigReloadSucceeded)
		}
		if last.Source != src {
			t.Errorf("Source = %q, want %q", last.Source, src)
		}
		if last.CorrelationID != res.CorrelationID {
			t.Errorf("CorrelationID mismatch: event=%q result=%q", last.CorrelationID, res.CorrelationID)
		}
		if !last.CreatedAt.Equal(res.Timestamp) {
			t.Errorf("CreatedAt = %v, want %v", last.CreatedAt, res.Timestamp)
		}
		if last.AuditID == "" {
			t.Errorf("AuditID empty")
		}
		if got, want := last.Payload["path"], path; got != want {
			t.Errorf("Payload[path] = %v, want %q", got, want)
		}
		if _, ok := last.Payload["phase"]; ok {
			t.Errorf("Payload[phase] present on success: %v", last.Payload)
		}
		if _, ok := last.Payload["validation_errors"]; ok {
			t.Errorf("Payload[validation_errors] present on success: %v", last.Payload)
		}
		if _, ok := last.Payload["changed_paths"]; ok {
			// ChangedPaths is empty in M0.3 — the key must be absent.
			t.Errorf("Payload[changed_paths] present when empty: %v", last.Payload)
		}
	}
}

// TestStore_ReloadEmitsAuditOnFailure — a validation failure (broken YAML)
// produces an audit event `config.reload_failed` with phase + validation_errors.
func TestStore_ReloadEmitsAuditOnFailure(t *testing.T) {
	path := fixtureKeeperPath(t)
	w := &mockWriter{}
	store, _, err := LoadKeeperStoreWithAudit(path, ValidateOptions{}, w)
	if err != nil {
		t.Fatalf("LoadKeeperStoreWithAudit: %v", err)
	}

	// Schema-fail: unknown top-level key — Phase = schema_validate.
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	corrupt := append(orig, []byte("\nzzz_unknown_top_level: 1\n")...)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if res.Swapped {
		t.Fatalf("Swapped=true on schema error")
	}

	evs := w.snapshot()
	if len(evs) != 1 {
		t.Fatalf("event count = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.EventType != audit.EventConfigReloadFailed {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventConfigReloadFailed)
	}
	if ev.Source != ReloadSourceSignal {
		t.Errorf("Source = %q, want signal", ev.Source)
	}
	if ev.CorrelationID != res.CorrelationID {
		t.Errorf("CorrelationID mismatch: event=%q result=%q", ev.CorrelationID, res.CorrelationID)
	}
	if ev.Payload["path"] != path {
		t.Errorf("Payload[path] = %v, want %q", ev.Payload["path"], path)
	}
	if ev.Payload["phase"] != string(diag.PhaseSchemaValidate) {
		t.Errorf("Payload[phase] = %v, want %q", ev.Payload["phase"], diag.PhaseSchemaValidate)
	}
	ve, ok := ev.Payload["validation_errors"].([]map[string]any)
	if !ok {
		t.Fatalf("Payload[validation_errors] type = %T, want []map[string]any", ev.Payload["validation_errors"])
	}
	if len(ve) == 0 {
		t.Fatalf("validation_errors empty on schema-fail")
	}
	// Check the mandatory keys per ADR-022(j) are present.
	for _, key := range []string{"code", "message", "phase", "level"} {
		if _, ok := ve[0][key]; !ok {
			t.Errorf("validation_errors[0] missing key %q: %#v", key, ve[0])
		}
	}
}

// TestStore_ReloadEmitsAuditOnIOFatal — a deleted file produces
// `config.reload_failed` with Phase=parse and validation_errors with io_error.
func TestStore_ReloadEmitsAuditOnIOFatal(t *testing.T) {
	path := fixtureKeeperPath(t)
	w := &mockWriter{}
	store, _, err := LoadKeeperStoreWithAudit(path, ValidateOptions{}, w)
	if err != nil {
		t.Fatalf("LoadKeeperStoreWithAudit: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if res.Swapped {
		t.Fatalf("Swapped=true on missing file")
	}

	evs := w.snapshot()
	if len(evs) != 1 {
		t.Fatalf("event count = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.EventType != audit.EventConfigReloadFailed {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventConfigReloadFailed)
	}
	if ev.Payload["phase"] != string(diag.PhaseParse) {
		t.Errorf("Payload[phase] = %v, want %q", ev.Payload["phase"], diag.PhaseParse)
	}
	ve, ok := ev.Payload["validation_errors"].([]map[string]any)
	if !ok {
		t.Fatalf("Payload[validation_errors] type = %T, want []map[string]any", ev.Payload["validation_errors"])
	}
	if len(ve) != 1 {
		t.Fatalf("validation_errors len = %d, want 1", len(ve))
	}
	if ve[0]["code"] != "io_error" {
		t.Errorf("validation_errors[0].code = %v, want io_error", ve[0]["code"])
	}
}

// TestStore_ReloadWithoutAuditWriter_NoEmit — the standard LoadKeeperStore
// (no audit) Reload works correctly, in the Store auditWriter==nil, and no audit
// emission happens.
func TestStore_ReloadWithoutAuditWriter_NoEmit(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	if store.auditWriter != nil {
		t.Errorf("auditWriter != nil for LoadKeeperStore (no audit) constructor")
	}

	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false on golden Reload")
	}
	// Extra indirect check: a failure-Reload also does not crash without a writer.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	res2 := store.Reload(context.Background(), ReloadSourceSignal)
	if res2.Swapped {
		t.Fatalf("Swapped=true on missing file")
	}
}

// TestStore_SetAuditWriter_LateBindingEmits — the Store is created without audit
// (LoadKeeperStore), a reload before SetAuditWriter does not emit; after
// late-binding SetAuditWriter every reload writes config.reload_*. This is the
// `keeper` binary's init-phase path (the Store is created before the audit-writer
// comes up).
func TestStore_SetAuditWriter_LateBindingEmits(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	// Before injection — reload does not emit.
	if res := store.Reload(context.Background(), ReloadSourceSignal); !res.Swapped {
		t.Fatalf("Swapped=false before SetAuditWriter")
	}

	w := &mockWriter{}
	store.SetAuditWriter(w)

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if !res.Swapped {
		t.Fatalf("Swapped=false after SetAuditWriter")
	}
	evs := w.snapshot()
	if len(evs) != 1 {
		t.Fatalf("event count = %d, want 1 (only post-SetAuditWriter reload)", len(evs))
	}
	if evs[0].EventType != audit.EventConfigReloadSucceeded {
		t.Errorf("EventType = %q, want %q", evs[0].EventType, audit.EventConfigReloadSucceeded)
	}
	if evs[0].CorrelationID != res.CorrelationID {
		t.Errorf("CorrelationID mismatch: event=%q result=%q", evs[0].CorrelationID, res.CorrelationID)
	}

	// nil reset — no emission again (back-compat).
	store.SetAuditWriter(nil)
	if res := store.Reload(context.Background(), ReloadSourceSignal); !res.Swapped {
		t.Fatalf("Swapped=false after SetAuditWriter(nil)")
	}
	if w.len() != 1 {
		t.Errorf("writer calls = %d after SetAuditWriter(nil), want 1 (no further emit)", w.len())
	}
}

// TestStore_AuditWriterError_DoesNotBlockReload — the Writer returns an error,
// Reload still returns a correct ReloadResult with Swapped=true.
func TestStore_AuditWriterError_DoesNotBlockReload(t *testing.T) {
	path := fixtureKeeperPath(t)
	w := &mockWriter{err: errors.New("audit backend down")}
	store, _, err := LoadKeeperStoreWithAudit(path, ValidateOptions{}, w)
	if err != nil {
		t.Fatalf("LoadKeeperStoreWithAudit: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false on success reload despite audit-write fail")
	}
	if res.CorrelationID == "" {
		t.Errorf("CorrelationID empty on success despite audit-write fail")
	}
	// The Writer was still called — it just returned an error.
	if w.len() != 1 {
		t.Errorf("writer calls = %d, want 1 (write attempted, error logged)", w.len())
	}
}

// TestStore_ReloadAuditCorrelationIDConsistency — two consecutive Reloads have
// different CorrelationIDs, and each matches the one the audit event received.
func TestStore_ReloadAuditCorrelationIDConsistency(t *testing.T) {
	path := fixtureKeeperPath(t)
	w := &mockWriter{}
	store, _, err := LoadKeeperStoreWithAudit(path, ValidateOptions{}, w)
	if err != nil {
		t.Fatalf("LoadKeeperStoreWithAudit: %v", err)
	}

	a := store.Reload(context.Background(), ReloadSourceAPI)
	b := store.Reload(context.Background(), ReloadSourceMCP)

	if a.CorrelationID == b.CorrelationID {
		t.Errorf("CorrelationID collision across reloads: %q", a.CorrelationID)
	}

	evs := w.snapshot()
	if len(evs) != 2 {
		t.Fatalf("event count = %d, want 2", len(evs))
	}
	if evs[0].CorrelationID != a.CorrelationID {
		t.Errorf("event[0].CorrelationID = %q, want %q", evs[0].CorrelationID, a.CorrelationID)
	}
	if evs[1].CorrelationID != b.CorrelationID {
		t.Errorf("event[1].CorrelationID = %q, want %q", evs[1].CorrelationID, b.CorrelationID)
	}
}

// TestStore_LoadSoulStoreWithAudit_EmitsOnReload — the symmetric test for the
// Soul constructor variant (needed for regression: both `kind`s must emit audit).
func TestStore_LoadSoulStoreWithAudit_EmitsOnReload(t *testing.T) {
	path := fixtureSoulPath(t)
	w := &mockWriter{}
	store, _, err := LoadSoulStoreWithAudit(path, ValidateOptions{}, w)
	if err != nil {
		t.Fatalf("LoadSoulStoreWithAudit: %v", err)
	}

	res := store.Reload(context.Background(), ReloadSourceSignal)
	if !res.Swapped {
		t.Fatalf("Swapped=false on golden soul.yml reload")
	}
	if w.len() != 1 {
		t.Fatalf("event count = %d, want 1", w.len())
	}
	ev := w.snapshot()[0]
	if ev.EventType != audit.EventConfigReloadSucceeded {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventConfigReloadSucceeded)
	}
}

// TestStore_LoadKeeperStoreWithAudit_NilWriterIsBackwardCompat — passing a
// nil-writer must not break the constructor and must behave like LoadKeeperStore
// without audit emission.
func TestStore_LoadKeeperStoreWithAudit_NilWriterIsBackwardCompat(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStoreWithAudit(path, ValidateOptions{}, nil)
	if err != nil {
		t.Fatalf("LoadKeeperStoreWithAudit(nil): %v", err)
	}
	if store.auditWriter != nil {
		t.Errorf("auditWriter != nil for nil-writer constructor")
	}
	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false on nil-writer Reload")
	}
}

// TestWatchSIGHUP_EmitsAuditOnReload — end-to-end: SIGHUP → WatchSIGHUP triggers
// Reload → Store.auditWriter receives the event.
func TestWatchSIGHUP_EmitsAuditOnReload(t *testing.T) {
	path := fixtureKeeperPath(t)
	w := &mockWriter{}
	store, _, err := LoadKeeperStoreWithAudit(path, ValidateOptions{}, w)
	if err != nil {
		t.Fatalf("LoadKeeperStoreWithAudit: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := WatchSIGHUP(ctx, store)

	time.Sleep(20 * time.Millisecond)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("kill SIGHUP: %v", err)
	}

	select {
	case res, ok := <-out:
		if !ok {
			t.Fatal("channel closed before delivering result")
		}
		if !res.Swapped {
			t.Fatalf("Swapped=false on SIGHUP reload")
		}
		// audit-write is best-effort, done inside Reload before the result is
		// returned to the channel, so by read time the event is already there.
		evs := w.snapshot()
		if len(evs) != 1 {
			t.Fatalf("event count = %d, want 1", len(evs))
		}
		if evs[0].Source != ReloadSourceSignal {
			t.Errorf("Source = %q, want signal", evs[0].Source)
		}
		if evs[0].CorrelationID != res.CorrelationID {
			t.Errorf("CorrelationID mismatch: event=%q result=%q", evs[0].CorrelationID, res.CorrelationID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no ReloadResult received within timeout")
	}
}

// TestStore_OnReload_FiresOnSwap — a successful Reload calls the callback exactly
// once; old/new contain the corresponding snapshot pointers.
func TestStore_OnReload_FiresOnSwap(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	type capture struct {
		old, new *KeeperConfig
	}
	ch := make(chan capture, 4)
	unsub := store.OnReload(func(old, new *KeeperConfig) {
		ch <- capture{old: old, new: new}
	})
	defer unsub()

	before := store.Get()
	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false on golden Reload, diags=%+v", res.Diagnostics)
	}
	after := store.Get()

	select {
	case got := <-ch:
		if got.old != before {
			t.Errorf("callback old = %p, want %p (snapshot before reload)", got.old, before)
		}
		if got.new != after {
			t.Errorf("callback new = %p, want %p (snapshot after reload)", got.new, after)
		}
		if got.new == nil {
			t.Error("callback new is nil on Swapped=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback not invoked within 2s")
	}
}

// TestStore_OnReload_NotCalledOnFailure — a validation failure and an I/O-fatal
// must not trigger the callback (the old snapshot does not change).
func TestStore_OnReload_NotCalledOnFailure(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	var called atomic.Int32
	unsub := store.OnReload(func(_, _ *KeeperConfig) {
		called.Add(1)
	})
	defer unsub()

	// Broken YAML — parse-fail.
	bad := []byte("kid: [\n  unterminated\n")
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res := store.Reload(context.Background(), ReloadSourceSignal)
	if res.Swapped {
		t.Fatalf("Swapped=true on broken YAML")
	}

	// I/O-fatal — a deleted file.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	res2 := store.Reload(context.Background(), ReloadSourceSignal)
	if res2.Swapped {
		t.Fatalf("Swapped=true on missing file")
	}

	// Give the notify-goroutines (which should not exist) a chance to fire.
	time.Sleep(50 * time.Millisecond)
	if got := called.Load(); got != 0 {
		t.Errorf("callback invoked %d times on failed reloads; want 0", got)
	}
}

// TestStore_OnReload_Unsubscribe — after unsub the callback is no longer called;
// a repeated unsub is a no-op.
func TestStore_OnReload_Unsubscribe(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	var called atomic.Int32
	unsub := store.OnReload(func(_, _ *KeeperConfig) {
		called.Add(1)
	})

	// First Reload — the callback must fire.
	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && called.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := called.Load(); got != 1 {
		t.Fatalf("first reload callback count = %d, want 1", got)
	}

	unsub()
	// Repeated unsub — idempotent, must not panic.
	unsub()

	res = store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false on second reload")
	}
	time.Sleep(50 * time.Millisecond)
	if got := called.Load(); got != 1 {
		t.Errorf("after unsubscribe callback was invoked again: count=%d, want 1", got)
	}
}

// TestStore_OnReload_MultipleSubscribers — multiple subscribers are notified
// independently; a slow subscriber does not block the others.
func TestStore_OnReload_MultipleSubscribers(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	fast := make(chan struct{}, 1)
	slow := make(chan struct{}, 1)
	unsubFast := store.OnReload(func(_, _ *KeeperConfig) {
		fast <- struct{}{}
	})
	defer unsubFast()
	unsubSlow := store.OnReload(func(_, _ *KeeperConfig) {
		time.Sleep(100 * time.Millisecond)
		slow <- struct{}{}
	})
	defer unsubSlow()

	start := time.Now()
	res := store.Reload(context.Background(), ReloadSourceAPI)
	elapsed := time.Since(start)
	if !res.Swapped {
		t.Fatalf("Swapped=false")
	}
	// Reload must not block on the slow subscriber — parallel goroutines. 100 ms
	// is the slow-subscriber threshold; Reload must return noticeably earlier
	// (we budget 50 ms for CI).
	if elapsed > 50*time.Millisecond {
		t.Errorf("Reload blocked by slow subscriber: elapsed=%v", elapsed)
	}

	select {
	case <-fast:
	case <-time.After(2 * time.Second):
		t.Fatal("fast subscriber not invoked")
	}
	select {
	case <-slow:
	case <-time.After(2 * time.Second):
		t.Fatal("slow subscriber not invoked")
	}
}

// TestStore_OnReload_PanicIsolated — a panic in one subscriber must not crash the
// process and must not wipe out calls to other subscribers.
func TestStore_OnReload_PanicIsolated(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	var goodCalled atomic.Int32
	unsubBad := store.OnReload(func(_, _ *KeeperConfig) {
		panic("subscriber panic")
	})
	defer unsubBad()
	unsubGood := store.OnReload(func(_, _ *KeeperConfig) {
		goodCalled.Add(1)
	})
	defer unsubGood()

	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && goodCalled.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := goodCalled.Load(); got != 1 {
		t.Errorf("good subscriber count = %d; want 1 (panic in sibling must not affect)", got)
	}
}

// TestStore_OnReload_UnsubscribeFromCallback — calling unsubscribe from within
// the callback itself must not deadlock (RWMutex + notify under RLock).
func TestStore_OnReload_UnsubscribeFromCallback(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	var called atomic.Int32
	var unsub func()
	done := make(chan struct{}, 1)
	unsub = store.OnReload(func(_, _ *KeeperConfig) {
		called.Add(1)
		unsub()
		done <- struct{}{}
	})

	res := store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not finish — possible deadlock on unsubscribe from callback")
	}

	// Second Reload — the callback is already unsubscribed, count must not grow.
	res = store.Reload(context.Background(), ReloadSourceAPI)
	if !res.Swapped {
		t.Fatalf("Swapped=false on second reload")
	}
	time.Sleep(50 * time.Millisecond)
	if got := called.Load(); got != 1 {
		t.Errorf("callback count = %d after self-unsubscribe; want 1", got)
	}
}

// TestStore_OnReload_NilCallbackPanics — a programming error by the caller.
func TestStore_OnReload_NilCallbackPanics(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("OnReload(nil) did not panic")
		}
	}()
	store.OnReload(nil)
}

// TestStore_OnReload_ConcurrentSubscribeAndReload — a race-resistant check under
// -race: concurrent OnReload/unsubscribe + Reload.
func TestStore_OnReload_ConcurrentSubscribeAndReload(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Subscriber-churn goroutine: constantly subscribes/unsubscribes.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					u := store.OnReload(func(_, _ *KeeperConfig) {})
					u()
				}
			}
		}()
	}

	// A persistent subscriber counting calls.
	var hits atomic.Int32
	unsub := store.OnReload(func(_, _ *KeeperConfig) {
		hits.Add(1)
	})
	defer unsub()

	// Reload-loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			res := store.Reload(context.Background(), ReloadSourceAPI)
			if !res.Swapped {
				t.Errorf("Swapped=false at iter %d", i)
				return
			}
		}
	}()

	time.Sleep(80 * time.Millisecond)
	close(stop)
	wg.Wait()

	// We don't check the exact hits value — the callback runs in a separate
	// goroutine, and the last Reloads may not finish before the check. The key
	// thing is the absence of race/deadlock under `go test -race`.
	if hits.Load() == 0 {
		t.Error("persistent subscriber never invoked")
	}
}
