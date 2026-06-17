// Verifier — парсинг и валидация HS256-JWT, выпущенных [Issuer].
//
// MVP (ADR-014): HS256, signing key из Vault KV `secret/keeper/jwt-signing-key`,
// pin `iss` и проверка `exp`. Используется HTTP-middleware Operator API
// ([keeper/internal/api/middleware/auth.go]).
//
// Verifier не отличает «expired» от «not yet valid» отдельным sentinel —
// expired-частота высока (короткие TTL), остальное (`nbf`) в issuer-е не
// проставляется. Все остальные ошибки парсинга/подписи/issuer-а
// сводятся к [ErrInvalidToken] и [ErrInvalidIssuer] для предсказуемой
// классификации 401-handler-ом.
package jwt

import (
	"errors"
	"fmt"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// Claims — экстракт из распарсенного JWT, безопасный для проброса в
// HTTP-context. Не содержит исходных полей jwtv5 (RegisteredClaims),
// чтобы middleware-потребители не зависели от внутреннего представления
// и не могли случайно прочитать невалидированные поля.
type Claims struct {
	Subject          string // sub → AID
	Issuer           string // iss
	Roles            []string
	BootstrapInitial bool
	IssuedAt         time.Time
	ExpiresAt        time.Time
}

// ErrInvalidToken — общая ошибка парсинга/подписи/структуры токена.
// Включает: malformed, plus-bad-segments, неверная подпись, отсутствие
// обязательных claims (sub/iss/exp/iat), wrong-alg.
var ErrInvalidToken = errors.New("jwt: invalid token")

// ErrExpiredToken — `exp` в прошлом. Выделено в отдельный sentinel,
// чтобы middleware мог дать более информативный `detail` в 401-ответе.
var ErrExpiredToken = errors.New("jwt: token expired")

// ErrInvalidIssuer — `iss` claim не совпадает с issuer-ом, на который
// настроен Verifier.
var ErrInvalidIssuer = errors.New("jwt: invalid issuer")

// publicDetail* — фиксированные строки, которые [ClassifyVerifyErr]
// возвращает в HTTP response. Никаких форматов с err.Error(): внутреннее
// сообщение golang-jwt/v5 (например, путь парсера) — лишняя поверхность
// атаки на oracle-attacks через различение причин 401.
const (
	publicDetailInvalidToken  = "invalid token"
	publicDetailExpiredToken  = "token expired"
	publicDetailInvalidIssuer = "token issuer not trusted"
)

// ClassifyVerifyErr возвращает public-safe detail-строку для HTTP 401-
// ответа по ошибке [Verifier.Verify]. Гарантия — НИКОГДА не пробрасывает
// raw err.Error() (внутреннее сообщение golang-jwt/v5).
//
// Контракт хрупкий, поэтому классификатор живёт здесь, а не в каждом
// middleware: при добавлении нового sentinel-а в этом пакете нужно
// расширить switch одновременно с экспортом константы — будет видно в
// одном code-review-е.
//
// nil err → пустая строка (caller обязан проверять err != nil сам).
func ClassifyVerifyErr(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrExpiredToken):
		return publicDetailExpiredToken
	case errors.Is(err, ErrInvalidIssuer):
		return publicDetailInvalidIssuer
	default:
		// ErrInvalidToken + любая необёрнутая ошибка → один generic-detail.
		return publicDetailInvalidToken
	}
}

// Verifier — конфигурация одного pin-issuer + signing-key. Безопасен
// для конкурентного использования (immutable после конструктора).
type Verifier struct {
	signingKey []byte
	issuer     string
}

// NewVerifier создаёт verifier. signingKey должен быть >= 32 байт
// (HS256-требование RFC 7518 §3.2), issuer — непустая строка для pin-а.
func NewVerifier(signingKey []byte, issuer string) (*Verifier, error) {
	if len(signingKey) < minSigningKeyBytes {
		return nil, fmt.Errorf("jwt: signing key length %d < %d (HS256 minimum)", len(signingKey), minSigningKeyBytes)
	}
	if issuer == "" {
		return nil, errors.New("jwt: issuer is empty")
	}
	return &Verifier{signingKey: signingKey, issuer: issuer}, nil
}

// Verify парсит и валидирует tokenString. Возвращает извлечённые
// claims или одну из трёх sentinel-ошибок ([ErrInvalidToken],
// [ErrExpiredToken], [ErrInvalidIssuer]) для предсказуемого маппинга
// на HTTP 401.
//
// Проверки:
//
//   - HMAC-метод (отказ от `alg: none` и любого asym-алгоритма);
//   - подпись HS256 по `signingKey`;
//   - `iss == verifier.issuer`;
//   - `exp` строго в будущем (jwtv5.WithExpirationRequired);
//   - `sub` непустой (иначе токен бесполезен — некому приписать действия).
func (v *Verifier) Verify(tokenString string) (*Claims, error) {
	if tokenString == "" {
		return nil, ErrInvalidToken
	}

	parsed, err := jwtv5.ParseWithClaims(tokenString, &archonClaims{},
		func(t *jwtv5.Token) (interface{}, error) {
			// Отказ принимать любые non-HMAC методы — защита от
			// `alg: none` и от подмены на asym-key, который злоумышленник
			// мог бы подсунуть как HMAC-secret.
			if _, ok := t.Method.(*jwtv5.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return v.signingKey, nil
		},
		jwtv5.WithValidMethods([]string{"HS256"}),
		jwtv5.WithExpirationRequired(),
		jwtv5.WithIssuedAt(),
	)
	if err != nil {
		switch {
		case errors.Is(err, jwtv5.ErrTokenExpired):
			return nil, ErrExpiredToken
		default:
			return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err.Error())
		}
	}
	if !parsed.Valid {
		return nil, ErrInvalidToken
	}

	claims, ok := parsed.Claims.(*archonClaims)
	if !ok {
		return nil, ErrInvalidToken
	}

	if claims.Issuer != v.issuer {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrInvalidIssuer, claims.Issuer, v.issuer)
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: empty sub", ErrInvalidToken)
	}
	if claims.IssuedAt == nil {
		return nil, fmt.Errorf("%w: missing iat", ErrInvalidToken)
	}
	if claims.ExpiresAt == nil {
		return nil, fmt.Errorf("%w: missing exp", ErrInvalidToken)
	}

	return &Claims{
		Subject:          claims.Subject,
		Issuer:           claims.Issuer,
		Roles:            claims.Roles,
		BootstrapInitial: claims.BootstrapInitial,
		IssuedAt:         claims.IssuedAt.Time,
		ExpiresAt:        claims.ExpiresAt.Time,
	}, nil
}
