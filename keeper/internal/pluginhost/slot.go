package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// ErrSlotNotFound — в кеше нет слота `<cacheRoot>/<ns>-<name>/` или в нём нет
// валидного manifest.yaml / бинаря. Возвращается [ReadSlot], когда плагин по
// ключу (namespace, name) не найден в кеше host-а; sigil.Service маппит это в
// ErrPluginNotInCache → 404.
var ErrSlotNotFound = errors.New("pluginhost: plugin slot not found in cache")

// CurrentLink — имя symlink-а на активный commit_sha-слот в R-nested-раскладке
// (ADR-026 F-fetch, A1-S1): `<cacheRoot>/<ns>-<name>/current → <commit_sha>`.
// Резолвер ([plugingit.Resolver]) переставляет его атомарно при наполнении кеша.
const CurrentLink = "current"

// SlotContents — содержимое слота плагина в кеше, прочитанное по ключу
// (namespace, name): путь к бинарю + сырые байты manifest.yaml + SHA-256
// бинаря (hex, lowercase).
//
// Это вход для подписи Sigil (ADR-026): Keeper читает АКТИВНЫЙ бинарь+manifest
// слота `<cacheRoot>/<ns>-<name>/current/` (R-nested layout, A1-S1: `current` —
// symlink на иммутабельный commit_sha-слот, наполняемый git-резолвером).
// `ref` в allow-record — operator-asserted метка, в поиске слота НЕ участвует.
type SlotContents struct {
	// BinaryPath — абсолютный путь к исполняемому файлу плагина.
	BinaryPath string
	// ManifestBytes — СЫРЫЕ байты manifest.yaml как лежат на диске (без
	// канонизации: её делает sigil.Signer перед хешированием — S3↔S6-инвариант).
	ManifestBytes []byte
	// BinarySHA256 — SHA-256 бинаря (hex, lowercase, 64 символа). Подаётся в
	// Signer.Sign и сохраняется в plugin_sigils.sha256.
	BinarySHA256 string
}

// ReadSlot читает бинарь+manifest АКТИВНОГО слота плагина через current-symlink
// `<cacheRoot>/<namespace>-<name>/current/` (R-nested layout, A1-S1). `ref` в
// поиске слота НЕ участвует — авторитет целостности = sha256 + подпись Sigil.
//
// Шаги:
//  1. активный слот `<cacheRoot>/<ns>-<name>/current/` (symlink на commit_sha-
//     каталог); нет / битый symlink → [ErrSlotNotFound];
//  2. читает manifest.yaml сырыми байтами и парсит его (нужен kind → конвенция
//     имени бинаря [sharedplugin.Manifest.BinaryName]); невалидный manifest →
//     ошибка валидации;
//  3. бинарь по конвенции рядом с manifest; нет / не исполняемый →
//     [ErrSlotNotFound];
//  4. потоковый SHA-256 бинаря.
//
// Только чтение: ReadSlot НЕ форкает плагин, НЕ трогает handshake/Discover,
// НЕ пишет sidecar. Контракт [Discover]/[Host.Spawn] не затрагивается.
//
// os.Stat следует symlink-у current, поэтому проверка st.IsDir() работает и для
// активного слота (битый/висячий current даёт ENOENT → [ErrSlotNotFound]).
func ReadSlot(cacheRoot, namespace, name string) (*SlotContents, error) {
	dir := filepath.Join(cacheRoot, namespace+"-"+name, CurrentLink)
	st, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrSlotNotFound, dir)
		}
		return nil, fmt.Errorf("pluginhost: stat plugin slot %q: %w", dir, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrSlotNotFound, dir)
	}

	manifestPath := filepath.Join(dir, sharedplugin.FileName)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: no %s in %s", ErrSlotNotFound, sharedplugin.FileName, dir)
		}
		return nil, fmt.Errorf("pluginhost: read %q: %w", manifestPath, err)
	}

	m, diags := sharedplugin.LoadFromBytes(manifestPath, manifestBytes)
	if err := firstManifestError(diags); err != nil {
		return nil, fmt.Errorf("pluginhost: invalid manifest %q: %w", manifestPath, err)
	}
	binName := m.BinaryName()
	if binName == "" {
		return nil, fmt.Errorf("pluginhost: manifest %q has no binary convention for kind=%q", manifestPath, m.Kind)
	}

	binPath := filepath.Join(dir, binName)
	bst, err := os.Stat(binPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: binary %s missing in %s", ErrSlotNotFound, binName, dir)
		}
		return nil, fmt.Errorf("pluginhost: stat binary %q: %w", binPath, err)
	}
	if bst.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory in %s", ErrSlotNotFound, binName, dir)
	}

	digest, err := fileDigest(binPath)
	if err != nil {
		return nil, err
	}

	return &SlotContents{
		BinaryPath:    binPath,
		ManifestBytes: manifestBytes,
		BinarySHA256:  digest,
	}, nil
}

// SlotCommitSHA читает commit_sha АКТИВНОГО слота плагина (namespace, name) —
// имя каталога, на который указывает symlink `<cacheRoot>/<ns>-<name>/current`
// (R-nested layout, A1-S1). commit_sha — audit-метка происхождения бинаря,
// заполняется в plugin_sigils при allow (ADR-026(g), вне подписи).
//
// Читает ТОЛЬКО target символа (os.Readlink, без следования по нему): target —
// относительная цель `<commit_sha>` (см. [plugingit.updateCurrentSymlink]),
// поэтому возвращается её базовое имя. Чтение лишь target-а, а не stat slot-а,
// делает хелпер дешёвым и независимым от наличия бинаря/manifest (их валидность
// уже проверил [ReadSlot] на шаге allow).
//
// fail-closed:
//   - нет каталога `<ns>-<name>/` или нет/битый symlink `current` (legacy-слот
//     без current, висячая ссылка) → [ErrSlotNotFound];
//   - `current` есть, но это не symlink → [ErrSlotNotFound] (R-nested-инвариант
//     нарушен: current обязан быть символом на commit_sha-каталог).
//
// Возвращает базовое имя target-а как есть (без проверки на 40-hex): валидность
// commit_sha гарантирует git-резолвер при наполнении кеша; здесь — только чтение
// уже зафиксированного значения.
func SlotCommitSHA(cacheRoot, namespace, name string) (string, error) {
	link := filepath.Join(cacheRoot, namespace+"-"+name, CurrentLink)
	target, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrSlotNotFound, link)
		}
		// EINVAL (current — не symlink) и прочие — legacy/повреждённый слот:
		// commit_sha надёжно не извлечь, fail-closed.
		return "", fmt.Errorf("%w: read current symlink %q: %v", ErrSlotNotFound, link, err)
	}
	commitSHA := filepath.Base(target)
	if commitSHA == "" || commitSHA == "." || commitSHA == string(filepath.Separator) {
		return "", fmt.Errorf("%w: empty commit_sha in current symlink %q", ErrSlotNotFound, link)
	}
	return commitSHA, nil
}

// fileDigest считает SHA-256 файла потоково (бинари плагинов — десятки МБ).
// Дубль computeFileDigest из shared/pluginhost (там unexported); локальная
// копия избегает расширения публичной поверхности shared только ради чтения
// слота.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("pluginhost: open binary for digest %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("pluginhost: read binary for digest %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// firstManifestError возвращает первую error-уровневую диагностику из diags
// (диагностики, не дотягивающие до error — warning/hint — игнорируются:
// manifest валиден к подписи). nil — фатальных ошибок нет.
func firstManifestError(diags []diag.Diagnostic) error {
	for _, d := range diags {
		if d.Level == diag.LevelError {
			return errors.New(d.Code + ": " + d.Message)
		}
	}
	return nil
}
