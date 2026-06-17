package jwt

import "strings"

// ParseBearerToken парсит заголовок `Authorization: Bearer <token>` по
// RFC 6750 §2.1 / RFC 7230 §3.2.3:
//
//   - scheme сравнивается case-insensitive (`Bearer` == `bearer` == `BEARER`);
//   - разделитель — OWS (SP / HTAB), не только SP;
//   - возвращает (token, true) при успехе; (\"\", false) для пустого header-а,
//     неизвестного scheme или одиночного `Bearer` без token-part.
//
// Используется HTTP-middleware Operator API
// ([keeper/internal/api/middleware.RequireJWT]) и MCP HTTP-listener-ом
// ([keeper/internal/mcp]) — оба парсят один и тот же header в одинаковой
// форме, отдельных правил не нужно.
func ParseBearerToken(header string) (string, bool) {
	const scheme = "bearer"
	if header == "" {
		return "", false
	}
	idx := strings.IndexAny(header, " \t")
	if idx <= 0 {
		return "", false
	}
	if !strings.EqualFold(header[:idx], scheme) {
		return "", false
	}
	tok := strings.TrimSpace(header[idx+1:])
	if tok == "" {
		return "", false
	}
	return tok, true
}
