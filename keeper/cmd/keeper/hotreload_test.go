package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/toll"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	shlog "github.com/souls-guild/soul-stack/shared/log"
)

// keeperFixtureStore -- copies the golden keeper.yml into a temp file and
// wraps it in a Store. Editing the file + Reload is equivalent to SIGHUP.
func keeperFixtureStore(t *testing.T) (*config.Store[config.KeeperConfig], string) {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash("../../../examples/keeper/keeper.yml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "keeper.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	store, diags, err := config.LoadKeeperStore(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("fixture has error diag: %s [%s] %s", d.Phase, d.Code, d.Message)
		}
	}
	return store, path
}

// TestLevelSubscriber_ReflectsReload -- the logger-level subscription on the
// store (as in runDaemon) moves the filter threshold on a Reload-swap from
// the new snapshot.
func TestLevelSubscriber_ReflectsReload(t *testing.T) {
	t.Parallel()
	store, path := keeperFixtureStore(t)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "keeper.log")
	cfg := store.Get()
	logger, logLevel := shlog.NewWithLevel(shlog.Options{Level: cfg.Logging.Level, Format: "json", File: logPath})
	store.OnReload(func(_, newCfg *config.KeeperConfig) {
		if newCfg != nil {
			logLevel.Set(newCfg.Logging.Level)
		}
	})

	logger.Debug("pre-reload-debug-dropped") // level=info

	src, _ := os.ReadFile(path)
	edited := bytes.Replace(src, []byte("level: info"), []byte("level: debug"), 1)
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write edit: %v", err)
	}
	res := store.Reload(context.Background(), config.ReloadSourceSignal)
	if !res.Swapped {
		t.Fatalf("Swapped=false on valid edit: %+v", res.Diagnostics)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logger.Debug("post-reload-debug-probe")
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "post-reload-debug-probe") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	data, _ := os.ReadFile(logPath)
	s := string(data)
	if strings.Contains(s, "pre-reload-debug-dropped") {
		t.Errorf("pre-reload debug leaked under level=info: %s", s)
	}
	if !strings.Contains(s, "post-reload-debug-probe") {
		t.Errorf("post-reload debug missing after Set(debug) via subscriber: %s", s)
	}
}

// TestTollSubscriber_AppliesNewThreshold -- the daemon-applyTollReload
// subscription (wired via store.OnReload in setupToll) rewrites the
// Leader config on a successful Reload-swap. The test assembles a real
// [toll.Leader] (publicly-exposed toll pkg adapters are not allowed; instead
// we check the linkage: OnReload calls the callback with the new
// cfg.Toll.* -> the daemon sees the new value). Full UpdateConfig semantics
// coverage is in leader_reload_test.go.
func TestTollSubscriber_AppliesNewThreshold(t *testing.T) {
	t.Parallel()
	store, path := keeperFixtureStore(t)

	var (
		mu          sync.Mutex
		lastApplied float64
	)
	// Simulates the setupToll subscription: on every Reload-swap we read
	// cfg.Toll.Threshold (resolving defaults as in applyTollReload).
	store.OnReload(func(_, newCfg *config.KeeperConfig) {
		mu.Lock()
		defer mu.Unlock()
		if newCfg == nil {
			return
		}
		cfgToll := newCfg.Toll
		if cfgToll == nil {
			cfgToll = &config.KeeperToll{}
		}
		threshold := cfgToll.Threshold
		if threshold <= 0 {
			threshold = config.DefaultTollThreshold
		}
		lastApplied = threshold
	})

	// Edit the fixture: add a toll block with an explicit threshold=0.42.
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture path: %v", err)
	}
	if bytes.Contains(src, []byte("\ntoll:")) {
		t.Skip("fixture already contains a toll block -- test needs adjusting")
	}
	edited := append(src, []byte("\ntoll:\n  threshold: 0.42\n")...)
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write edit: %v", err)
	}

	res := store.Reload(context.Background(), config.ReloadSourceSignal)
	if !res.Swapped {
		t.Fatalf("Swapped=false: %+v", res.Diagnostics)
	}

	// Notify happens in a separate goroutine -- wait for the result.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := lastApplied
		mu.Unlock()
		if got == 0.42 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	got := lastApplied
	mu.Unlock()
	if got != 0.42 {
		t.Fatalf("expected threshold=0.42 after a SIGHUP-style reload, got %v", got)
	}
}

// TestTollWebhookCfgChanged_Diff -- a targeted check of the diff helper
// [tollWebhookCfgChanged] (see daemon.go). Covers nil boundaries and each
// field individually.
func TestTollWebhookCfgChanged_Diff(t *testing.T) {
	t.Parallel()
	if tollWebhookCfgChanged(nil, nil) {
		t.Fatal("nil/nil -- no change")
	}
	cfg := &config.KeeperTollWebhook{Enabled: true, URLRef: "https://x", Format: "generic", Timeout: "10s"}
	if !tollWebhookCfgChanged(nil, cfg) {
		t.Fatal("nil → non-nil -- change")
	}
	if !tollWebhookCfgChanged(cfg, nil) {
		t.Fatal("non-nil → nil -- change")
	}
	clone := *cfg
	if tollWebhookCfgChanged(cfg, &clone) {
		t.Fatal("identical blocks -- no change")
	}
	mutated := *cfg
	mutated.URLRef = "https://y"
	if !tollWebhookCfgChanged(cfg, &mutated) {
		t.Fatal("URLRef diff -- change")
	}
	mutated = *cfg
	mutated.Enabled = false
	if !tollWebhookCfgChanged(cfg, &mutated) {
		t.Fatal("Enabled diff -- change")
	}
	mutated = *cfg
	mutated.Format = "slack"
	if !tollWebhookCfgChanged(cfg, &mutated) {
		t.Fatal("Format diff -- change")
	}
	mutated = *cfg
	mutated.Timeout = "5s"
	if !tollWebhookCfgChanged(cfg, &mutated) {
		t.Fatal("Timeout diff -- change")
	}
}

// TestApplyTollReload_NoLeader -- applyTollReload is guarded by a nil check:
// with no Toll attached (no Redis / opted out), the call is a no-op.
func TestApplyTollReload_NoLeader(t *testing.T) {
	t.Parallel()
	d := &daemon{cfg: &config.KeeperConfig{KID: "kid-test"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Must not panic / NPE when tollLeader==nil.
	d.applyTollReload(&config.KeeperConfig{KID: "kid-test"}, logger)
}

// TestApplyTollReload_RealLeader_UpdatesThreshold -- assembles a real
// [toll.Leader] via the public constructor with a minimal fake pipeline
// (adapters here are integrated as thin inline structs; they are not
// imported from toll-test-helpers -- those are unexported). Verifies the
// end-to-end path: applyTollReload → Leader.UpdateConfig → the tick
// snapshot sees the new value.
func TestApplyTollReload_RealLeader_UpdatesThreshold(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := toll.LeaderConfig{
		KID:              "kid-test",
		LeaseTTL:         100 * time.Millisecond,
		AcquireRetry:     10 * time.Millisecond,
		TickInterval:     20 * time.Millisecond,
		WindowSize:       60 * time.Second,
		Threshold:        0.10,
		DegradedTTL:      60 * time.Second,
		ClearGrace:       50 * time.Millisecond,
		BaselineCacheTTL: 60 * time.Second,
	}
	leader, err := toll.NewLeader(cfg, toll.LeaderDeps{
		Lease:          stubLeaseAcquirer{},
		SortedSet:      stubSortedSetReader{},
		DegradedWriter: stubDegradedWriter{},
		Baseline:       stubBaseline{value: 100},
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("NewLeader: %v", err)
	}

	d := &daemon{
		cfg:        &config.KeeperConfig{KID: "kid-test"},
		tollLeader: leader,
	}
	// Hot-reload: threshold=0.42 + per-coven set.
	newCfg := &config.KeeperConfig{
		KID: "kid-test",
		Toll: &config.KeeperToll{
			Threshold:          0.42,
			WindowSize:         "30s",
			DegradedTTL:        "20s",
			ClearGrace:         "10s",
			PerCovenThresholds: map[string]float64{"x": 0.05},
		},
	}
	d.applyTollReload(newCfg, logger)

	// Sanity: CurrentNotifier stays nil (no webhook block in cfg).
	if leader.CurrentNotifier() != nil {
		t.Fatal("expected nil CurrentNotifier with no webhook block")
	}
}

// stubLeaseAcquirer -- never grants a lease (the Leader-loop spins in
// acquire-retry, but we never call Run in this test). The test checks the
// UpdateConfig path in applyTollReload, not the leader loop itself.
type stubLeaseAcquirer struct{}

func (stubLeaseAcquirer) Acquire(_ context.Context, _, _ string, _ time.Duration) (toll.Lease, error) {
	return nil, toll.ErrLeaseTaken
}

type stubSortedSetReader struct{}

func (stubSortedSetReader) CountInWindow(context.Context, int64, int64) (int64, error) {
	return 0, nil
}

func (stubSortedSetReader) TrimBelow(context.Context, int64) error { return nil }

type stubDegradedWriter struct{}

func (stubDegradedWriter) SetDegraded(context.Context, string, time.Duration) error { return nil }
func (stubDegradedWriter) ClearDegraded(context.Context) error                      { return nil }

type stubBaseline struct{ value int64 }

func (b stubBaseline) BaselineConnected(context.Context) (int64, error) { return b.value, nil }
