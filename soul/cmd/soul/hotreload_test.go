package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	shlog "github.com/souls-guild/soul-stack/shared/log"
)

// soulFixtureStore copies the golden soul.yml into a temp file and wraps it
// in a Store. Returns the Store and the path (for a later edit + Reload,
// equivalent to a SIGHUP re-read).
func soulFixtureStore(t *testing.T) (*config.Store[config.SoulConfig], string) {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash("../../../examples/soul/soul.yml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "soul.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	store, diags, err := config.LoadSoulStore(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulStore: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("fixture has error diag: %s [%s] %s", d.Phase, d.Code, d.Message)
		}
	}
	return store, path
}

// TestResolveBackoff_ReflectsReload — after editing the file + Reload,
// resolveBackoff sees the new keeper.retry.backoff (hot-reload, ADR-021).
func TestResolveBackoff_ReflectsReload(t *testing.T) {
	t.Parallel()
	store, path := soulFixtureStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	b := resolveBackoff(store, logger)
	if b.initial != 1*time.Second || b.max != 30*time.Second {
		t.Fatalf("initial backoff = %+v, want initial=1s max=30s", b)
	}

	src, _ := os.ReadFile(path)
	edited := bytes.Replace(src, []byte("initial: 1s"), []byte("initial: 4s"), 1)
	edited = bytes.Replace(edited, []byte("max: 30s"), []byte("max: 90s"), 1)
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write edit: %v", err)
	}

	res := store.Reload(context.Background(), config.ReloadSourceSignal)
	if !res.Swapped {
		t.Fatalf("Swapped=false on valid edit: %+v", res.Diagnostics)
	}

	b2 := resolveBackoff(store, logger)
	if b2.initial != 4*time.Second || b2.max != 90*time.Second {
		t.Errorf("after reload backoff = %+v, want initial=4s max=90s", b2)
	}
}

// TestResolveSoulprintInterval_ReflectsReload — soulprint.refresh_interval is
// re-read from the snapshot after Reload.
func TestResolveSoulprintInterval_ReflectsReload(t *testing.T) {
	t.Parallel()
	store, path := soulFixtureStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if d := resolveSoulprintInterval(store, logger); d != 5*time.Minute {
		t.Fatalf("initial refresh_interval = %s, want 5m", d)
	}

	src, _ := os.ReadFile(path)
	edited := bytes.Replace(src, []byte("refresh_interval: 5m"), []byte("refresh_interval: 15m"), 1)
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write edit: %v", err)
	}
	if res := store.Reload(context.Background(), config.ReloadSourceSignal); !res.Swapped {
		t.Fatalf("Swapped=false on valid edit: %+v", res.Diagnostics)
	}

	if d := resolveSoulprintInterval(store, logger); d != 15*time.Minute {
		t.Errorf("after reload refresh_interval = %s, want 15m", d)
	}
}

// TestLevelSubscriber_ReflectsReload — the logger-level subscription on
// store (as in runDaemon) actually moves the filter threshold on a
// Reload-swap.
func TestLevelSubscriber_ReflectsReload(t *testing.T) {
	t.Parallel()
	store, path := soulFixtureStore(t)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "soul.log")
	cfg := store.Get()
	logger, logLevel := shlog.NewWithLevel(shlog.Options{Level: cfg.Logging.Level, Format: "json", File: logPath})
	store.OnReload(func(_, newCfg *config.SoulConfig) {
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
	// OnReload subscriber runs in a separate goroutine; wait for the effect.
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
