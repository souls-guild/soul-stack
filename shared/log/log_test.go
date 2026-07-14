package log

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestNew_WritesToFile — with File set, logs go to the file, not stderr.
func TestNew_WritesToFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "soul.log")

	logger := New(Options{Level: "info", Format: "json", File: logPath})
	logger.Info("hello", slog.String("k", "v"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(firstLine(t, data), &rec); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if rec["msg"] != "hello" || rec["k"] != "v" {
		t.Errorf("log record = %v; want msg=hello k=v", rec)
	}
}

// TestNew_RotatesBySize — a small max_size forces backup files to be created.
//
// lumberjack rotates when a write does NOT fit the current file at the reached
// limit. With max_size=1 MB we write several ~0.59 MB records — every second one
// does not fit and triggers a rotation; the directory keeps the active file plus
// backups.
//
// Root of the previous flakiness (failed under parallel `go test ./...`, not in
// isolation): on every Write lumberjack starts a background mill goroutine
// (compress/remove backups). Close() does NOT wait for it — with MaxBackups>0 it
// kept scanning and touching the directory after the test body returned, and the
// race with t.TempDir() cleanup (RemoveAll) produced "directory not empty".
//
// Determinism:
//   - MaxBackups: 0 → millRunOnce returns early and does NOT touch the FS (keeps
//     all backups; size rotation still works fully). The background goroutine
//     does not touch the directory — no race with cleanup.
//   - We write directly into the rotator, bypassing the slog handler: exact
//     control of the count and size of records, no dependency on handler
//     buffering.
//   - Close() before ReadDir: the current file is closed and the synchronous
//     rotation renames are guaranteed on disk.
//   - Records >0.5 MB spread rotations in time, avoiding backup-name collisions
//     (lumberjack granularity is a millisecond).
func TestNew_RotatesBySize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "soul.log")

	rot := &lumberjackWriter{
		Filename:   logPath,
		MaxSize:    1, // MB
		MaxBackups: 0, // see comment: the mill goroutine does not touch the directory
		MaxAge:     0,
		Compress:   false,
	}

	// A record just over half the limit: every second one does not fit and
	// triggers a rotation. 9 records → 4 rotations (4 backups) + the active file.
	line := append([]byte(strings.Repeat("x", 600*1024)), '\n') // ~0.59 MB
	const writes = 9
	for i := 0; i < writes; i++ {
		if _, err := rot.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := rot.Close(); err != nil {
		t.Fatalf("close rotator: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var active, backups int
	for _, e := range entries {
		switch {
		case e.Name() == "soul.log":
			active++
		case strings.HasPrefix(e.Name(), "soul-") && strings.HasSuffix(e.Name(), ".log"):
			backups++ // archived backup like soul-2006-01-02T15-04-05.000.log
		}
	}
	if active != 1 {
		t.Errorf("expected exactly one active soul.log, got %d", active)
	}
	if backups < 1 {
		t.Errorf("expected at least one backup file from size rotation, got %d backups in %s", backups, dir)
		for _, e := range entries {
			t.Logf("  entry: %s", e.Name())
		}
	}
}

// TestNew_FallbackStderr — without File the writer is stderr, no file is created.
func TestNew_FallbackStderr(t *testing.T) {
	t.Parallel()
	if w := writer(Options{}); w != os.Stderr {
		t.Errorf("writer without file = %T; want os.Stderr", w)
	}
}

// TestParseLevel — level from config, soft fallback to info.
func TestParseLevel(t *testing.T) {
	t.Parallel()
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"WARN":  slog.LevelWarn,
		" info": slog.LevelInfo,
		"":      slog.LevelInfo,
		"bogus": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v; want %v", in, got, want)
		}
	}
}

// TestNew_LevelFiltersDebug — at level=info debug records are dropped.
func TestNew_LevelFiltersDebug(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "soul.log")

	logger := New(Options{Level: "info", Format: "json", File: logPath})
	logger.Debug("should-be-dropped")
	logger.Warn("should-stay")

	data, _ := os.ReadFile(logPath)
	s := string(data)
	if strings.Contains(s, "should-be-dropped") {
		t.Errorf("debug line leaked under level=info: %s", s)
	}
	if !strings.Contains(s, "should-stay") {
		t.Errorf("warn line missing under level=info: %s", s)
	}
}

// TestLevelSet_ChangesFiltering — NewWithLevel returns a handle, and Level.Set
// actually changes the filtering threshold of an already-running logger
// (hot-reload logging.level, ADR-021). Before Set debug is dropped; after
// Set("debug") it passes.
func TestLevelSet_ChangesFiltering(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "soul.log")

	logger, lvl := NewWithLevel(Options{Level: "info", Format: "json", File: logPath})

	logger.Debug("before-set-dropped")
	lvl.Set("debug")
	logger.Debug("after-set-kept")
	lvl.Set("error")
	logger.Warn("after-error-dropped")
	logger.Error("after-error-kept")

	data, _ := os.ReadFile(logPath)
	s := string(data)
	if strings.Contains(s, "before-set-dropped") {
		t.Errorf("debug leaked under level=info before Set: %s", s)
	}
	if !strings.Contains(s, "after-set-kept") {
		t.Errorf("debug missing after Set(debug): %s", s)
	}
	if strings.Contains(s, "after-error-dropped") {
		t.Errorf("warn leaked after Set(error): %s", s)
	}
	if !strings.Contains(s, "after-error-kept") {
		t.Errorf("error missing after Set(error): %s", s)
	}
}

// TestLevelSet_Fallback — an unknown/empty level in Set → soft fallback to info
// (symmetric to parseLevel at initial build).
func TestLevelSet_Fallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "soul.log")

	logger, lvl := NewWithLevel(Options{Level: "error", Format: "json", File: logPath})
	lvl.Set("bogus") // → info
	logger.Info("info-after-fallback")
	logger.Debug("debug-still-dropped")

	data, _ := os.ReadFile(logPath)
	s := string(data)
	if !strings.Contains(s, "info-after-fallback") {
		t.Errorf("info missing after Set(bogus) fallback to info: %s", s)
	}
	if strings.Contains(s, "debug-still-dropped") {
		t.Errorf("debug leaked after fallback to info: %s", s)
	}
}

// TestRotationMaxAgeOverridesDefault — rotation.max_age_days reaches the
// rotator, the other fields keep their defaults.
func TestRotationMaxAgeOverridesDefault(t *testing.T) {
	t.Parallel()
	w := writer(Options{File: "/tmp/soul.log", Rotation: &config.LoggingRotation{MaxAgeDays: 21}})
	rot, ok := w.(*lumberjackWriter)
	if !ok {
		t.Fatalf("writer = %T; want *lumberjack.Logger", w)
	}
	if rot.MaxAge != 21 {
		t.Errorf("MaxAge = %d; want 21", rot.MaxAge)
	}
	if rot.MaxSize != defaultMaxSizeMB || rot.MaxBackups != defaultMaxBackups {
		t.Errorf("defaults not applied: size=%d backups=%d", rot.MaxSize, rot.MaxBackups)
	}
}

// TestRotationDefaults — File set, Rotation nil → all defaults on the rotator.
func TestRotationDefaults(t *testing.T) {
	t.Parallel()
	w := writer(Options{File: "/tmp/soul.log"})
	rot, ok := w.(*lumberjackWriter)
	if !ok {
		t.Fatalf("writer = %T; want *lumberjack.Logger", w)
	}
	if rot.MaxSize != defaultMaxSizeMB || rot.MaxBackups != defaultMaxBackups ||
		rot.MaxAge != defaultMaxAgeDays || rot.Compress != defaultCompress {
		t.Errorf("defaults not applied: %+v", rot)
	}
}

// TestFromSoul / TestFromKeeper — the builders carry fields into Options without loss.
func TestFromSoul(t *testing.T) {
	t.Parallel()
	opts := FromSoul(config.SoulLogging{
		Level: "warn", Format: "text", File: "/var/log/soul/soul.log",
		Rotation: &config.LoggingRotation{MaxSizeMB: 10, MaxAgeDays: 14},
	})
	if opts.Level != "warn" || opts.Format != "text" || opts.File != "/var/log/soul/soul.log" {
		t.Errorf("FromSoul scalar mismatch: %+v", opts)
	}
	if opts.Rotation == nil || opts.Rotation.MaxAgeDays != 14 {
		t.Errorf("FromSoul rotation mismatch: %+v", opts.Rotation)
	}
}

func TestFromKeeper(t *testing.T) {
	t.Parallel()
	opts := FromKeeper(config.KeeperLogging{
		Level: "info", Format: "json", File: "/var/log/keeper/keeper.log",
		Rotation: &config.LoggingRotation{MaxSizeMB: 100, MaxFiles: 10},
	})
	if opts.Level != "info" || opts.Format != "json" || opts.File != "/var/log/keeper/keeper.log" {
		t.Errorf("FromKeeper scalar mismatch: %+v", opts)
	}
	if opts.Rotation == nil || opts.Rotation.MaxFiles != 10 {
		t.Errorf("FromKeeper rotation mismatch: %+v", opts.Rotation)
	}
}

func firstLine(t *testing.T, data []byte) []byte {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	if !sc.Scan() {
		t.Fatal("no log lines written")
	}
	return []byte(sc.Text())
}
