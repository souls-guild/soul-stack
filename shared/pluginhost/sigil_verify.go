package pluginhost

import (
	"errors"
	"fmt"
)

// Verify-замена TOFU на Sigil (ADR-026, slice S6b). До этого slice-а первая
// загрузка бинаря плагина доверялась «как есть» (TOFU) — это закрытый gap
// first-load: malicious-бинарь без подписи получал управление. Здесь TOFU-ветка
// заменена на fail-closed verify против печати доверия Sigil, доехавшей до Soul-а
// broadcast-ом от Keeper-а.
//
// Этот файл держит DI-контракт verify, по которому shared/pluginhost получает
// допуски и trust-anchor БЕЗ зависимости от keeper-proto: узкий [SigilRecord] +
// интерфейс [SigilLookup]. Soul-side адаптер (soul/) мапит keeperv1.PluginSigil
// → SigilRecord, не протаскивая proto/gen/go/keeper/v1 в shared.

// SigilRecord — verify-DTO одной печати доверия Sigil в форме, нужной
// shared/pluginhost для верификации. Узкая проекция keeperv1.PluginSigil:
// shared НЕ импортирует keeper-proto, Soul-side адаптер заполняет эту структуру.
//
// Поля симметричны подписываемому блоку [BuildSigilBlock]:
//   - Namespace / Name / Ref — идентичность допуска (Ref operator-asserted,
//     с диском не сверяется, входит в блок подписи);
//   - BinarySHA256hex — допущенный хеш бинаря (64 нижних hex-символа), сверяется
//     с фактическим digest-ом бинаря на диске;
//   - Signature — сырые байты ed25519-подписи блока (64 байта);
//   - Manifest — СЫРЫЕ байты manifest.yaml из транспорта (M1), которые verify
//     прогоняет через [NormalizeManifestBytes] перед хешем (S3↔S6-инвариант:
//     НЕ файл с диска, иначе хеш разъедется с подписью Keeper-а).
type SigilRecord struct {
	Namespace       string
	Name            string
	Ref             string
	BinarySHA256hex string
	Signature       []byte
	Manifest        []byte
}

// SigilLookup — поверхность чтения активного допуска по (namespace, name).
// Single-slot: на пару допущен ровно один активный Sigil (ADR-026(g)), поэтому
// ключ без ref. Реализуется Soul-side адаптером поверх runtime-кеша Sigil-ов
// (soul/internal/sigilcache); nil-результат = допуска нет → verify fail-closed
// (reason no_sigil).
type SigilLookup interface {
	Get(namespace, name string) *SigilRecord
}

// VerifyReason — машинно-различимая причина отказа Sigil-verify (ADR-026,
// event plugin.verify_failed). Каждое значение → fail-closed: плагин НЕ
// запускается (G-sigil-5, без allow-TOFU флага).
type VerifyReason string

const (
	// VerifyReasonNoSigil — допуск для (namespace, name) не доехал до Soul-а
	// (rec == nil). НЕ «ошибка → допустить»: не-допущенный плагин = «не
	// допущен», запускать нельзя.
	VerifyReasonNoSigil VerifyReason = "no_sigil"
	// VerifyReasonNoTrustAnchor — на Soul-е нет trust-anchor-а Sigil (pubkey
	// nil): Sigil не настроен на Keeper-е, проверять подпись нечем.
	VerifyReasonNoTrustAnchor VerifyReason = "no_trust_anchor"
	// VerifyReasonDigestMismatch — фактический digest бинаря на диске не
	// совпал с допущенным хешем (binary_sha256 в Sigil).
	VerifyReasonDigestMismatch VerifyReason = "digest_mismatch"
	// VerifyReasonBadSignature — подпись Sigil не прошла проверку trust-anchor-ом
	// (подменён manifest/бинарь/ref или ротация ключа без пересоздания допуска).
	VerifyReasonBadSignature VerifyReason = "bad_signature"
)

// ErrSigilVerify — sentinel-обёртка любого fail-closed-отказа Sigil-verify.
// Caller-ы отличают tamper/no-trust от прочих I/O-ошибок Spawn через
// errors.Is(err, ErrSigilVerify); конкретную причину достают через
// errors.As(err, &*VerifyError) и поле [VerifyError.Reason].
var ErrSigilVerify = errors.New("pluginhost: sigil verification failed")

// VerifyError — детализированный отказ Sigil-verify: причина + actionable-
// сообщение для оператора (event plugin.verify_failed, ADR-026). Оборачивает
// [ErrSigilVerify] для errors.Is.
type VerifyError struct {
	// Reason — машинно-различимая причина (для метрик/логов/тестов).
	Reason VerifyReason
	// Namespace / Name — адрес плагина, который не прошёл verify.
	Namespace string
	Name      string
	// Hint — человекочитаемая actionable-подсказка оператору.
	Hint string
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("%s: %s.%s [%s]: %s", ErrSigilVerify.Error(), e.Namespace, e.Name, e.Reason, e.Hint)
}

func (e *VerifyError) Unwrap() error { return ErrSigilVerify }

// verifyErrorFor собирает [VerifyError] с actionable-подсказкой под каждую
// причину. ns/name/ref печатаются в подсказке, чтобы оператор мог скопировать
// команду допуска без догадок.
func verifyErrorFor(reason VerifyReason, namespace, name, ref string) *VerifyError {
	var hint string
	switch reason {
	case VerifyReasonNoSigil:
		hint = fmt.Sprintf("плагин (%s, %s) не допущен; выполните `keeper.plugin.allow ns=%s name=%s ref=<ref>`",
			namespace, name, namespace, name)
	case VerifyReasonNoTrustAnchor:
		hint = "Sigil не настроен на Keeper (нет trust-anchor для verify подписи плагинов)"
	case VerifyReasonDigestMismatch:
		hint = "бинарь плагина не совпадает с допущенным хешем (подмена бинаря или устаревший допуск)"
	case VerifyReasonBadSignature:
		hint = fmt.Sprintf("подпись допуска недействительна (ротация ключа подписи? пересоздайте допуск: `keeper.plugin.allow ns=%s name=%s ref=%s`)",
			namespace, name, ref)
	default:
		hint = "Sigil-verify не пройден"
	}
	return &VerifyError{Reason: reason, Namespace: namespace, Name: name, Hint: hint}
}
