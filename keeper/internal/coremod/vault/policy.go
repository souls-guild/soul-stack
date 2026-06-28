package vault

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"

	"google.golang.org/protobuf/types/known/structpb"
)

// passwordPolicy — описываемые автором сервиса правила генерации одного секрета
// (`core.vault.kv-present`): сколько символов и из какого алфавита. В отличие от
// «энтропии в байтах» (непредсказуемая для автора длина итоговой строки),
// policy задаёт ИТОГОВУЮ длину строки в символах и явный набор разрешённых
// символов — автор видит в YAML ровно то, что окажется в секрете.
//
// Форма в params (см. parsePolicy): автор пишет либо именованный `charset`-пресет,
// либо явный `allowed_chars`; `length` — число символов. Дефолт алфавита —
// redis.conf-safe (без пробела/кавычек/`#`/`\`/управляющих), чтобы пароль не
// ломал redis.conf / users.acl при подстановке.
type passwordPolicy struct {
	// length — итоговая длина пароля В СИМВОЛАХ (не в байтах энтропии).
	length int
	// alphabet — набор разрешённых символов (рунический срез для bias-free
	// выбора по индексу). Гарантированно непустой после parsePolicy.
	alphabet []rune
}

// Дефолты policy. defaultPasswordLength — 32 символа: при redis-safe-алфавите
// (~90 символов) это ~207 бит энтропии — с запасом для пароля. defaultCharset —
// redis.conf-safe-пресет (см. charsetAlphabets).
const (
	defaultPasswordLength = 32
	defaultCharset        = charsetASCIIPrintableSafe

	// minPasswordLength / maxPasswordLength — границы `length` (защита от
	// 0/отрицательного и от гигантского значения, забивающего KV/метрики).
	// 8 — нижний разумный порог для генерируемого пароля; 1024 символа —
	// заведомо избыточный потолок.
	minPasswordLength = 8
	maxPasswordLength = 1024
)

// Имена charset-пресетов (значение param `charset`).
const (
	charsetAlphanumeric       = "alphanumeric"
	charsetHex                = "hex"
	charsetBase64URL          = "base64url"
	charsetASCIIPrintableSafe = "ascii-printable-safe"
)

// charsetAlphabets — алфавиты именованных пресетов.
//
//   - alphanumeric: латиница в обоих регистрах + цифры (безопасно везде, но уже
//     по энтропии на символ).
//   - hex: строчные hex-цифры (для секретов, потребляемых как hex-строка).
//   - base64url: url-safe base64-алфавит (`-`/`_` вместо `+`/`/`, без `=`).
//   - ascii-printable-safe: ПЕЧАТНЫЙ ASCII МИНУС символы, ломающие redis.conf /
//     users.acl / shell-подстановку: пробел, `"`, `'`, `#`, `\`, а также
//     обратный апостроф `` ` `` и `$` (защита от случайной интерполяции в
//     конфигах). Дефолт шага — пароль не должен ломать целевой конфиг.
var charsetAlphabets = map[string]string{
	charsetAlphanumeric: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789",
	charsetHex:          "0123456789abcdef",
	charsetBase64URL:    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_",
	// ascii-printable-safe собирается из печатного диапазона 0x21..0x7E минус
	// excludedFromSafe — держим список исключений в одном месте (safeAlphabet).
	charsetASCIIPrintableSafe: safeAlphabet(),
}

// excludedFromSafe — символы, исключаемые из ascii-printable-safe-пресета:
// ломают redis.conf/users.acl (пробел/кавычки/`#`/`\`) или провоцируют
// интерполяцию (`` ` ``/`$`).
const excludedFromSafe = " \"'#\\`$"

// safeAlphabet строит ascii-printable-safe-алфавит: печатный ASCII 0x21..0x7E
// (`!`..`~`) за вычетом excludedFromSafe.
func safeAlphabet() string {
	excluded := make(map[byte]bool, len(excludedFromSafe))
	for i := 0; i < len(excludedFromSafe); i++ {
		excluded[excludedFromSafe[i]] = true
	}
	out := make([]byte, 0, 0x7E-0x21+1)
	for c := byte(0x21); c <= 0x7E; c++ {
		if !excluded[c] {
			out = append(out, c)
		}
	}
	return string(out)
}

// parsePolicy разбирает policy из объекта params (верхний уровень шага или
// override внутри target). nil-объект → policy из дефолтов (length=32,
// charset=ascii-printable-safe). base — дефолт, который переопределяется
// заданными полями (для per-target override поверх step-level default).
//
// `charset` и `allowed_chars` взаимоисключимы: задавать оба — ошибка
// (двусмысленность алфавита). Пустой `allowed_chars` / неизвестный `charset` —
// ошибка.
func parsePolicy(obj *structpb.Struct, base passwordPolicy) (passwordPolicy, error) {
	p := base

	n, ok, err := util.OptIntParam(obj, "length")
	if err != nil {
		return passwordPolicy{}, err
	}
	if ok {
		if n < minPasswordLength || n > maxPasswordLength {
			return passwordPolicy{}, fmt.Errorf("policy.length: want %d..%d characters, got %d", minPasswordLength, maxPasswordLength, n)
		}
		p.length = int(n)
	}

	charset, err := util.OptStringParam(obj, "charset")
	if err != nil {
		return passwordPolicy{}, err
	}
	allowed, err := util.OptStringParam(obj, "allowed_chars")
	if err != nil {
		return passwordPolicy{}, err
	}
	if charset != "" && allowed != "" {
		return passwordPolicy{}, fmt.Errorf("policy: charset and allowed_chars are mutually exclusive")
	}
	switch {
	case allowed != "":
		p.alphabet = dedupeRunes(allowed)
	case charset != "":
		alpha, known := charsetAlphabets[charset]
		if !known {
			return passwordPolicy{}, fmt.Errorf("policy.charset: unknown %q (want %s/%s/%s/%s)", charset,
				charsetAlphanumeric, charsetHex, charsetBase64URL, charsetASCIIPrintableSafe)
		}
		p.alphabet = []rune(alpha)
	}
	if len(p.alphabet) < 2 {
		// <2 символов: генерация выродилась бы в константу (0 энтропии).
		return passwordPolicy{}, fmt.Errorf("policy: alphabet must have >= 2 distinct characters, got %d", len(p.alphabet))
	}
	return p, nil
}

// defaultPolicy — базовый policy из дефолтов (length=32, ascii-printable-safe).
func defaultPolicy() passwordPolicy {
	return passwordPolicy{
		length:   defaultPasswordLength,
		alphabet: []rune(charsetAlphabets[defaultCharset]),
	}
}

// dedupeRunes удаляет повторы рун, сохраняя порядок первого вхождения. Нужна для
// `allowed_chars`: дубль в строке исказил бы равномерность (символ чаще
// выпадал бы), поэтому алфавит дедуплицируется до выбора.
func dedupeRunes(s string) []rune {
	seen := make(map[rune]bool, len(s))
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// generate возвращает пароль длины p.length, каждый символ — равновероятно
// выбранная руна из p.alphabet. Источник — crypto/rand (НЕ math/rand). Выбор
// bias-free: rand.Int(crypto/rand, n) даёт равномерный индекс на [0,n) без
// modulo-перекоса (внутри — rejection sampling). p.alphabet гарантированно
// непуст (parsePolicy/defaultPolicy).
func (p passwordPolicy) generate() (string, error) {
	n := big.NewInt(int64(len(p.alphabet)))
	out := make([]rune, p.length)
	for i := range out {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		out[i] = p.alphabet[idx.Int64()]
	}
	return string(out), nil
}
