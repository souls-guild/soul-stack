// Package essence собирает effective essence сервиса для конкретного хоста по
// иерархии слоёв (см. architecture.md → «Essence: pipeline сборки»):
//
//	essence/_default.yaml → essence/os/<family>.yaml → essence/coven/<метка>.yaml... → incarnation.spec.essence
//
// Реализован convention-based порядок (без `_stack.yaml`): essence
// role-agnostic (ADR-008 — ступени role/<Y>.yaml нет). Каждый следующий слой
// deep-merge-ится поверх предыдущего: maps сливаются рекурсивно, скаляры и
// списки заменяются целиком (later wins).
package essence

import "log/slog"

// Имена слоёв essence в service-репозитории. Все пути — относительно
// `<ServiceDir>/essence/` по документированной конвенции (docs/service/manifest.md,
// architecture.md → «Раскладка репозитория»): essence лежит в поддиректории
// сервиса, НЕ в его корне.
const (
	// essenceDir — корневой каталог essence внутри снапшота сервиса.
	essenceDir = "essence"
	// defaultFile — baseline-слой, общий для всех incarnation
	// (`essence/_default.yaml`). Отсутствие файла допустимо (пустая база).
	defaultFile = essenceDir + "/_default.yaml"
	// osDir — каталог per-OS overlay'ев (`essence/os/debian.yaml`,
	// `essence/os/rhel.yaml`).
	osDir = essenceDir + "/os"
	// covenDir — каталог per-coven overlay'ев (`essence/coven/<метка>.yaml`).
	covenDir = essenceDir + "/coven"
)

// ResolveInput — вход для сборки essence одного хоста.
type ResolveInput struct {
	// ServiceDir — корень снапшота сервиса (artifact.ServiceArtifact.LocalDir).
	ServiceDir string
	// OSFamily — `soulprint.self.os.family` хоста (например, "debian"). Пустая
	// строка → os-слой пропускается.
	OSFamily string
	// Covens — Coven-метки хоста (souls.coven[]). Слои применяются в порядке
	// сортировки имён для детерминизма.
	Covens []string
	// IncarnationSpec — `incarnation.spec.essence`, override оператора (самый
	// сильный слой). Может быть nil.
	IncarnationSpec map[string]any
}

// Resolver собирает essence-map по слоям. Без внутреннего состояния, безопасен
// для конкурентного использования.
type Resolver struct {
	logger *slog.Logger
}

// NewResolver создаёт Resolver. Если logger nil, используется slog.Default.
func NewResolver(logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{logger: logger}
}
