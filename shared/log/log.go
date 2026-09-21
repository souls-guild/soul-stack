// Package log builds an slog logger with built-in log-file rotation (the
// cross-cutting "rotation out of the box" requirement, docs/requirements.md). Shared by
// keeper / soul / soul-lint (ADR-011 → shared/log).
//
// The package shadows stdlib `log`; import it under an alias (e.g. `shlog`).
//
// Default behavior:
//   - File unset → output to stderr (dev mode, convenient under systemd/journald
//     and in a container, no rotation).
//   - File set → write to the file through the lumberjack rotator with defaults for
//     omitted fields (50 MB / 7 days / 5 backups / compress).
//
// Name mapping: shared `rotation.max_files` maps to lumberjack `MaxBackups` ("how many
// archived files to keep"); there's no separate `max_backups` in the schema to avoid a
// duplicate.
//
// The builder is stateless and re-callable: New takes an Options snapshot with no
// globals/singleton — suitable for a future hot-reload re-init of the log writer.
package log

import (
	"io"
	"log/slog"
	"os"
	"strings"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"github.com/souls-guild/soul-stack/shared/config"
)

// lumberjackWriter — a local alias for the rotator; keeps the type in one place
// (simplifies tests, isolates the dependency package name).
type lumberjackWriter = lumberjack.Logger

// Rotation defaults. Applied when File is set but the corresponding field is omitted
// (0 / empty). Values match the normative tables in docs/soul/config.md and
// docs/keeper/config.md → `logging:`.
//
// defaultMaxAgeDays: the shared schema moved from an overlay `*int` to a flat int, and
// the "0 vs unset" distinction was dropped deliberately (see config.LoggingRotation).
// MaxAgeDays==0 → this default (7 days); the current grammar has no way to express "no
// age-based deletion" — a documented MVP limitation.
const (
	defaultMaxSizeMB  = 50
	defaultMaxAgeDays = 7
	defaultMaxBackups = 5
	defaultCompress   = true
)

// Options — the assembled logger parameters. Passed to [New]. Level/format/file path +
// rotation fields explicitly, without wrapping in a config struct (the builder doesn't
// depend on whose config it is — keeper or soul).
type Options struct {
	Level  string
	Format string
	// File — the log-file path. Empty → output to stderr without rotation.
	File string
	// Rotation — rotation fields; nil → defaults are used (when File is set).
	Rotation *config.LoggingRotation
}

// FromSoul builds Options from a config.SoulLogging snapshot.
func FromSoul(lg config.SoulLogging) Options {
	return Options{Level: lg.Level, Format: lg.Format, File: lg.File, Rotation: lg.Rotation}
}

// FromKeeper builds Options from a config.KeeperLogging snapshot.
func FromKeeper(lg config.KeeperLogging) Options {
	return Options{Level: lg.Level, Format: lg.Format, File: lg.File, Rotation: lg.Rotation}
}

// Level — a runtime-mutable logger-level handle (a wrapper over [slog.LevelVar]).
// Returned from [NewWithLevel]; held by the caller to change the logging threshold on
// hot-reload (`logging.level`, ADR-021) without recreating the writer (file handle /
// rotator remain restart-required).
//
// `*slog.LevelVar` is already thread-safe (atomic). The wrapper adds a string
// [Level.Set] that accepts a level from config (`debug`/`info`/`warn`/`error`) — with
// the same soft fallback as the initial build.
type Level struct {
	v *slog.LevelVar
}

// Set changes the logging threshold on the fly. String parsing via [parseLevel]
// (empty/unknown → info; schema enum validation already filtered out garbage at the
// Load phase). Safe to call concurrently with log writes.
func (l *Level) Set(level string) {
	l.v.Set(parseLevel(level))
}

// New builds a *slog.Logger from Options. A thin wrapper over [NewWithLevel] for call
// sites that don't need the runtime level handle (tests, push-oneshot).
//
// File empty → the handler writes to stderr (dev). Otherwise a lumberjack rotator is
// created at the File path with defaults for omitted fields. Format defaults to json;
// text on request. Level defaults to info.
func New(opts Options) *slog.Logger {
	logger, _ := NewWithLevel(opts)
	return logger
}

// NewWithLevel builds a *slog.Logger and returns [*Level] — a handle for runtime level
// changes (hot-reload `logging.level`, ADR-021).
//
// The level in [slog.HandlerOptions] is set via [slog.LevelVar] rather than a static
// [slog.Level], so a later [Level.Set] changes filtering on the already-running handler
// without recreating the writer. file/format/rotation are fixed at build time
// (restart-required) — Level doesn't touch them.
func NewWithLevel(opts Options) (*slog.Logger, *Level) {
	w := writer(opts)
	lv := &slog.LevelVar{}
	lv.Set(parseLevel(opts.Level))
	ho := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if strings.EqualFold(opts.Format, "text") {
		h = slog.NewTextHandler(w, ho)
	} else {
		h = slog.NewJSONHandler(w, ho)
	}
	return slog.New(h), &Level{v: lv}
}

// writer picks the sink: stderr (File unset) or the lumberjack rotator.
func writer(opts Options) io.Writer {
	if opts.File == "" {
		return os.Stderr
	}
	rot := &lumberjackWriter{
		Filename:   opts.File,
		MaxSize:    defaultMaxSizeMB,
		MaxBackups: defaultMaxBackups,
		MaxAge:     defaultMaxAgeDays,
		Compress:   defaultCompress,
	}
	if r := opts.Rotation; r != nil {
		if r.MaxSizeMB > 0 {
			rot.MaxSize = r.MaxSizeMB
		}
		if r.MaxFiles > 0 {
			rot.MaxBackups = r.MaxFiles
		}
		if r.MaxAgeDays > 0 {
			rot.MaxAge = r.MaxAgeDays
		}
		// Compress — a bool with no "omitted": omitempty in the schema makes false and
		// "unset" and "explicitly false" indistinguishable. The rotation default is
		// compression on, so we honor explicit compress only when true. Enough for MVP:
		// true in config → compress on, otherwise the default.
		if r.Compress {
			rot.Compress = true
		}
	}
	return rot
}

// parseLevel converts a level string to slog.Level. Empty/unknown → Info (schema enum
// validation already filtered out garbage at the Load* stage; here — a soft fallback).
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
