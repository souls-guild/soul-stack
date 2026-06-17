package pluginhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// Discovered — один плагин, найденный в кеше host-а. Бинарь и manifest
// лежат в одной директории (ADR-020(a): «manifest.yaml … рядом с бинарём
// в кеше host-а»).
type Discovered struct {
	// Manifest — распарсенный и провалидированный manifest.yaml.
	Manifest *sharedplugin.Manifest
	// BinaryPath — абсолютный путь к исполняемому файлу плагина.
	BinaryPath string
	// Dir — директория, в которой лежат manifest.yaml и бинарь (для логов).
	Dir string
	// Digest — SHA-256 бинаря (hex), вычисленный при Discover. Используется
	// для логов/OTel-атрибутов; авторитетная integrity-сверка делается в
	// [Host.Spawn] против sidecar (security fix H2). Пустая строка — бинарь
	// не удалось прочитать для digest (попадает в warnings, плагин
	// пропускается).
	Digest string
}

// Discover ищет плагины в корневой директории cacheRoot.
//
// Раскладка кеша на хосте (docs/soul/modules.md, docs/keeper/plugins.md):
//
//	<cacheRoot>/
//	  <namespace>-<name>/
//	    manifest.yaml
//	    soul-mod-<name>         # для kind=soul_module
//	    soul-cloud-<name>       # для kind=cloud_driver
//	    soul-ssh-<name>         # для kind=ssh_provider
//
// Discover **не фильтрует по kind** — это задача caller-а (soul-host
// принимает только soul_module, keeper-host — только cloud_driver и
// ssh_provider). См. [FilterByKinds].
//
// Имя бинаря определяется по конвенции [sharedplugin.Manifest.BinaryName];
// директории, в которых бинарь не найден или не имеет +x, попадают в
// warnings, но не прерывают обход.
//
// Ошибки чтения отдельных директорий не прерывают обход — собираются в
// warnings, и Discover возвращает то, что удалось найти. Только fatal —
// ошибка чтения самого cacheRoot (например, ENOENT).
func Discover(cacheRoot string) ([]Discovered, []string, error) {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("pluginhost: read plugin cache root %q: %w", cacheRoot, err)
	}
	var (
		out      []Discovered
		warnings []string
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found, warns := DiscoverSlot(filepath.Join(cacheRoot, e.Name()))
		out = append(out, found...)
		warnings = append(warnings, warns...)
	}
	return out, warnings, nil
}

// DiscoverSlot читает плагин из ОДНОГО каталога слота `dir` (manifest.yaml +
// бинарь по конвенции BinaryName рядом). Возвращает (0..1 Discovered, warnings):
// невалидный manifest / отсутствующий или не-исполняемый бинарь → пустой
// результат + warning.
//
// Выделена из [Discover] для R-nested-раскладки Keeper-host-а (A1-S1): keeper
// дискаверит через current-symlink (`<ns>-<name>/current`), указывая именно на
// каталог активного слота, а не на корень кеша.
func DiscoverSlot(dir string) ([]Discovered, []string) {
	manifestPath := filepath.Join(dir, sharedplugin.FileName)
	m, diags, ioErr := sharedplugin.Load(manifestPath)
	if ioErr != nil {
		return nil, []string{fmt.Sprintf("skip %s: %v", dir, ioErr)}
	}
	if err := firstDiagError(diags); err != nil {
		return nil, []string{fmt.Sprintf("skip %s: %v", dir, err)}
	}
	binName := m.BinaryName()
	if binName == "" {
		return nil, []string{fmt.Sprintf("skip %s: no binary convention for kind=%q", dir, m.Kind)}
	}
	binPath := filepath.Join(dir, binName)
	st, err := os.Stat(binPath)
	if err != nil {
		return nil, []string{fmt.Sprintf("skip %s: binary %s not found: %v", dir, binName, err)}
	}
	if st.IsDir() {
		return nil, []string{fmt.Sprintf("skip %s: %s is a directory", dir, binName)}
	}
	if st.Mode().Perm()&0o111 == 0 {
		return nil, []string{fmt.Sprintf("skip %s: %s is not executable (mode %o)", dir, binName, st.Mode().Perm())}
	}
	digest, err := computeFileDigest(binPath)
	if err != nil {
		return nil, []string{fmt.Sprintf("skip %s: digest %s: %v", dir, binName, err)}
	}
	return []Discovered{{
		Manifest:   m,
		BinaryPath: binPath,
		Dir:        dir,
		Digest:     digest,
	}}, nil
}

// FilterByKinds оставляет в discovered только плагины, чей manifest.kind
// входит в allowedKinds. Не-проходящие записи попадают в warnings с
// человекочитаемым сообщением. Возвращает (отфильтрованный список, warnings).
//
// Удобно использовать сразу после [Discover]:
//
//	found, w1, err := pluginhost.Discover(root)
//	found, w2 := pluginhost.FilterByKinds(found, []string{sharedplugin.KindSoulModule})
//	warnings := append(w1, w2...)
func FilterByKinds(discovered []Discovered, allowedKinds []string) ([]Discovered, []string) {
	if len(allowedKinds) == 0 {
		return discovered, nil
	}
	allowed := make(map[string]struct{}, len(allowedKinds))
	for _, k := range allowedKinds {
		allowed[k] = struct{}{}
	}
	var (
		out      = make([]Discovered, 0, len(discovered))
		warnings []string
	)
	for _, d := range discovered {
		if _, ok := allowed[d.Manifest.Kind]; ok {
			out = append(out, d)
			continue
		}
		warnings = append(warnings, fmt.Sprintf("skip %s: kind=%q not allowed on this host (want %v)",
			d.Dir, d.Manifest.Kind, allowedKinds))
	}
	return out, warnings
}

// firstDiagError собирает все error-уровневые diag-записи в одну ошибку с
// делителем `; `. Возвращает nil, если диагностики пусты или содержат только
// warning/hint. Дубликат [sharedplugin.Manifest.ValidateSimple]-логики ровно
// потому, что [sharedplugin.Load] отдаёт ошибки структурно через diag, а
// callsite-у discovery нужен `error` для warning-сообщения.
func firstDiagError(ds []diag.Diagnostic) error {
	var msgs []string
	for _, d := range ds {
		if d.Level != diag.LevelError {
			continue
		}
		msgs = append(msgs, d.Code+": "+d.Message)
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "; "))
}
