// Package log — построение slog-логгера с встроенной ротацией лог-файла
// (сквозное требование «ротация из коробки», docs/requirements.md). Общий для
// keeper / soul / soul-lint (ADR-011 → shared/log).
//
// Пакет шэдоуит stdlib `log`; импортировать под алиасом (например `shlog`).
//
// Поведение по умолчанию:
//   - File не задан → вывод в stderr (dev-режим, удобно под systemd/journald
//     и в контейнере, без ротации).
//   - File задан → запись в файл через ротатор lumberjack с дефолтами для
//     опущенных полей (50 МБ / 7 дней / 5 backups / compress).
//
// Маппинг имён: shared `rotation.max_files` соответствует lumberjack
// `MaxBackups` («сколько архивных файлов держать»); отдельного `max_backups`
// в схеме нет, чтобы не плодить дубль.
//
// Builder stateless и пере-вызываемый: New принимает снимок Options без
// глобалов/синглтона — пригодно для будущего hot-reload re-init log writer-а.
package log

import (
	"io"
	"log/slog"
	"os"
	"strings"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"github.com/souls-guild/soul-stack/shared/config"
)

// lumberjackWriter — локальный alias на ротатор; держит тип в одном месте
// (упрощает тесты, изолирует имя пакета зависимости).
type lumberjackWriter = lumberjack.Logger

// Дефолты ротации. Применяются, когда File задан, но соответствующее поле
// опущено (0 / пусто). Значения согласованы с нормативными таблицами
// docs/soul/config.md и docs/keeper/config.md → `logging:`.
//
// defaultMaxAgeDays: shared-схема перешла с overlay-`*int` на плоский int, и
// различение «0 vs не задано» снято сознательно (см. config.LoggingRotation).
// MaxAgeDays==0 → этот дефолт (7 дней); чтобы «без age-based удаления», в
// текущей грамматике средства нет — это задокументированное ограничение MVP.
const (
	defaultMaxSizeMB  = 50
	defaultMaxAgeDays = 7
	defaultMaxBackups = 5
	defaultCompress   = true
)

// Options — собранные параметры логгера. Передаётся в [New]. Уровень/формат/
// путь к файлу + поля ротации явно, без обёртки в config-структуру (builder
// не зависит от того, чей это конфиг — keeper или soul).
type Options struct {
	Level  string
	Format string
	// File — путь к лог-файлу. Пусто → вывод в stderr без ротации.
	File string
	// Rotation — поля ротации; nil → используются дефолты (когда File задан).
	Rotation *config.LoggingRotation
}

// FromSoul строит Options из снимка config.SoulLogging.
func FromSoul(lg config.SoulLogging) Options {
	return Options{Level: lg.Level, Format: lg.Format, File: lg.File, Rotation: lg.Rotation}
}

// FromKeeper строит Options из снимка config.KeeperLogging.
func FromKeeper(lg config.KeeperLogging) Options {
	return Options{Level: lg.Level, Format: lg.Format, File: lg.File, Rotation: lg.Rotation}
}

// Level — runtime-изменяемый handle уровня логгера (обёртка над
// [slog.LevelVar]). Возвращается из [NewWithLevel]; держится caller-ом, чтобы
// на hot-reload (`logging.level`, ADR-021) сменить порог логирования без
// пере-создания writer-а (file handle / ротатор остаются restart-required).
//
// `*slog.LevelVar` уже потокобезопасен (atomic). Обёртка добавляет
// строковый [Level.Set], принимающий уровень из конфига (`debug`/`info`/
// `warn`/`error`) — тем же мягким фолбэком, что и initial-build.
type Level struct {
	v *slog.LevelVar
}

// Set меняет порог логирования на лету. Парсинг строки — через [parseLevel]
// (пустое/неизвестное → info, схемная enum-валидация уже отсеяла мусор на
// Load-фазе). Безопасен для конкурентного вызова с записью логов.
func (l *Level) Set(level string) {
	l.v.Set(parseLevel(level))
}

// New строит *slog.Logger по Options. Тонкая обёртка над [NewWithLevel] для
// call-site-ов, которым не нужен runtime-handle уровня (тесты, push-oneshot).
//
// File пуст → handler пишет в stderr (dev). Иначе создаётся lumberjack-ротатор
// по пути File с дефолтами для опущенных полей. Format по умолчанию json;
// text — по запросу. Level по умолчанию info.
func New(opts Options) *slog.Logger {
	logger, _ := NewWithLevel(opts)
	return logger
}

// NewWithLevel строит *slog.Logger и возвращает [*Level] — handle для
// runtime-смены уровня (hot-reload `logging.level`, ADR-021).
//
// Уровень в [slog.HandlerOptions] задаётся через [slog.LevelVar], а не через
// статический [slog.Level], поэтому последующий [Level.Set] меняет фильтрацию
// уже работающего handler-а без пере-создания writer-а. file/format/rotation
// фиксируются на build-е (restart-required) — их Level не трогает.
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

// writer выбирает приёмник: stderr (File не задан) либо lumberjack-ротатор.
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
		// Compress — bool без «опущено»: omitempty в схеме делает false и
		// «не задано», и «явно false» неразличимыми. Дефолт ротации — сжатие
		// включено, поэтому уважаем явный compress только если он true. Для
		// MVP достаточно: true в конфиге → compress on, иначе дефолт.
		if r.Compress {
			rot.Compress = true
		}
	}
	return rot
}

// parseLevel переводит строку уровня в slog.Level. Пустое/неизвестное → Info
// (схемная валидация enum уже отсеяла мусор на этапе Load*; здесь — мягкий
// фолбэк).
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
