package config

// Мутация одного scalar-значения по yaml.Path с сохранением inline-comment-а
// под [ADR-021](docs/architecture.md). API библиотечный; consumer (Operator
// API `config.set`) появится отдельным slice-ом в M0.3.

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
)

// ErrPathNotFound — yaml-path не существует в документе. Caller отвечает
// за решение: вернуть пользователю как 404, либо обернуть в create-on-write
// pipeline (отложено в M0.2.5).
var ErrPathNotFound = errors.New("yaml path not found")

// PatchKeeper мутирует значение по `yamlPath` в KeeperConfig-документе.
//
// Contract:
//   - `yamlPath` — формат goccy/go-yaml.PathString (например, `$.kid`,
//     `$.listen.grpc.addr`, `$.services[0].ref`). Резолв через
//     `yaml.PathString(...)`; синтаксические ошибки возвращаются как есть.
//   - `value` маршалится через `yaml.Marshal` — caller отвечает за
//     совместимость типа со схемой (PatchKeeper НЕ перевалидирует
//     KeeperConfig после мутации; делать это — задача caller-а).
//   - На несуществующем path возвращает `ErrPathNotFound`. Create-on-write
//     в MVP не делаем.
//   - Inline-comment scalar-узла, который был target-ом Patch, сохраняется
//     (snapshot+restore через `path.FilterFile` + `SetComment`).
//   - Метод thread-safe: конкурентные `PatchKeeper`/`PatchSoul`/`Save*` для
//     одного и того же `*Document` сериализуются через `doc.mu`. Один Document
//     можно безопасно расшаривать между goroutine-ами; внешняя синхронизация
//     не нужна.
//
// error возвращается при I/O fatal (`Save*` — write/rename/stat) или
// programming error (nil Document, пустой yaml path, путь без `$`-префикса,
// non-scalar target, marshal value с неподдерживаемым типом — chan/func/
// циклическая структура). Validation-error-ы при `Load*` приходят как
// `[]diag.Diagnostic` (см. ADR-021 d).
func PatchKeeper(doc *Document, yamlPath string, value any) error {
	return patchOne(doc, yamlPath, value)
}

// PatchSoul — то же для SoulConfig-документа. Контракт идентичен PatchKeeper,
// включая thread-safety и набор возвращаемых error-ов.
func PatchSoul(doc *Document, yamlPath string, value any) error {
	return patchOne(doc, yamlPath, value)
}

func patchOne(doc *Document, yamlPath string, value any) error {
	if doc == nil {
		return errors.New("config: Document is nil")
	}
	// doc.file устанавливается только в parseAndValidate; после конструирования immutable.
	if doc.file == nil {
		return errors.New("config: Document has no AST file (parse failed; cannot patch)")
	}

	// Pre-validate yamlPath ДО `yaml.PathString`: на пустой / whitespace-only
	// строке goccy возвращает не-nil Path с nil error, и последующий
	// `FilterFile` улетает в SIGSEGV (`path.go:491`). Аналогично пути без
	// `$`-префикса — это синтаксически некорректный YAMLPath, лучше отвергать
	// явным сообщением, чем доверять goccy.
	if strings.TrimSpace(yamlPath) == "" {
		return errors.New("config: yaml path is empty")
	}
	if !strings.HasPrefix(yamlPath, "$") {
		return fmt.Errorf("config: yaml path must start with '$': got %q", yamlPath)
	}

	p, err := yaml.PathString(yamlPath)
	if err != nil {
		return fmt.Errorf("config: invalid yaml path %q: %w", yamlPath, err)
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	// (a) Existence check + захват inline-comment текущего scalar-узла.
	//
	// Используем FilterFile (не ReadNode): ReadNode принимает io.Reader и
	// внутри пересортировывает AST, теряя ссылки на оригинальные узлы;
	// FilterFile отдаёт живой указатель на узел внутри `doc.file`,
	// inline-comment которого мы и сохраним.
	target, err := p.FilterFile(doc.file)
	if err != nil {
		if yaml.IsNotFoundNodeError(err) {
			return fmt.Errorf("%w: %s", ErrPathNotFound, yamlPath)
		}
		return fmt.Errorf("config: cannot resolve path %q: %w", yamlPath, err)
	}

	// Reject non-scalar target (mapping/sequence/anchor/...). Контракт
	// PatchKeeper/PatchSoul — scalar-replace; молчаливая замена целого
	// поддерева чревата silent data loss (`$.listen.grpc` → выкинет
	// tls.cert/tls.key/tls.ca). Кому надо менять mapping — пусть пишет
	// отдельный Patch на каждое скалярное поле.
	if _, isScalar := target.(ast.ScalarNode); !isScalar {
		return fmt.Errorf("config: yaml path %q points to non-scalar node (kind=%s); "+
			"only scalars are patchable (non_scalar_patch_target)", yamlPath, target.Type().String())
	}
	oldComment := target.GetComment()

	// (b) Marshal значения. yaml.Marshal добавляет завершающий `\n`,
	// trim его — для ReplaceWithReader важна одна-строка/одна-нода.
	raw, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Errorf("config: cannot marshal value for %q: %w", yamlPath, err)
	}
	// Marshal скаляра отдаёт `value\n`, mapping/sequence — multi-line с `\n`
	// в конце. Хвостовой `\n` парсер terpит, но trim для чистоты.
	fragment := bytes.TrimRight(raw, "\n")

	// (c) Replace в AST.
	if err := p.ReplaceWithReader(doc.file, bytes.NewReader(fragment)); err != nil {
		if yaml.IsNotFoundNodeError(err) {
			return fmt.Errorf("%w: %s", ErrPathNotFound, yamlPath)
		}
		return fmt.Errorf("config: cannot replace at %q: %w", yamlPath, err)
	}

	// (d) Restore inline-comment.
	if oldComment != nil {
		newNode, err := p.FilterFile(doc.file)
		if err == nil && newNode != nil {
			_ = newNode.SetComment(oldComment)
		}
	}

	doc.mutated = true
	return nil
}
