package config

import (
	"sync"

	"github.com/goccy/go-yaml/ast"
)

// Document — opaque handle на AST + исходные байты для round-trip write-back
// под [ADR-021](docs/architecture.md).
//
// Поля приватные: внешние пакеты не должны зависеть от внутренней раскладки.
// Все мутации идут через свободные функции пакета (`PatchKeeper`/`PatchSoul`),
// все записи — через `SaveKeeper`/`SaveSoul` / `*ToBytes`.
//
// `mutated` фиксирует факт того, что хотя бы один Patch* успешно отработал
// над этим документом: для немутированного документа `Save*ToBytes` отдаёт
// исходные байты (гарантия byte-identical round-trip), для мутированного —
// рендер AST через `file.String()` с приложенным `round_trip_warning`-ом.
type Document struct {
	file    *ast.File
	source  []byte
	path    string
	mu      sync.Mutex
	mutated bool
}
