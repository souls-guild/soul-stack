package jwt

import "strings"

// ParseBearerToken parses the `Authorization: Bearer <token>` header per
// RFC 6750 §2.1 / RFC 7230 §3.2.3:
//
//   - scheme comparison is case-insensitive (`Bearer` == `bearer` == `BEARER`);
//   - separator is OWS (SP / HTAB), not just SP;
//   - returns (token, true) on success; ("", false) for an empty header,
//     an unknown scheme, or a bare `Bearer` without a token part.
//
// Used by the Operator API HTTP middleware
// ([keeper/internal/api/middleware.RequireJWT]) and the MCP HTTP listener
// ([keeper/internal/mcp]) — both parse the same header in the same form,
// so no separate rules are needed.
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
