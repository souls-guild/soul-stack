package voyage

import (
	"errors"
	"strconv"
	"strings"
)

// BatchSpecMode — дискриминатор разобранного строкового batch-поля: размер пачки
// задан абсолютным числом хостов/инкарнаций либо процентом от scope.
type BatchSpecMode int

const (
	// BatchSpecHosts — `batch: "N"`: абсолютный размер Leg-а (parity batch_size).
	BatchSpecHosts BatchSpecMode = iota
	// BatchSpecPercent — `batch: "N%"`: процент от резолвнутого scope (parity
	// batch_percent), эффективный размер считается ПОСЛЕ резолва scope через
	// существующий effectiveBatchSize.
	BatchSpecPercent
)

// Sentinel-ошибки [ParseBatchSpec] — caller маппит их в человекочитаемый 422-detail
// (или трактует [ErrBatchSpecEmpty] как «поле не задано»).
//   - ErrBatchSpecEmpty        — пустая/пробельная строка; не «ошибка ввода», а
//     отсутствие значения (caller: весь scope одним Leg).
//   - ErrBatchSpecMalformed    — не соответствует `^\d+%?$` (точка, знак, мусор,
//     внутренний пробел) либо число не влезает в int (overflow).
//   - ErrBatchSpecPercentRange — percent вне [1, 100].
//   - ErrBatchSpecHostsRange   — hosts < 1.
var (
	ErrBatchSpecEmpty        = errors.New("voyage: batch spec is empty")
	ErrBatchSpecMalformed    = errors.New("voyage: batch spec malformed (expected N or N%)")
	ErrBatchSpecPercentRange = errors.New("voyage: batch percent out of range [1, 100]")
	ErrBatchSpecHostsRange   = errors.New("voyage: batch hosts must be >= 1")
)

// batchSpecMaxDigits — потолок длины числовой части. int64 в худшем случае —
// 19 значащих цифр; 9 безопасно покрывает любой осмысленный размер батча
// (до 999 999 999) и заведомо влезает в int на всех целевых платформах,
// исключая overflow ещё до strconv.Atoi.
const batchSpecMaxDigits = 9

// ParseBatchSpec разбирает строковое batch-поле Voyage (S1 строковых batch-полей).
//
// Грамматика (fail-closed): trim входной строки → строго `^(\d+)(%?)$`. Суффикс
// `%` ⇒ [BatchSpecPercent], value∈[1,100]; иначе [BatchSpecHosts], value≥1.
// Любое отклонение (знак, точка, внутренний пробел, лишние символы, overflow) →
// [ErrBatchSpecMalformed]. Пустая/пробельная строка → [ErrBatchSpecEmpty]
// (caller трактует как «не задано», НЕ как ошибку ввода).
//
// Pure-функция: без аллокаций сверх trim, без regexp (ручной скан дешевле и даёт
// явный overflow-guard по длине цифровой части).
func ParseBatchSpec(s string) (mode BatchSpecMode, value int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, ErrBatchSpecEmpty
	}

	digits := s
	percent := false
	if last := len(s) - 1; s[last] == '%' {
		percent = true
		digits = s[:last]
	}
	// Пустая цифровая часть ("%"), пустая после trim уже отсечена выше.
	if digits == "" {
		return 0, 0, ErrBatchSpecMalformed
	}
	// Только ASCII-цифры: исключает знак, точку, внутренний пробел, второй `%`.
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, 0, ErrBatchSpecMalformed
		}
	}
	// Overflow-guard по длине ДО strconv: огромные цифры → malformed.
	if len(digits) > batchSpecMaxDigits {
		return 0, 0, ErrBatchSpecMalformed
	}

	n, convErr := strconv.Atoi(digits)
	if convErr != nil {
		return 0, 0, ErrBatchSpecMalformed
	}

	if percent {
		if n < 1 || n > 100 {
			return 0, 0, ErrBatchSpecPercentRange
		}
		return BatchSpecPercent, n, nil
	}
	if n < 1 {
		return 0, 0, ErrBatchSpecHostsRange
	}
	return BatchSpecHosts, n, nil
}
