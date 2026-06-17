package url

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// applyFetched реализует state `fetched`.
//
// Идемпотентность и порядок действий:
//   - checksum задан + path существует и совпадает по хэшу → no-op (НЕ качаем);
//   - checksum задан, хэш не совпал / файла нет → скачать во temp → verify →
//     atomic rename (verify mismatch → failed, temp удаляется, целевой путь
//     не трогается, supply-chain);
//   - checksum НЕ задан → всегда скачать во temp → сравнить SHA-256 с
//     существующим → записать только при diff (корректный changed).
//
// Скачивание всегда идёт во временный файл в директории path, материализация —
// rename (util.AtomicWrite-паттерн): на целевом пути никогда не возникает
// частичного или неверифицированного файла.
func (m *Module) applyFetched(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest) error {
	allowHTTP, err := util.OptBoolParam(req.Params, "allow_http")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	insecureSkipVerify, err := util.OptBoolParam(req.Params, "insecure_skip_verify")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	allowPrivate, err := util.OptBoolParam(req.Params, "allow_private")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	rawURL, err := util.StringParam(req.Params, "url")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if verr := util.ValidateFetchURL(rawURL, allowHTTP); verr != nil {
		return util.SendFailed(stream, verr.Error())
	}
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	checksum, err := util.OptStringParam(req.Params, "checksum")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	modeStr, err := util.OptStringParam(req.Params, "mode")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	owner, err := util.OptStringParam(req.Params, "owner")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	group, err := util.OptStringParam(req.Params, "group")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	headers, err := util.OptStringMapParam(req.Params, "headers")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	timeoutStr, err := util.OptStringParam(req.Params, "timeout")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	mode, err := util.ParseMode(modeStr)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	timeout := defaultTimeout
	if timeoutStr != "" {
		timeout, err = parseTimeout(timeoutStr)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}

	var algo, wantHex string
	if checksum != "" {
		algo, wantHex, err = parseChecksum(checksum)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}

	// Клиент строится per-Apply из распарсенных opt-out-флагов: каждый флаг
	// ослабляет независимый контур (схема / dial-guard / TLS-цепочка).
	clientOpts := util.HTTPClientOpts{
		AllowPrivate:       allowPrivate,
		InsecureSkipVerify: insecureSkipVerify,
		AllowHTTPRedirect:  allowHTTP,
	}
	// При снятом guard — warning (только host) в output финального события:
	// оператор видит факт ослабления контура в RunResult (конвенция
	// core.repo/core.http). Формулировки и host-маскинг — единый helper util.
	warnings := util.GuardWarnings(util.WarnHost(rawURL), clientOpts)
	client := m.NewClient(clientOpts)

	// Состояние существующего файла. Для checksum-ветки хэшируем по заявленному
	// алгоритму; для бесчексумной ветки — всегда SHA-256 (формат output).
	existingHashHex, existingExists, herr := hashExisting(path, hashAlgoFor(algo))
	if herr != nil {
		return util.SendFailed(stream, herr.Error())
	}

	// checksum задан + существующий файл уже совпадает → контент не качаем, но
	// mode/owner всё равно приводим к декларации (convergence, как в rendered).
	if checksum != "" && existingExists && strings.EqualFold(existingHashHex, wantHex) {
		attrChanged, aerr := m.converge(path, modeStr, mode, owner, group)
		if aerr != nil {
			return util.SendFailed(stream, aerr.Error())
		}
		sha, _ := canonicalSHA256(path, algo, existingHashHex)
		return finalOutput(stream, attrChanged, path, rawURL, sha, fileSize(path), warnings)
	}

	// Скачиваем во временный файл в директории path, считая хэш на лету.
	// notModified=true означает 304 на conditional-GET (If-None-Match через
	// headers): тело не скачано, temp не создан.
	tmpName, sha256Hex, algoHex, size, notModified, derr := m.download(stream.Context(), client, rawURL, path, headers, timeout, algo)
	if derr != nil {
		return util.SendFailed(stream, derr.Error())
	}

	// 304 Not Modified: оператор задал условный GET (If-None-Match/If-Modified-Since
	// в headers) и сервер подтвердил, что локальная копия актуальна.
	if notModified {
		if !existingExists {
			// Сервер сказал «не изменилось», но локального файла нет: stale
			// If-None-Match без кэша. Скачивать нельзя (тела нет) — fail-fast.
			return util.SendFailed(stream, fmt.Sprintf(
				"server returned 304 but no local file at %s: stale If-None-Match without cache", path))
		}
		// Контент актуален → запись не нужна, но mode/owner приводим к декларации.
		attrChanged, aerr := m.converge(path, modeStr, mode, owner, group)
		if aerr != nil {
			return util.SendFailed(stream, aerr.Error())
		}
		sha, serr := canonicalSHA256(path, algo, existingHashHex)
		if serr != nil {
			return util.SendFailed(stream, serr.Error())
		}
		return finalOutput(stream, attrChanged, path, rawURL, sha, fileSize(path), warnings)
	}

	cleanup := func() { _ = os.Remove(tmpName) }

	// Verify ДО публикации. Mismatch → failed, temp удаляется, целевой путь
	// не трогается. Неверный хэш не материализуется никогда.
	if checksum != "" && !strings.EqualFold(algoHex, wantHex) {
		cleanup()
		return util.SendFailed(stream, fmt.Sprintf(
			"checksum mismatch for %s: want %s:%s, got %s:%s", rawURL, algo, wantHex, algo, algoHex))
	}

	// Бесчексумная ветка: если содержимое совпало с существующим — запись не
	// нужна, но mode/owner всё равно сверяем/правим (convergence). Сравнение по
	// SHA-256.
	if checksum == "" && existingExists && strings.EqualFold(existingHashHex, sha256Hex) {
		cleanup()
		attrChanged, aerr := m.converge(path, modeStr, mode, owner, group)
		if aerr != nil {
			return util.SendFailed(stream, aerr.Error())
		}
		return finalOutput(stream, attrChanged, path, rawURL, sha256Hex, size, warnings)
	}

	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return util.SendFailed(stream, fmt.Sprintf("chmod temp for %s: %v", path, err))
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return util.SendFailed(stream, fmt.Sprintf("rename %s -> %s: %v", tmpName, path, err))
	}

	if owner != "" || group != "" {
		if _, oerr := util.ApplyOwnership(path, owner, group, m.LookupUser, m.LookupGroup); oerr != nil {
			return util.SendFailed(stream, oerr.Error())
		}
	}

	return finalOutput(stream, true, path, rawURL, sha256Hex, size, warnings)
}

// converge приводит mode/owner существующего файла к декларации, когда контент
// уже совпал и скачивание/перезапись не нужны. Семантика 1:1 с rendered
// ([file/rendered.go]): mode сверяется и правится только при заданном modeStr
// (пустой mode не навязывает дефолт существующему файлу); owner/group — через
// util.ApplyOwnership. changed = mode-diff || owner-diff.
func (m *Module) converge(path, modeStr string, mode fs.FileMode, owner, group string) (bool, error) {
	modeChanged := false
	if modeStr != "" {
		info, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != mode {
			if cerr := os.Chmod(path, mode); cerr != nil {
				return false, fmt.Errorf("chmod %s: %v", path, cerr)
			}
			modeChanged = true
		}
	}

	ownerChanged := false
	if owner != "" || group != "" {
		changed, oerr := util.ApplyOwnership(path, owner, group, m.LookupUser, m.LookupGroup)
		if oerr != nil {
			return false, oerr
		}
		ownerChanged = changed
	}

	return modeChanged || ownerChanged, nil
}

// download выполняет GET во временный файл рядом с path, считая параллельно
// SHA-256 (для output) и, если algo задан и не sha256, хэш по algo (для verify).
// Возвращает имя temp-файла, SHA-256-хэш, algo-хэш (=sha256Hex если algo пуст
// или sha256), размер и флаг notModified. На любой ошибке temp удаляется внутри.
//
// notModified=true (HTTP 304) обрабатывается ДО проверки 2xx: при conditional-GET
// (If-None-Match/If-Modified-Since в headers) 304 — штатный ответ «не изменилось»,
// тело не качается, temp не создаётся. Решение о no-op/ошибке принимает
// вызывающий (зависит от наличия локального файла).
//
// client — per-call HTTP-клиент, построенный из opt-out-флагов задачи.
// headers применяются к запросу, но НИКОГДА не логируются и не возвращаются
// (sensitive-by-construction, [ADR-010] §7.4).
func (m *Module) download(
	ctx context.Context, client util.HTTPDoer, rawURL, path string, headers map[string]string,
	timeout time.Duration, algo string,
) (tmpName, sha256Hex, algoHex string, size int64, notModified bool, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", "", 0, false, fmt.Errorf("build request for %s: %v", rawURL, err)
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", "", "", 0, false, fmt.Errorf("fetch %s: %v", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 304 — раньше проверки 2xx: conditional-GET (If-None-Match) штатно отвечает
	// 304, это не «unexpected status».
	if resp.StatusCode == http.StatusNotModified {
		return "", "", "", 0, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", 0, false, fmt.Errorf("fetch %s: unexpected status %d", rawURL, resp.StatusCode)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", "", "", 0, false, fmt.Errorf("create temp for %s: %v", path, err)
	}
	tmpName = tmp.Name()

	sha := sha256.New()
	var extra hash.Hash
	if algo != "" && algo != "sha256" {
		extra = hashAlgoFor(algo)
	}
	writers := []io.Writer{tmp, sha}
	if extra != nil {
		writers = append(writers, extra)
	}

	n, copyErr := io.Copy(io.MultiWriter(writers...), resp.Body)
	if copyErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", "", 0, false, fmt.Errorf("download %s: %v", rawURL, copyErr)
	}
	if syncErr := tmp.Sync(); syncErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", "", 0, false, fmt.Errorf("fsync temp for %s: %v", path, syncErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		_ = os.Remove(tmpName)
		return "", "", "", 0, false, fmt.Errorf("close temp for %s: %v", path, closeErr)
	}

	sha256Hex = hex.EncodeToString(sha.Sum(nil))
	algoHex = sha256Hex
	if extra != nil {
		algoHex = hex.EncodeToString(extra.Sum(nil))
	}
	return tmpName, sha256Hex, algoHex, n, false, nil
}

// parseChecksum разбирает форму "<algo>:<hex>". Поддерживаются sha256 и sha1
// (md5 сознательно НЕ поддержан — слаб для supply-chain). hex проверяется на
// корректную длину и алфавит.
func parseChecksum(s string) (algo, hexDigest string, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("param %q: expected \"<algo>:<hex>\", got %q", "checksum", s)
	}
	algo = strings.ToLower(strings.TrimSpace(parts[0]))
	hexDigest = strings.TrimSpace(parts[1])

	var wantLen int
	switch algo {
	case "sha256":
		wantLen = sha256.Size * 2
	case "sha1":
		wantLen = sha1.Size * 2
	default:
		return "", "", fmt.Errorf("param %q: unsupported algo %q (want sha256|sha1)", "checksum", algo)
	}
	if len(hexDigest) != wantLen {
		return "", "", fmt.Errorf("param %q: %s hex must be %d chars, got %d", "checksum", algo, wantLen, len(hexDigest))
	}
	if _, derr := hex.DecodeString(hexDigest); derr != nil {
		return "", "", fmt.Errorf("param %q: invalid hex digest %q", "checksum", hexDigest)
	}
	return algo, hexDigest, nil
}

// hashAlgoFor возвращает hash.Hash для алгоритма checksum. Пустой algo или
// неизвестный → SHA-256 (бесчексумная ветка работает по SHA-256).
func hashAlgoFor(algo string) hash.Hash {
	if algo == "sha1" {
		return sha1.New()
	}
	return sha256.New()
}

// hashExisting хэширует существующий файл переданным хэшем. Возвращает hex,
// флаг существования и ошибку. Отсутствие файла → ("", false, nil).
func hashExisting(path string, h hash.Hash) (string, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(h, f); err != nil {
		return "", false, fmt.Errorf("read %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

// canonicalSHA256 возвращает SHA-256-хэш файла для output. В no-op-ветке с
// checksum по sha1 уже посчитанный sha1-хэш для output не годится — пересчитываем
// SHA-256 файла. Если algo пуст или sha256 — возвращаем уже посчитанный хэш.
func canonicalSHA256(path, algo, existingHashHex string) (string, error) {
	if algo == "" || algo == "sha256" {
		return existingHashHex, nil
	}
	sha, _, err := hashExisting(path, sha256.New())
	if err != nil {
		return "", err
	}
	return sha, nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// finalOutput собирает output модуля. headers в output НЕ включаются никогда
// (sensitive-by-construction). url — эхо без headers. warnings (если есть) —
// host-only guard-предупреждения от util.GuardWarnings, доходят до оператора
// в RunResult.
func finalOutput(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, path, rawURL, sha256Hex string, size int64, warnings []string) error {
	out := map[string]any{
		"path":    path,
		"url":     rawURL,
		"sha256":  sha256Hex,
		"size":    size,
		"changed": changed,
		"fetched": true,
	}
	if len(warnings) > 0 {
		out["warnings"] = util.StringsToAny(warnings)
	}
	return util.SendFinal(stream, changed, out)
}
