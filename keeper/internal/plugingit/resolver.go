// Package plugingit — git-резолвер источников Keeper-side плагинов
// (ADR-026 Sigil, подход F-fetch, slice A1-S1).
//
// До A1-S1 каталог `keeper.yml::plugins.{cloud_drivers,ssh_providers}` нёс
// `source`/`ref`, но они НЕ использовались — бинарь в слот кеша попадал вне
// Soul Stack. Резолвер закрывает это: для каждой записи каталога Keeper сам
// git-резолвит `source`+`ref` в commit_sha-слот, извлекая УЖЕ СОБРАННЫЙ бинарь
// из `dist/<binary-name>` (F-fetch — компиляции на Keeper НЕТ).
//
// Раскладка кеша (R-nested layout, A1-S1):
//
//	<cacheRoot>/
//	  <ns>-<name>/
//	    current -> <commit_sha>        # symlink на активный слот (атомарно)
//	    <commit_sha>/                  # иммутабельный слот (commit_sha уникален)
//	      manifest.yaml
//	      soul-cloud-<name>            # или soul-ssh-<name>
//
// git-egress — HIGH security-риск. Git-операции идут через go-git (pure-Go,
// без форка системного `git`): hooks НЕ исполняются by design, транспорта
// `ext::` не существует, submodules не рекурсятся по умолчанию, `file://`
// заперт scheme-allowlist-ом ([validateGitScheme]); clone/fetch — shallow
// (Depth=1) под context-timeout. Подробности hardening-инвариантов — в
// [git.go]. Резолвленный бинарь НЕ исполняется и НЕ помечается доверенным —
// доверие даётся отдельно через `plugin.allow` + Sigil (S3/S4/S6), резолвер
// лишь наполняет кеш.
package plugingit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// currentLink — имя symlink-а на активный commit_sha-слот внутри <ns>-<name>/.
const currentLink = "current"

// Sentinel-ошибки резолва одной записи каталога. ResolveCatalog маппит их в
// per-entry warnings (fail-closed: сломанная запись скипается, Keeper не падает).
var (
	// ErrRefNotResolved — ResolveRevision(<ref>^{commit}) не нашёл коммит (ref
	// не существует ни как тег, ни как ветка, ни как полный hash).
	ErrRefNotResolved = errors.New("plugingit: ref does not resolve to a commit")
	// ErrManifestNotFound — в checkout-е нет manifest.yaml в корне.
	ErrManifestNotFound = errors.New("plugingit: manifest.yaml not found in checkout")
	// ErrArtifactNotFound — ожидаемого собранного бинаря dist/<binary-name> нет
	// или это не обычный файл (F-fetch требует УЖЕ собранный артефакт).
	ErrArtifactNotFound = errors.New("plugingit: built artifact not found in dist/")
	// ErrSourceUnavailable — git clone/fetch/checkout источника провалился
	// (недоступен remote, auth, таймаут).
	ErrSourceUnavailable = errors.New("plugingit: git source unavailable")
	// ErrArtifactTooLarge — бинарь dist/<binary-name> превышает
	// plugins.max_artifact_size_mb. git-egress hardening (ADR-026(g)):
	// враждебный/огромный артефакт не должен забить кеш keeper-host-а. Fail-closed
	// — слот не создаётся.
	ErrArtifactTooLarge = errors.New("plugingit: built artifact exceeds size limit")
	// ErrCloneTooLarge — суммарный размер рабочего дерева клона (checkout + .git)
	// превышает plugins.max_clone_size_mb. git-egress hardening (ADR-026(g)):
	// огромный репозиторий не должен забить work_root. Fail-closed — workdir
	// чистится, слот не создаётся.
	ErrCloneTooLarge = errors.New("plugingit: clone tree exceeds size limit")
)

// DefaultGitTimeout — дефолтный timeout цепочки git-операций резолва
// (clone/fetch → resolve → checkout). Совпадает с config-дефолтом
// [config.DefaultPluginFetchTimeout].
const DefaultGitTimeout = config.DefaultPluginFetchTimeout

// artifactSubdir — подкаталог собранного артефакта в репозитории плагина
// (F-fetch: бинарь уже собран и лежит в dist/, Keeper его не компилирует).
const artifactSubdir = "dist"

// ResolvedSlot — результат успешного резолва одной записи каталога: куда лёг
// иммутабельный слот и чем он идентифицируется.
type ResolvedSlot struct {
	// Namespace / Name — ключ плагина (из manifest checkout-а, не из каталога:
	// каталог несёт только `name`, namespace берётся из самого manifest-а).
	Namespace string
	Name      string
	// Ref — operator-asserted метка из каталога (`ref:`), как есть.
	Ref string
	// CommitSHA — 40-hex commit, в который зарезолвился ref. Идентификатор слота
	// (иммутабелен: один commit_sha → один каталог слота).
	CommitSHA string
	// SlotDir — абсолютный путь иммутабельного слота
	// `<cacheRoot>/<ns>-<name>/<commit_sha>/`.
	SlotDir string
	// BinarySHA256 — SHA-256 (hex, lowercase) бинаря в слоте.
	BinarySHA256 string
}

// Resolver — git-резолвер каталога плагинов. cacheRoot — корень кеша слотов;
// workRoot — корень рабочих клонов (СТРОГО вне cacheRoot, чтобы .git и checkout
// не попадали в кеш-каталог, читаемый Discover/ReadSlot).
//
// maxArtifactSize / maxCloneSize — size-лимиты git-egress hardening (ADR-026(g),
// байты): потолок одного извлекаемого бинаря и суммарного рабочего дерева клона.
// Защита диска keeper-host-а от враждебного/огромного репозитория (timeout
// ограничивает egress по времени, эти — по объёму). Превышение — fail-closed.
type Resolver struct {
	cacheRoot       string
	workRoot        string
	gitTimeout      time.Duration
	maxArtifactSize int64
	maxCloneSize    int64
	logger          *slog.Logger
}

// NewResolver конструирует резолвер. gitTimeout <= 0 → [DefaultGitTimeout].
// maxArtifactSize / maxCloneSize <= 0 → дефолты [config.DefaultPluginMaxArtifactSizeMB]
// / [config.DefaultPluginMaxCloneSizeMB] (резолв симметричен Resolved*-методам
// конфига). logger nil → slog.Default(). Git-операции — go-git (pure-Go, без
// форка системного `git`).
func NewResolver(cacheRoot, workRoot string, gitTimeout time.Duration, maxArtifactSize, maxCloneSize int64, logger *slog.Logger) *Resolver {
	if gitTimeout <= 0 {
		gitTimeout = DefaultGitTimeout
	}
	if maxArtifactSize <= 0 {
		maxArtifactSize = int64(config.DefaultPluginMaxArtifactSizeMB) * bytesPerMiB
	}
	if maxCloneSize <= 0 {
		maxCloneSize = int64(config.DefaultPluginMaxCloneSizeMB) * bytesPerMiB
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{
		cacheRoot:       cacheRoot,
		workRoot:        workRoot,
		gitTimeout:      gitTimeout,
		maxArtifactSize: maxArtifactSize,
		maxCloneSize:    maxCloneSize,
		logger:          logger,
	}
}

// bytesPerMiB — множитель МиБ→байты, локальная копия [config.bytesPerMiB]
// (неэкспортируемая в shared/config), чтобы дефолтить лимиты в байтах без
// import-обхода.
const bytesPerMiB = 1024 * 1024

// ResolveCatalog резолвит весь каталог cloud_drivers + ssh_providers.
// Per-entry ошибки превращаются в warnings (fail-closed): сломанная запись
// скипается, Keeper не падает. Возвращает (успешно зарезолвленные слоты,
// warnings, fatal-ошибку). fatal — только то, что ломает резолв В ПРИНЦИПЕ
// (например, невозможность создать workRoot); nil plugins → пустой результат.
func (r *Resolver) ResolveCatalog(ctx context.Context, plugins *config.KeeperPlugins) ([]ResolvedSlot, []string, error) {
	if plugins == nil {
		return nil, nil, nil
	}
	var (
		slots    []ResolvedSlot
		warnings []string
	)
	entries := make([]config.PluginCatalogEntry, 0, len(plugins.CloudDrivers)+len(plugins.SSHProviders))
	entries = append(entries, plugins.CloudDrivers...)
	entries = append(entries, plugins.SSHProviders...)

	for _, e := range entries {
		slot, err := r.ResolveEntry(ctx, e)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"plugin %q (source=%q ref=%q): %v", e.Name, e.Source, e.Ref, err))
			continue
		}
		slots = append(slots, slot)
	}
	return slots, warnings, nil
}

// ResolveEntry резолвит одну запись каталога в иммутабельный commit_sha-слот.
// Поток (F-fetch — без компиляции):
//
//  1. validateGitScheme(source): allowlist https/ssh/scp (file:// — только под
//     env-флагом); недопустимая схема → ErrSourceUnavailable;
//  2. workdir := <workRoot>/<name>/ (вне cacheRoot, mode 0700);
//     go-git shallow clone (или fetch, если клон уже есть);
//  3. commit_sha := resolveRef(<ref>^{commit}) (40-hex гарантирован типом
//     plumbing.Hash; нерезолвящийся ref → ErrRefNotResolved);
//  4. checkout detached-HEAD на commit_sha (go-git, hooks не исполняются);
//  5. parse <workdir>/manifest.yaml (нет → ErrManifestNotFound) → kind →
//     BinaryName() → ожидаемый dist/<binary-name> (нет/не файл → ErrArtifactNotFound);
//  6. dst := <cacheRoot>/<ns>-<name>/<commit_sha>/; если уже валиден → skip
//     (commit_sha иммутабелен);
//  7. staging-каталог на том же fs → copy(manifest+binary)+fsync → atomic rename;
//  8. atomic-обновление symlink <cacheRoot>/<ns>-<name>/current → <commit_sha>;
//  9. binary_sha256 := sha256(<dst>/<binary-name>).
//
// `<ns>` берётся из manifest-а ПОСЛЕ checkout: до parse namespace неизвестен,
// поэтому workdir именуется по `name` каталога (детерминирован до parse), а
// перенос в namespace-aware слот делается на шаге 6 (cacheRoot), где namespace
// уже прочитан.
func (r *Resolver) ResolveEntry(ctx context.Context, e config.PluginCatalogEntry) (ResolvedSlot, error) {
	ctx, cancel := context.WithTimeout(ctx, r.gitTimeout)
	defer cancel()

	if e.Source == "" {
		return ResolvedSlot{}, fmt.Errorf("%w: empty source", ErrSourceUnavailable)
	}
	if e.Ref == "" {
		return ResolvedSlot{}, fmt.Errorf("%w: empty ref", ErrRefNotResolved)
	}
	if err := validateGitScheme(e.Source); err != nil {
		return ResolvedSlot{}, err
	}

	// workdir именуется по name каталога (namespace ещё не известен до parse).
	workdir := filepath.Join(r.workRoot, sanitizeSegment(e.Name))
	commitSHA, err := r.prepareCheckout(ctx, workdir, e.Source, e.Ref)
	if err != nil {
		return ResolvedSlot{}, err
	}

	manifestPath := filepath.Join(workdir, sharedplugin.FileName)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ResolvedSlot{}, fmt.Errorf("%w: %s", ErrManifestNotFound, manifestPath)
		}
		return ResolvedSlot{}, fmt.Errorf("plugingit: read manifest %q: %w", manifestPath, err)
	}
	m, diags := sharedplugin.LoadFromBytes(manifestPath, manifestBytes)
	if err := firstManifestError(diags); err != nil {
		return ResolvedSlot{}, fmt.Errorf("plugingit: invalid manifest %q: %w", manifestPath, err)
	}
	binName := m.BinaryName()
	if binName == "" {
		return ResolvedSlot{}, fmt.Errorf("plugingit: manifest %q has no binary convention for kind=%q", manifestPath, m.Kind)
	}

	artifactPath := filepath.Join(workdir, artifactSubdir, binName)
	ast, err := os.Stat(artifactPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ResolvedSlot{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, artifactPath)
		}
		return ResolvedSlot{}, fmt.Errorf("plugingit: stat artifact %q: %w", artifactPath, err)
	}
	if !ast.Mode().IsRegular() {
		return ResolvedSlot{}, fmt.Errorf("%w: %s is not a regular file", ErrArtifactNotFound, artifactPath)
	}
	// git-egress hardening (ADR-026(g)): отсекаем огромный бинарь ДО копирования
	// в кеш по os.Stat-размеру (LimitReader на самом copyFileSync — defense-in-
	// depth ниже). Fail-closed: слот не материализуется.
	if ast.Size() > r.maxArtifactSize {
		return ResolvedSlot{}, fmt.Errorf("%w: %s = %d bytes > limit %d bytes", ErrArtifactTooLarge, artifactPath, ast.Size(), r.maxArtifactSize)
	}

	slotKey := m.Namespace + "-" + m.Name
	pluginDir := filepath.Join(r.cacheRoot, slotKey)
	dst := filepath.Join(pluginDir, commitSHA)

	// commit_sha иммутабелен: если слот уже валиден — пропускаем извлечение,
	// только обновляем current и считаем digest существующего бинаря.
	if !r.slotValid(dst, binName) {
		if err := r.materializeSlot(pluginDir, dst, manifestPath, artifactPath, binName, r.maxArtifactSize); err != nil {
			return ResolvedSlot{}, err
		}
	}

	if err := updateCurrentSymlink(pluginDir, commitSHA); err != nil {
		return ResolvedSlot{}, err
	}

	digest, err := fileDigest(filepath.Join(dst, binName))
	if err != nil {
		return ResolvedSlot{}, err
	}

	return ResolvedSlot{
		Namespace:    m.Namespace,
		Name:         m.Name,
		Ref:          e.Ref,
		CommitSHA:    commitSHA,
		SlotDir:      dst,
		BinarySHA256: digest,
	}, nil
}

// prepareCheckout готовит рабочий клон workdir на ref-е и возвращает
// зарезолвленный 40-hex commit_sha. Shallow-клонирует (Depth=1) при отсутствии
// клона, иначе делает shallow fetch ровно ref-а; резолвит ref в commit; затем
// checkout detached-HEAD на этот commit. workdir создаётся mode 0700
// (service-user-only).
//
// transport/auth/таймаут-фейлы clone/fetch → ErrSourceUnavailable;
// нерезолвящийся ref → ErrRefNotResolved (от [resolveRef]).
func (r *Resolver) prepareCheckout(ctx context.Context, workdir, source, ref string) (string, error) {
	if err := os.MkdirAll(r.workRoot, 0o700); err != nil {
		return "", fmt.Errorf("plugingit: mkdir work root %q: %w", r.workRoot, err)
	}

	auth, err := authFor(source)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	repo, err := openOrClone(ctx, workdir, source, auth)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if err := fetch(ctx, repo, auth); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	commitSHA, err := resolveRef(repo, ref)
	if err != nil {
		return "", err
	}
	if err := checkout(repo, commitSHA); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	// git-egress hardening (ADR-026(g)): go-git не даёт byte-cap на clone, поэтому
	// меряем рабочее дерево (checkout + .git) ПОСЛЕ извлечения, но ДО копирования
	// артефакта в кеш. Shallow Depth=1 уже отсекает историю; этот walk ловит
	// огромный рабочий-tree (мусорные файлы / раздутый артефакт). Превышение —
	// fail-closed: чистим workdir, слот не создаётся.
	size, err := dirSize(workdir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if size > r.maxCloneSize {
		_ = os.RemoveAll(workdir)
		return "", fmt.Errorf("%w: %s = %d bytes > limit %d bytes", ErrCloneTooLarge, workdir, size, r.maxCloneSize)
	}
	return commitSHA, nil
}

// dirSize суммирует размер обычных файлов поддерева root (du-подобно, без учёта
// каталогов/симлинков). Прерывается на первой walk-ошибке — частичный обход
// сделал бы лимит-проверку недостоверной.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("plugingit: measure clone tree %q: %w", root, err)
	}
	return total, nil
}

// slotValid — true, если dst уже содержит manifest.yaml и бинарь binName
// (commit_sha-слот иммутабелен; повторный резолв того же коммита — skip).
func (r *Resolver) slotValid(dst, binName string) bool {
	if st, err := os.Stat(filepath.Join(dst, sharedplugin.FileName)); err != nil || st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(dst, binName)); err != nil || !st.Mode().IsRegular() {
		return false
	}
	return true
}

// materializeSlot извлекает manifest+бинарь в иммутабельный слот dst атомарно:
// сборка в staging-каталоге НА ТОМ ЖЕ fs, что dst (rename атомарен только в
// пределах одного fs), fsync файлов, затем os.Rename(staging, dst). artifactMax
// — байт-cap копирования бинаря (ADR-026(g)): copy через LimitReader, при
// превышении staging чистится и возвращается ErrArtifactTooLarge (fail-closed).
func (r *Resolver) materializeSlot(pluginDir, dst, manifestSrc, artifactSrc, binName string, artifactMax int64) error {
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return fmt.Errorf("plugingit: mkdir plugin dir %q: %w", pluginDir, err)
	}
	staging := filepath.Join(pluginDir, ".staging-"+randSuffix())
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("plugingit: mkdir staging %q: %w", staging, err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }

	// manifest без cap (заведомо мелкий, его форму валидирует loader).
	if err := copyFileSync(manifestSrc, filepath.Join(staging, sharedplugin.FileName), 0o644, 0); err != nil {
		cleanup()
		return err
	}
	if err := copyFileSync(artifactSrc, filepath.Join(staging, binName), 0o755, artifactMax); err != nil {
		cleanup()
		return err
	}

	if err := os.Rename(staging, dst); err != nil {
		cleanup()
		// Гонка двух резолверов на один commit_sha: победитель уже создал dst —
		// это не ошибка (слот иммутабелен, содержимое идентично).
		if r.slotValid(dst, binName) {
			return nil
		}
		return fmt.Errorf("plugingit: atomic rename staging→slot %q: %w", dst, err)
	}
	return nil
}

// fileDigest считает SHA-256 файла потоково (бинари плагинов — десятки МБ).
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("plugingit: open binary for digest %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("plugingit: read binary for digest %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFileSync копирует src→dst с заданным mode и fsync (слот должен пережить
// crash до того, как current на него переключится). maxBytes > 0 — байт-cap
// (ADR-026(g) git-egress hardening): копируем через LimitReader(maxBytes+1) и при
// превышении возвращаем ErrArtifactTooLarge fail-closed (dst остаётся в staging,
// который вызывающий чистит). maxBytes <= 0 — без лимита (manifest).
func copyFileSync(src, dst string, mode os.FileMode, maxBytes int64) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("plugingit: open %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("plugingit: create %q: %w", dst, err)
	}
	var reader io.Reader = in
	if maxBytes > 0 {
		// +1 байт: если LimitReader отдал ровно maxBytes+1 — источник больше cap-а.
		reader = io.LimitReader(in, maxBytes+1)
	}
	written, err := io.Copy(out, reader)
	if err != nil {
		_ = out.Close()
		return fmt.Errorf("plugingit: copy %q→%q: %w", src, dst, err)
	}
	if maxBytes > 0 && written > maxBytes {
		_ = out.Close()
		return fmt.Errorf("%w: %s > limit %d bytes", ErrArtifactTooLarge, src, maxBytes)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("plugingit: fsync %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("plugingit: close %q: %w", dst, err)
	}
	// OpenFile с mode подвержен umask — выставляем mode явно.
	if err := os.Chmod(dst, mode); err != nil {
		return fmt.Errorf("plugingit: chmod %q: %w", dst, err)
	}
	return nil
}

// updateCurrentSymlink атомарно переставляет <pluginDir>/current → commitSHA:
// создаёт temp-symlink рядом и os.Rename-ит его на current (rename symlink-а
// атомарен в пределах каталога). Цель — относительная (commitSHA), чтобы слот
// был перемещаемым вместе с pluginDir.
func updateCurrentSymlink(pluginDir, commitSHA string) error {
	tmp := filepath.Join(pluginDir, ".current-"+randSuffix())
	if err := os.Symlink(commitSHA, tmp); err != nil {
		return fmt.Errorf("plugingit: create temp symlink in %q: %w", pluginDir, err)
	}
	if err := os.Rename(tmp, filepath.Join(pluginDir, currentLink)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("plugingit: atomic swap current symlink in %q: %w", pluginDir, err)
	}
	return nil
}

// randSuffix — короткий случайный суффикс для staging/temp-имён (избегаем
// коллизий параллельных резолверов). crypto/rand — не для крипто-стойкости,
// а чтобы два процесса не выбрали один суффикс.
func randSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sanitizeSegment защищает имя path-сегмента от `/`/`..`: в каталог попадает
// только `name` из каталога (kebab-case по manifest-валидации), но workdir
// строится до parse — поэтому страхуемся от path-traversal в config-значении.
func sanitizeSegment(s string) string {
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	s = strings.TrimPrefix(s, ".")
	if s == "" {
		return "_"
	}
	return s
}

// firstManifestError возвращает первую error-уровневую диагностику manifest-а
// (warning/hint игнорируются). nil — фатальных ошибок нет.
func firstManifestError(diags []diag.Diagnostic) error {
	for _, d := range diags {
		if d.Level == diag.LevelError {
			return errors.New(d.Code + ": " + d.Message)
		}
	}
	return nil
}
