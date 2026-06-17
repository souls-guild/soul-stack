package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// applyPresent материализует описание репозитория для выбранного backend-а.
// Идемпотентность: целевой файл + (для apt) ключ совпадают → changed=false.
func (m *Module) applyPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	if p.uri == "" {
		return util.SendFailed(stream, `param "uri": required for state present`)
	}
	if serr := validateURIScheme(p.uri); serr != nil {
		return util.SendFailed(stream, serr.Error())
	}

	switch mgr {
	case util.PkgMgrApt:
		return m.applyAptPresent(stream, mgr, p)
	case util.PkgMgrDnf, util.PkgMgrYum:
		return m.applyYumPresent(stream, mgr, p)
	case util.PkgMgrApk:
		return m.applyApkPresent(stream, mgr, p)
	default:
		return util.SendFailed(stream, fmt.Sprintf("core.repo: unsupported package manager %q", mgr))
	}
}

// applyAbsent удаляет описание репозитория. GPG-ключ НЕ удаляется намеренно: он
// может использоваться другими репозиториями; ручная чистка ключа — отдельный
// явный шаг оператора.
func (m *Module) applyAbsent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	switch mgr {
	case util.PkgMgrApt:
		return m.removeFile(stream, filepath.Join(m.AptSourcesDir, p.name+".list"))
	case util.PkgMgrDnf, util.PkgMgrYum:
		return m.removeFile(stream, filepath.Join(m.YumReposDir, p.name+".repo"))
	case util.PkgMgrApk:
		return m.applyApkAbsent(stream, p)
	default:
		return util.SendFailed(stream, fmt.Sprintf("core.repo: unsupported package manager %q", mgr))
	}
}

// --- apt ---

// applyAptPresent пишет /etc/apt/sources.list.d/<name>.list в современном
// deb822-простом one-line формате с signed-by= на keyring. Ключ (если задан)
// материализуется в /etc/apt/keyrings/<name>.gpg.
func (m *Module) applyAptPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	listPath := filepath.Join(m.AptSourcesDir, p.name+".list")
	keyPath := filepath.Join(m.AptKeyringsDir, p.name+".gpg")

	var warnings []string
	addRepoWarnings(&warnings, mgr, p)

	// Ключ кладём первым: .list ссылается на keyPath через signed-by=, и
	// idempotency content-сравнения .list завязано на наличие этой ссылки.
	keyChanged := false
	if p.gpgKey != "" {
		ch, kerr := m.ensureKey(keyPath, p.gpgKey)
		if kerr != nil {
			return util.SendFailed(stream, kerr.Error())
		}
		keyChanged = ch
	}

	wantContent := aptListContent(p, keyPath)
	fileChanged, ferr := m.ensureFile(listPath, wantContent)
	if ferr != nil {
		return util.SendFailed(stream, ferr.Error())
	}

	return finalOutput(stream, fileChanged || keyChanged, map[string]any{
		"name":    p.name,
		"backend": "apt",
		"path":    listPath,
	}, warnings)
}

// aptListContent собирает одну строку sources.list.d. Формат:
//
//	deb [signed-by=<keyPath> arch=...] <uri> <suite> <components...>
//
// signed-by присутствует только если ключ задан (привязка доверия к репо).
// enabled=false → строка закомментирована (apt не имеет enabled-флага в
// one-line формате; стандартная практика — comment-out).
func aptListContent(p repoParams, keyPath string) []byte {
	var opts []string
	if p.gpgKey != "" {
		opts = append(opts, "signed-by="+keyPath)
	}
	var b strings.Builder
	if !p.enabled {
		b.WriteString("# ")
	}
	b.WriteString("deb ")
	if len(opts) > 0 {
		b.WriteString("[" + strings.Join(opts, " ") + "] ")
	}
	b.WriteString(p.uri)
	if p.suite != "" {
		b.WriteString(" " + p.suite)
	}
	if len(p.components) > 0 {
		b.WriteString(" " + strings.Join(p.components, " "))
	}
	b.WriteString("\n")
	return []byte(b.String())
}

// --- dnf / yum ---

// applyYumPresent пишет /etc/yum.repos.d/<name>.repo в ini-формате.
func (m *Module) applyYumPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	repoPath := filepath.Join(m.YumReposDir, p.name+".repo")

	var warnings []string
	addRepoWarnings(&warnings, mgr, p)

	wantContent := yumRepoContent(p)
	changed, ferr := m.ensureFile(repoPath, wantContent)
	if ferr != nil {
		return util.SendFailed(stream, ferr.Error())
	}

	return finalOutput(stream, changed, map[string]any{
		"name":    p.name,
		"backend": "yum",
		"path":    repoPath,
	}, warnings)
}

// yumRepoContent собирает ini-секцию репозитория. gpgcheck/enabled — 0/1.
// gpgkey пишется только если gpg_key задан (для yum это URL или file:// путь;
// мы пишем значение как есть — оператор задаёт URL ключа).
func yumRepoContent(p repoParams) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", p.name)
	fmt.Fprintf(&b, "name=%s\n", p.name)
	fmt.Fprintf(&b, "baseurl=%s\n", p.uri)
	fmt.Fprintf(&b, "enabled=%s\n", boolDigit(p.enabled))
	fmt.Fprintf(&b, "gpgcheck=%s\n", boolDigit(p.gpgCheck))
	if p.gpgKey != "" {
		fmt.Fprintf(&b, "gpgkey=%s\n", p.gpgKey)
	}
	return []byte(b.String())
}

// --- apk ---

// applyApkPresent добавляет/обновляет строку в /etc/apk/repositories.
// apk хранит репозитории построчно; idempotency — наличие точной строки.
// Формат строки: `<uri>` (apk не использует suite/components в URL — оператор
// кладёт полный URL в uri). enabled=false → строка закомментирована.
func (m *Module) applyApkPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	wantLine := apkLine(p)
	changed, err := m.upsertApkLine(wantLine)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	var warnings []string
	// apk gpg_check на уровне строки репозитория не выражается (ключи лежат в
	// /etc/apk/keys/); gpg_check=false по-прежнему предупреждаем для симметрии.
	addRepoWarnings(&warnings, mgr, p)

	return finalOutput(stream, changed, map[string]any{
		"name":    p.name,
		"backend": "apk",
		"path":    m.ApkReposFile,
	}, warnings)
}

// applyApkAbsent удаляет строку репозитория. apk не хранит имя репо в файле,
// поэтому идентичность — uri: absent для apk ТРЕБУЕТ uri (в отличие от
// apt/yum, где есть файл <name>). Без uri удаление было бы угадыванием и риском
// снести чужую строку.
func (m *Module) applyApkAbsent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], p repoParams) error {
	if p.uri == "" {
		return util.SendFailed(stream, `param "uri": required for apk repo absent (apk has no per-repo file, removal matches by uri)`)
	}
	changed, err := m.removeApkLine(p.uri)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return finalOutput(stream, changed, map[string]any{
		"name":    p.name,
		"backend": "apk",
		"path":    m.ApkReposFile,
	}, nil)
}

func apkLine(p repoParams) string {
	if !p.enabled {
		return "# " + p.uri
	}
	return p.uri
}

// upsertApkLine идемпотентно вставляет/обновляет строку репозитория. Имя репо в
// apk не хранится в файле, поэтому совпадение ищется по uri (с учётом
// возможного `# ` префикса). Если строка уже точно равна want — no-op.
func (m *Module) upsertApkLine(want string) (bool, error) {
	lines, err := readLines(m.ApkReposFile)
	if err != nil {
		return false, err
	}
	wantBare := strings.TrimSpace(strings.TrimPrefix(want, "# "))
	for i, l := range lines {
		bare := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "# "))
		if bare == wantBare {
			if l == want {
				return false, nil
			}
			lines[i] = want
			return true, m.writeLines(m.ApkReposFile, lines)
		}
	}
	lines = append(lines, want)
	return true, m.writeLines(m.ApkReposFile, lines)
}

// removeApkLine удаляет строку с заданным uri (учитывая возможный `# `
// префикс закомментированной строки). Возвращает changed.
func (m *Module) removeApkLine(uri string) (bool, error) {
	lines, err := readLines(m.ApkReposFile)
	if err != nil {
		return false, err
	}
	out := make([]string, 0, len(lines))
	changed := false
	for _, l := range lines {
		bare := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "# "))
		if bare == uri {
			changed = true
			continue
		}
		out = append(out, l)
	}
	if !changed {
		return false, nil
	}
	return true, m.writeLines(m.ApkReposFile, out)
}

// --- общие файловые операции ---

// ensureFile пишет content в path, если файла нет или содержимое отличается.
// Запись — preserve-by-default (util.AtomicWritePreserving): права/владелец
// существующего файла сохраняются. Возвращает changed.
func (m *Module) ensureFile(path string, content []byte) (bool, error) {
	cur, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if existed && string(cur) == string(content) {
		return false, nil
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return false, fmt.Errorf("mkdir %s: %v", filepath.Dir(path), mkErr)
	}
	if werr := util.AtomicWritePreserving(path, content, "", "", "", m.LookupUser, m.LookupGroup); werr != nil {
		return false, werr
	}
	return true, nil
}

// ensureKey материализует GPG-ключ в keyPath, если его нет или содержимое
// отличается. gpgKey трактуется как inline-содержимое ключа (PEM/ASCII-armored
// или бинарный keyring — пишем как есть). Ключ критичен для supply-chain.
//
// Замечание: gpgKey-как-URL (скачать ключ по https) в MVP НЕ реализуется —
// download-by-URL это отдельный модуль core.url; здесь ключ передаётся inline
// (CEL может подставить содержимое через ${ file(...) } или vault). Это
// сознательное ограничение MVP, расширяемо позже.
func (m *Module) ensureKey(keyPath, gpgKey string) (bool, error) {
	cur, existed, err := readFile(keyPath)
	if err != nil {
		return false, err
	}
	want := []byte(gpgKey)
	if existed && string(cur) == string(want) {
		return false, nil
	}
	if mkErr := os.MkdirAll(filepath.Dir(keyPath), 0o755); mkErr != nil {
		return false, fmt.Errorf("mkdir %s: %v", filepath.Dir(keyPath), mkErr)
	}
	// Ключ читается apt-ом — mode 0644 (мир может читать публичный ключ).
	if werr := util.AtomicWrite(keyPath, want, 0o644); werr != nil {
		return false, werr
	}
	return true, nil
}

func (m *Module) removeFile(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], path string) error {
	_, existed, err := readFile(path)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !existed {
		return finalOutput(stream, false, map[string]any{"path": path}, nil)
	}
	if rerr := os.Remove(path); rerr != nil {
		return util.SendFailed(stream, fmt.Sprintf("remove %s: %v", path, rerr))
	}
	return finalOutput(stream, true, map[string]any{"path": path}, nil)
}

// addRepoWarnings добавляет обязательные warning-и opt-out-ов (gpg_check=false,
// http uri) — симметрично checksum-opt-out в core.url. Warning попадает в
// output (паттерн core.line), а не теряется.
func addRepoWarnings(warnings *[]string, mgr util.PkgMgr, p repoParams) {
	if !p.gpgCheck {
		*warnings = append(*warnings, fmt.Sprintf("repo %q: gpg_check disabled — packages will NOT be cryptographically verified (supply-chain risk)", p.name))
	}
	if p.gpgCheck && p.gpgKey == "" {
		*warnings = append(*warnings, fmt.Sprintf("repo %q: gpg_check enabled but no gpg_key set — %s", p.name, gpgNoKeyDetail(mgr)))
	}
	if isHTTP(p.uri) {
		*warnings = append(*warnings, fmt.Sprintf("repo %q: uri uses plain http:// — traffic is unencrypted (use https unless this is a trusted internal mirror)", p.name))
	}
}

// gpgNoKeyDetail возвращает backend-специфичное продолжение warning-а
// «gpg_check enabled but no gpg_key set». dnf/yum жёстко требуют gpgkey= при
// gpgcheck=1 (иначе установка падает); apt и apk опираются на свои хранилища
// доверия (/etc/apt/keyrings + global keyring, /etc/apk/keys).
func gpgNoKeyDetail(mgr util.PkgMgr) string {
	switch mgr {
	case util.PkgMgrDnf, util.PkgMgrYum:
		return "gpgcheck=1 without gpgkey will fail package install on the host"
	case util.PkgMgrApk:
		return "signature verification relies on keys in /etc/apk/keys"
	default: // apt и прочее
		return "signature verification relies on the system/global trust store"
	}
}

func boolDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func readFile(path string) (content []byte, existed bool, err error) {
	b, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		return b, true, nil
	case errors.Is(rerr, fs.ErrNotExist):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("read %s: %v", path, rerr)
	}
}

func readLines(path string) ([]string, error) {
	data, _, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// writeLines пишет /etc/apk/repositories построчно. Это in-place правка
// существующего файла, поэтому запись — preserve-by-default
// (util.AtomicWritePreserving): права/владелец существующего файла сохраняются
// (симметрично ensureFile для apt/yum и fstab в core.mount).
func (m *Module) writeLines(path string, lines []string) error {
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return fmt.Errorf("mkdir %s: %v", filepath.Dir(path), mkErr)
	}
	content := strings.Join(lines, "\n") + "\n"
	if werr := util.AtomicWritePreserving(path, []byte(content), "", "", "", m.LookupUser, m.LookupGroup); werr != nil {
		return werr
	}
	return nil
}

// finalOutput собирает финальный ApplyEvent с changed и (если есть) warnings.
func finalOutput(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, output map[string]any, warnings []string) error {
	output["changed"] = changed
	if len(warnings) > 0 {
		ws := make([]any, len(warnings))
		for i, w := range warnings {
			ws[i] = w
		}
		output["warnings"] = ws
	}
	return util.SendFinal(stream, changed, output)
}
