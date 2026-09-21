package audit

import (
	"log/slog"
	"regexp"
	"strconv"
	"strings"
)

// SealHooks holds process-global observability points for the regex-last-resort
// layer ([ADR-010] §7.4, layer 4). Decoupling: shared/audit does not pull in
// prometheus — keeper wires the keeper_mask_regex_fallback_total metric + a warn
// logger once at startup (cmd/keeper) via the setter below. Nil fields → no-op
// (tests/offline/Soul).
type SealHooks struct {
	// RegexFallback increments the regex-fallback metric (keeper_mask_regex_fallback_total).
	RegexFallback func(path string)
	// Logger is the warn-log channel for regex-fallback (cell path, no value).
	Logger *slog.Logger
}

// DefaultSealHooks are the global hooks called by [MaskSecretsWithSchema].
// Zero value (both nil) is a no-op. Set by [SetSealHooks] in cmd/keeper.
var DefaultSealHooks SealHooks

// SetSealHooks wires process-global observability for the regex-last-resort layer.
// Called once at keeper startup (after metrics registration). Idempotently
// overwrites. On the shared/audit side the prometheus dependency stays in keeper.
func SetSealHooks(h SealHooks) { DefaultSealHooks = h }

// reIdx matches an index path segment `[<digits>]` to generalize it to `[]` —
// the schema/seal set describes an array element without a concrete index
// (`acl[].password`), while the cell path carries a concrete one (`acl[0].password`).
var reIdx = regexp.MustCompile(`\[\d+\]`)

// normalizeIdx replaces every concrete path index with `[]`: `acl[0].users[2].pw`
// → `acl[].users[].pw`. This lets SecretPathSet/Sealed store the generalized
// idx form (one entry per array element) while the check matches any index.
func normalizeIdx(path string) string {
	if !strings.ContainsRune(path, '[') {
		return path
	}
	return reIdx.ReplaceAllString(path, "[]")
}

// joinPath/joinIdx build the dot/idx cell path, BIT-FOR-BIT like
// keeper/internal/render.joinKey/joinIdx (there the path is built during render;
// here it is checked during masking — the forms must match). joinPath on an empty
// path returns key (the root key without a leading dot).
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func joinIdx(path string, i int) string {
	return path + "[" + strconv.Itoa(i) + "]"
}
