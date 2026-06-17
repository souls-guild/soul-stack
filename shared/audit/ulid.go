package audit

import (
	"crypto/rand"
	"regexp"

	"github.com/oklog/ulid/v2"
)

// NewULID возвращает свежий ULID (26 символов, lexically sortable
// timestamp prefix + random). Используется для `audit_id`, а также
// переиспользуется shared/config/store.go::newCorrelationID().
//
// `ulid.Make()` использует `entropy.New(rand.Reader, ...)` внутри;
// reader-фейл — машина в аварийном состоянии. Соблюдаем тот же контракт,
// что и предыдущий `newCorrelationID()` под `crypto/rand`: panic на
// I/O-fail, чтобы сохранить инвариант формата строки вместо тихого
// разрушения.
func NewULID() string {
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		panic("audit: ulid generation failed: " + err.Error())
	}
	return id.String()
}

// ulidPattern — Crockford base32 (без I, L, O, U). 26 символов. Тот же
// regex, что в shared/audit/ulid_test.go::ulidPattern и в M0.3
// regression-тестах CorrelationID.
var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// IsValidULID — синтаксическая проверка ULID-строки по Crockford base32
// (длина 26, символы из `0-9A-HJKMNP-TV-Z`). Используется validation-ом
// query-параметров на API-границе (например, `apply_id`-фильтр в
// `/v1/incarnations/{name}/history`), чтобы отсечь мусор до round-trip-а
// в Postgres.
func IsValidULID(s string) bool {
	return ulidPattern.MatchString(s)
}
