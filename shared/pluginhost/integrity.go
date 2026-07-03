package pluginhost

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrPluginDigestMismatch — бинарь плагина не совпал с зафиксированным в
// sidecar SHA-256. Сигнализирует подмену бинаря в кеше host-а после первой
// загрузки (security fix H2, docs/keeper/plugins.md → Integrity-model).
//
// Caller-ы сверяют через errors.Is, чтобы отличить tamper от прочих
// I/O-ошибок Spawn.
var ErrPluginDigestMismatch = errors.New("pluginhost: plugin binary digest mismatch")

// DigestSidecarName — имя sidecar-файла рядом с бинарём плагина в кеше host-а.
// Точкой в начале файл не прячется в общем листинге, но визуально отделён от
// бинаря и manifest.yaml. Экспортировано для install-flow core.module.installed
// (ADR-065): при замене бинаря слота stale-sidecar удаляется до atomic rename.
const DigestSidecarName = ".sha256"

// digestSidecarMode — sidecar пишется read-only: после первой загрузки запись
// в него не предполагается, а защита от подмены digest-а вместе с бинарём
// держится на правах каталога (least-privilege service-user, см.
// docs/keeper/plugins.md → Permissions).
const digestSidecarMode = 0o400

// computeFileDigest считает SHA-256 файла потоково (без чтения целиком в
// память — бинари плагинов могут быть десятки МБ). Возвращает hex-строку
// нижним регистром.
func computeFileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("pluginhost: open for digest %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("pluginhost: read for digest %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifySigilAndSeal — fail-closed verify бинаря плагина против печати доверия
// Sigil (ADR-026, slice S6b), заменивший TOFU-ветку first-load. Возвращает nil
// только если плагин допущен и целостность подтверждена; любой провал →
// *VerifyError (errors.Is(err, ErrSigilVerify)) и плагин НЕ запускается.
//
// Шаги (нормативный порядок, симметричный Keeper-side Sign из keeper/internal/sigil):
//  1. digest бинаря с диска (binDigestHex);
//  2. lookup допуска по (ns, name); rec == nil → fail-closed no_sigil
//     (не-доехавший Sigil = «не допущен», НЕ «ошибка → допустить»);
//  3. пустой набор якорей → fail-closed no_trust_anchor (Sigil не настроен на Keeper);
//  4. сверка digest бинаря с допущенным хешем (binary_sha256) → digest_mismatch;
//  5. manifest_sha256 = SHA-256(NormalizeManifestBytes(rec.Manifest)) — байты ИЗ
//     ТРАНСПОРТА (M1), НЕ файл с диска;
//  6. block = BuildSigilBlock(...) — тот же helper, что Keeper при Sign
//     (симметрия sign↔verify гарантируется компилятором, не второй имплементацией);
//  7. OR-цикл по набору якорей (ADR-026(h), multi-anchor): подпись валидна, если
//     ed25519.Verify подтвердил её ЛЮБОЙ якорь из набора → pass; ни один из
//     непустого набора → bad_signature. OR-семантика даёт безразрывную ротацию
//     ключа подписи (старый и новый anchor сосуществуют в наборе);
//  8. OK → seal sidecar (для re-exec defense-in-depth) + nil.
//
// re-exec остаётся defense-in-depth: при последующих exec из shared-кеша
// проверяется sidecar (см. [Host.Spawn] вызывает повторно — sidecar совпадёт).
// НО отсутствие sidecar больше НЕ значит «доверяй»: это значит «нужен
// Sigil-verify», который и выполняется здесь каждый раз.
//
// ns/name берутся из manifest плагина (d.Manifest.Namespace/.Name); ref — из
// rec (operator-asserted, с диска не сверяется, single-slot). anchors — снимок
// набора trust-anchor-ов (ADR-026(h)); пустой набор → no_trust_anchor.
func verifySigilAndSeal(dir, binaryPath, namespace, name string, anchors []ed25519.PublicKey, sigils SigilLookup) error {
	binDigestHex, err := computeFileDigest(binaryPath)
	if err != nil {
		return err
	}

	var rec *SigilRecord
	if sigils != nil {
		rec = sigils.Get(namespace, name)
	}
	if err := verifyRecordAgainstDigest(binDigestHex, namespace, name, rec, anchors); err != nil {
		return err
	}

	// Verify пройден → seal sidecar для re-exec defense-in-depth. Если sidecar
	// уже есть (повторный Spawn из кеша) — сверяем digest вместо записи.
	sidecarPath := filepath.Join(dir, DigestSidecarName)
	want, rerr := os.ReadFile(sidecarPath)
	switch {
	case rerr == nil:
		return verifyDigest(binaryPath, string(want))
	case errors.Is(rerr, os.ErrNotExist):
		return sealDigest(binaryPath, sidecarPath)
	default:
		return fmt.Errorf("pluginhost: read digest sidecar %q: %w", sidecarPath, rerr)
	}
}

// VerifyArtifactBytes — fail-closed Sigil-verify СКАЧАННЫХ байтов артефакта ДО
// материализации на диск (ADR-065(f), install-flow core.module.installed).
// Та же нормативная цепочка, что у [verifySigilAndSeal] (digest → подпись по
// блоку), но вход — байты в памяти; sidecar-seal не выполняется — файла ещё нет,
// его запечатает первый Spawn после установки.
func VerifyArtifactBytes(data []byte, rec *SigilRecord, anchors *AnchorSet) error {
	sum := sha256.Sum256(data)
	var namespace, name string
	if rec != nil {
		namespace, name = rec.Namespace, rec.Name
	}
	return verifyRecordAgainstDigest(hex.EncodeToString(sum[:]), namespace, name, rec, anchors.snapshot())
}

// verifyRecordAgainstDigest — общая середина verify (шаги 2–7 нормативного
// порядка [verifySigilAndSeal]): lookup-результат → anchors → digest-сверка →
// подпись блока. binDigestHex — фактический digest проверяемого артефакта
// (файл или байты в памяти).
func verifyRecordAgainstDigest(binDigestHex, namespace, name string, rec *SigilRecord, anchors []ed25519.PublicKey) error {
	if rec == nil {
		return verifyErrorFor(VerifyReasonNoSigil, namespace, name, "")
	}
	if len(anchors) == 0 {
		return verifyErrorFor(VerifyReasonNoTrustAnchor, namespace, name, rec.Ref)
	}

	binRaw, err := hex.DecodeString(rec.BinarySHA256hex)
	if err != nil {
		// Допущенный хеш не hex — допуск битый, fail-closed как mismatch (бинарь
		// заведомо не совпадёт с невалидным эталоном).
		return verifyErrorFor(VerifyReasonDigestMismatch, namespace, name, rec.Ref)
	}
	actualRaw, err := hex.DecodeString(binDigestHex)
	if err != nil {
		// computeFileDigest всегда отдаёт валидный hex — defensive.
		return fmt.Errorf("pluginhost: decode binary digest %q: %w", binDigestHex, err)
	}
	if subtle.ConstantTimeCompare(binRaw, actualRaw) != 1 {
		return verifyErrorFor(VerifyReasonDigestMismatch, namespace, name, rec.Ref)
	}

	manifestDigest := sha256.Sum256(NormalizeManifestBytes(rec.Manifest))
	block := BuildSigilBlock(rec.Namespace, rec.Name, rec.Ref, binRaw, manifestDigest[:])
	if !verifyAnyAnchor(anchors, block, rec.Signature) {
		return verifyErrorFor(VerifyReasonBadSignature, namespace, name, rec.Ref)
	}
	return nil
}

// verifyAnyAnchor — OR-проверка подписи против набора trust-anchor-ов
// (ADR-026(h), multi-anchor). Возвращает true, если подпись подтвердил хотя бы
// один якорь из набора; перебор останавливается на первом успехе.
//
// Перебор последовательный (не constant-time по числу якорей): набор якорей —
// publicly-known публичные ключи, не секрет, а само ed25519.Verify
// constant-time относительно подписи. Якорей единицы (primary + ротируемые),
// поэтому стоимость перебора пренебрежима в холодном Spawn-пути.
func verifyAnyAnchor(anchors []ed25519.PublicKey, block, signature []byte) bool {
	for _, pub := range anchors {
		if len(pub) == 0 {
			continue
		}
		if ed25519.Verify(pub, block, signature) {
			return true
		}
	}
	return false
}

// verifyDigest сверяет фактический digest бинаря с ожидаемым (из sidecar).
// Лишние пробелы/перевод строки в sidecar игнорируются.
func verifyDigest(binaryPath, wantRaw string) error {
	want := trimDigest(wantRaw)
	got, err := computeFileDigest(binaryPath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: %s (want %s, got %s)", ErrPluginDigestMismatch, binaryPath, want, got)
	}
	return nil
}

// sealDigest фиксирует digest бинаря в sidecar при первой загрузке.
// Запись атомарна через temp-файл + rename, чтобы конкурентные Spawn не
// увидели полупустой sidecar. Если sidecar успел появиться между проверкой и
// записью (гонка двух first-load Spawn) — переключаемся на verify.
func sealDigest(binaryPath, sidecarPath string) error {
	digest, err := computeFileDigest(binaryPath)
	if err != nil {
		return err
	}

	dir := filepath.Dir(sidecarPath)
	tmp, err := os.CreateTemp(dir, ".sha256-*.tmp")
	if err != nil {
		return fmt.Errorf("pluginhost: create temp digest sidecar in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.WriteString(digest); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pluginhost: write temp digest sidecar %q: %w", tmpPath, err)
	}
	if err := tmp.Chmod(digestSidecarMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pluginhost: chmod temp digest sidecar %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pluginhost: close temp digest sidecar %q: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, sidecarPath); err != nil {
		return fmt.Errorf("pluginhost: seal digest sidecar %q: %w", sidecarPath, err)
	}
	return nil
}

// trimDigest убирает окружающие пробелы/перевод строки из содержимого sidecar.
func trimDigest(s string) string {
	start, end := 0, len(s)
	for start < end && isSpaceByte(s[start]) {
		start++
	}
	for end > start && isSpaceByte(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
