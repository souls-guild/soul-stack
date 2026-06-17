package config

// Write-back YAML под [ADR-021](docs/architecture.md). Сохранение
// `keeper.yml`/`soul.yml` с preserve comments/order/anchors через AST
// goccy/go-yaml + атомарный rename (tmp + chmod до write + rename + fsync).
//
// Known limitations (документируются `round_trip_warning`-диагностиками при
// мутации; для немутированного Document `Save*ToBytes` возвращает исходные
// байты как есть, чтобы round-trip был byte-identical):
//
//   - Flow-style mappings («`{ a: b, c: d }`») после AST-рендера goccy
//     теряют выравнивание ключей и внутренние пробелы (`{a: b, c: d}`).
//     В немутированном документе это нивелируется отдачей `doc.source`;
//     после Patch — известный косметический drift, поднимается warning.
//   - Inline-comment scalar-узла, который сам стал target Patch-а,
//     сохраняется (snapshot+restore через `path.FilterFile`+`SetComment`),
//     но multi-space между значением и `#` сжимается до одного пробела
//     (`"value"  # cmt` → `"value" # cmt`) — особенность стрингера goccy.
//   - Anchors-mutation (`&anchor` / `*alias`) при Patch-е якорного узла
//     может расщепить alias-references (best-effort, без явной валидации).
//   - Multi-line scalar style best-effort: block-литерал `|` или `>` может
//     быть переписан в flow при Patch коротких значений.
//   - Numeric literals (`0755`, `0xFF`, `0o755`) не сохраняются в literal-
//     форме после Patch — записываются decimal-ом.
//   - BOM (`EF BB BF`) не восстанавливается на запись: strip-нут при Load
//     по правилам YAML 1.2.
//
// I/O pipeline atomic-rename (9 шагов, см. `writeFileAtomically`):
//   1. Stat dst → mode/uid/gid; reject symlink на Lstat этапе.
//   2. CreateTemp в той же директории.
//   3. Chmod tmp до Write (избежать окна 0600 default при чтении).
//   4. Chown tmp на uid/gid исходника (best-effort на permission denied).
//   5. Write rendered bytes.
//   6. tmp.Sync() — fsync содержимого.
//   7. tmp.Close().
//   8. Rename(tmp, dst).
//   9. fsync директории-родителя (best-effort).
//
// На любой ошибке после CreateTemp — `os.Remove(tmp.Name())` cleanup.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// SaveKeeper — round-trip write-back KeeperConfig в файл `path`.
//
// Contract:
//   - Возвращаемые `[]diag.Diagnostic` уровня warning сигнализируют о
//     round-trip degradation (см. known limitations выше).
//   - error возвращается при I/O fatal (write/rename/stat/chmod/symlink-target)
//     или programming error (nil Document, пустой `path`). Validation-error-ы
//     при `Load*` приходят как `[]diag.Diagnostic` (см. ADR-021 d).
//   - Permissions и uid/gid исходного файла переносятся на новый (best-effort
//     для chown — если у процесса нет CAP_CHOWN, не считается fatal).
//   - Если файл по пути dst не существует, он будет создан с mode 0o644.
//     Permissions/uid/gid существующего файла сохраняются. Если `os.Stat(dst)`
//     упал по причине, отличной от `IsNotExist` (permission denied и т.п.) —
//     Save отвергает запись (а не молча перезатирает mode по 0o644).
//   - Если по `path` уже лежит symlink — отвергается с `error` и diagnostic
//     `symlink_write_not_supported`, файл не модифицируется.
//   - Document мьютируется только в чтение; mutated-флаг учитывается в
//     рендере (см. SaveKeeperToBytes).
//   - Метод thread-safe: конкурентные `Save*` / `Patch*` для одного и того
//     же `*Document` сериализуются через `doc.mu`.
func SaveKeeper(path string, doc *Document) ([]diag.Diagnostic, error) {
	return saveTo(path, doc)
}

// SaveSoul — round-trip write-back SoulConfig. См. `SaveKeeper` для контракта,
// включая семантику thread-safety и набор возвращаемых error-ов.
func SaveSoul(path string, doc *Document) ([]diag.Diagnostic, error) {
	return saveTo(path, doc)
}

// SaveKeeperToBytes — рендер без записи на диск (для тестов и in-memory
// pipelines, где caller сам организует I/O).
//
// Для немутированного документа возвращает `doc.source` как есть —
// гарантия byte-identical round-trip с Load (golden tests).
// Для мутированного — рендерит `doc.file.String()` и поднимает
// `round_trip_warning`, если рендер отличается от исходника.
func SaveKeeperToBytes(doc *Document) ([]byte, []diag.Diagnostic, error) {
	return renderBytes(doc)
}

// SaveSoulToBytes — то же для SoulConfig.
func SaveSoulToBytes(doc *Document) ([]byte, []diag.Diagnostic, error) {
	return renderBytes(doc)
}

// renderBytes — общая часть для обоих *ToBytes: тип SoulConfig vs KeeperConfig
// на рендер AST никак не влияет (YAML — один формат), функции типизированы
// раздельно для симметрии API и будущей дивергенции.
func renderBytes(doc *Document) ([]byte, []diag.Diagnostic, error) {
	if doc == nil {
		return nil, nil, errors.New("config: Document is nil")
	}
	doc.mu.Lock()
	defer doc.mu.Unlock()

	if !doc.mutated {
		// Не было ни одного Patch — отдаём исходник как есть, без рендера
		// AST. Это единственный путь, на котором round-trip гарантированно
		// byte-identical (goccy stringer нормализует flow-style, тримит
		// тривиальные пробелы).
		out := make([]byte, len(doc.source))
		copy(out, doc.source)
		return out, nil, nil
	}

	if doc.file == nil {
		return nil, nil, errors.New("config: Document has no AST file (parse failed)")
	}

	rendered := doc.file.String()
	if len(rendered) == 0 || rendered[len(rendered)-1] != '\n' {
		rendered += "\n"
	}
	out := []byte(rendered)

	var diags []diag.Diagnostic
	if !bytes.Equal(out, doc.source) {
		diags = append(diags, diag.Diagnostic{
			Level:   diag.LevelWarning,
			Phase:   diag.PhaseWriteBack,
			File:    doc.path,
			Code:    "round_trip_warning",
			Message: "rendered YAML differs from source (flow-style restyled / inline-comment whitespace normalized / similar AST-stringer artifact)",
			Hint:    "if format preservation is critical, restrict mutations to block-style values",
		})
	}
	return out, diags, nil
}

// saveTo — общая часть SaveKeeper/SaveSoul. Renders bytes, проверяет symlink,
// атомарно пишет.
func saveTo(path string, doc *Document) ([]diag.Diagnostic, error) {
	if path == "" {
		return nil, errors.New("config: empty save path")
	}
	out, diags, err := renderBytes(doc)
	if err != nil {
		return diags, err
	}

	writeDiags, writeErr := writeFileAtomically(path, out)
	diags = append(diags, writeDiags...)
	return diags, writeErr
}

// writeFileAtomically реализует 9-шаговый pipeline (см. doc-comment файла).
func writeFileAtomically(path string, data []byte) ([]diag.Diagnostic, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// (1+8) Symlink-check: Lstat обнаруживает symlink, не следуя ему.
	// Отвергаем до создания tmp — пользователь должен явно решить, что
	// делать с симлинком (create-on-write / follow — отложено в M0.2.5).
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return []diag.Diagnostic{{
				Level:   diag.LevelError,
				Phase:   diag.PhaseWriteBack,
				File:    path,
				Code:    "symlink_write_not_supported",
				Message: "refusing to write through a symlink",
				Hint:    "resolve the symlink target manually or replace it with a regular file",
			}}, fmt.Errorf("config: refusing to write through symlink %q", path)
		}
	}

	// (1) Stat dst → mode/uid/gid (если файл существует). При отсутствии
	// файла используем mode 0644 (типичный POSIX default для конфигов).
	// Любой stat-error, отличный от `IsNotExist` (например, EACCES на parent
	// directory) — fatal: молча падать на 0o644 нельзя, это потенциальное
	// расширение прав.
	var (
		dstMode os.FileMode = 0o644
		dstStat os.FileInfo
	)
	s, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		dstStat = s
		dstMode = s.Mode().Perm()
	case os.IsNotExist(statErr):
		// файл создаём заново — оставляем default 0o644
	default:
		return []diag.Diagnostic{atomicRenameDiag(path, statErr, "stat dst")}, fmt.Errorf("config: cannot stat dst %q: %w", path, statErr)
	}

	// (2) CreateTemp в той же директории — иначе rename мог бы пересечь
	// файловые системы (EXDEV) и потерять атомарность.
	tmp, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return []diag.Diagnostic{atomicRenameDiag(path, err, "create temp")}, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpName)
	}

	// (3) Chmod tmp ДО Write. CreateTemp создаёт файл с mode 0600 — если
	// читатель откроет tmp между Write и Rename, он получит более узкие
	// права, чем у исходника. Меняем mode сразу.
	if err := os.Chmod(tmpName, dstMode); err != nil {
		_ = tmp.Close()
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "chmod temp")}, err
	}

	// (4) Chown tmp на uid/gid исходника, если процесс может (best-effort).
	if dstStat != nil {
		if uid, gid, ok := statOwner(dstStat); ok {
			if err := os.Chown(tmpName, uid, gid); err != nil {
				// EPERM при отсутствии CAP_CHOWN — не fatal: tmp получит
				// effective uid процесса. Прочее — поднимаем warning.
				if !errors.Is(err, os.ErrPermission) {
					_ = tmp.Close()
					cleanup()
					return []diag.Diagnostic{atomicRenameDiag(path, err, "chown temp")}, err
				}
			}
		}
	}

	// (5) Write.
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "write temp")}, err
	}

	// (6) Sync содержимого.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "fsync temp")}, err
	}

	// (7) Close.
	if err := tmp.Close(); err != nil {
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "close temp")}, err
	}

	// (8) Symlink-recheck перед Rename — TOCTOU защита: между первой
	// Lstat-проверкой и Rename злоумышленник мог подложить симлинк.
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			cleanup()
			return []diag.Diagnostic{{
				Level:   diag.LevelError,
				Phase:   diag.PhaseWriteBack,
				File:    path,
				Code:    "symlink_write_not_supported",
				Message: "refusing to write through a symlink (TOCTOU race detected)",
			}}, fmt.Errorf("config: symlink appeared at %q between checks", path)
		}
	}

	// (8.b) Rename.
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "rename")}, err
	}

	// (9) fsync директории-родителя (best-effort: на некоторых файловых
	// системах / ОС directory fsync не поддерживается).
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}

	return nil, nil
}

// atomicRenameDiag — единый помощник для нумерованного error-кода
// `atomic_rename_failed` с указанием конкретной фазы pipeline-а в Hint.
func atomicRenameDiag(path string, err error, stage string) diag.Diagnostic {
	return diag.Diagnostic{
		Level:   diag.LevelError,
		Phase:   diag.PhaseWriteBack,
		File:    path,
		Code:    "atomic_rename_failed",
		Message: err.Error(),
		Hint:    "stage: " + stage,
	}
}
