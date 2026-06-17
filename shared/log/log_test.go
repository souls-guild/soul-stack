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

// TestNew_WritesToFile — при заданном File логи уходят в файл, не в stderr.
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

// TestNew_RotatesBySize — маленький max_size_mb вынуждает создать backup-файл.
//
// lumberjack ротирует, когда запись НЕ влезает в текущий файл при достигнутом
// лимите. С max_size=1 МБ пишем заведомо больше мегабайта мелкими строками —
// в каталоге должен появиться хотя бы один архивный файл рядом с активным.
func TestNew_RotatesBySize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "soul.log")

	logger := New(Options{
		Level:    "info",
		Format:   "json",
		File:     logPath,
		Rotation: &config.LoggingRotation{MaxSizeMB: 1, MaxFiles: 3, Compress: false},
	})

	big := strings.Repeat("x", 4096)
	for i := 0; i < 600; i++ { // ~2.4 МБ payload + JSON-обвязка
		logger.Info("rotate-probe", slog.String("payload", big))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var logs int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "soul") && strings.HasSuffix(e.Name(), ".log") {
			logs++
		} else if strings.HasPrefix(e.Name(), "soul-") {
			logs++ // архивный backup вида soul-2006-01-02T15-04-05.000.log[.gz]
		}
	}
	if logs < 2 {
		t.Errorf("expected active + at least one backup file, got %d log-files in %s", logs, dir)
		for _, e := range entries {
			t.Logf("  entry: %s", e.Name())
		}
	}
}

// TestNew_FallbackStderr — без File writer = stderr, файл не создаётся.
func TestNew_FallbackStderr(t *testing.T) {
	t.Parallel()
	if w := writer(Options{}); w != os.Stderr {
		t.Errorf("writer without file = %T; want os.Stderr", w)
	}
}

// TestParseLevel — уровень из конфига, мягкий фолбэк на info.
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

// TestNew_LevelFiltersDebug — при level=info debug-записи отбрасываются.
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

// TestLevelSet_ChangesFiltering — NewWithLevel возвращает handle, и Level.Set
// реально меняет порог фильтрации уже работающего логгера (hot-reload
// logging.level, ADR-021). До Set debug отбрасывается; после Set("debug") —
// проходит.
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

// TestLevelSet_Fallback — неизвестный/пустой уровень в Set → мягкий фолбэк на
// info (симметрия с parseLevel на initial-build).
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

// TestRotationMaxAgeOverridesDefault — rotation.max_age_days доезжает до
// ротатора, остальные поля остаются дефолтными.
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

// TestRotationDefaults — File задан, Rotation nil → все дефолты на ротаторе.
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

// TestFromSoul / TestFromKeeper — builders переносят поля в Options без потерь.
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
