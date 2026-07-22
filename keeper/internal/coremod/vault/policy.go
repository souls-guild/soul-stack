package vault

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"

	"google.golang.org/protobuf/types/known/structpb"
)

// passwordPolicy describes rules for generating one secret (set by the service
// author in `core.vault.kv-present`): how many characters and from which
// alphabet. Unlike "entropy in bytes" (unpredictable final string length for
// the author), policy specifies the FINAL string length in characters and an
// explicit set of allowed characters — the author sees in YAML exactly what
// will end up in the secret.
//
// Form in params (see parsePolicy): the author writes either a named `charset`
// preset or explicit `allowed_chars`; `length` is the character count. Default
// alphabet is redis.conf-safe (no space/quotes/`#`/`\`/control chars) to prevent
// password from breaking redis.conf / users.acl on substitution.
type passwordPolicy struct {
	// length is the final password length IN CHARACTERS (not bytes of entropy).
	length int
	// alphabet is the set of allowed characters (rune slice for bias-free index
	// selection). Guaranteed non-empty after parsePolicy.
	alphabet []rune
}

// Policy defaults. defaultPasswordLength is 32 characters: with redis-safe
// alphabet (~90 characters) this is ~207 bits of entropy — with headroom for a
// password. defaultCharset is the redis.conf-safe preset (see charsetAlphabets).
const (
	defaultPasswordLength = 32
	defaultCharset        = charsetASCIIPrintableSafe

	// minPasswordLength / maxPasswordLength are bounds on `length` (protection
	// from 0/negative and from huge value filling KV/metrics). 8 is the minimum
	// reasonable threshold for generated password; 1024 characters is notionally
	// excessive ceiling.
	minPasswordLength = 8
	maxPasswordLength = 1024
)

// Names of charset presets (value of param `charset`).
const (
	charsetAlphanumeric       = "alphanumeric"
	charsetHex                = "hex"
	charsetBase64URL          = "base64url"
	charsetASCIIPrintableSafe = "ascii-printable-safe"
)

// charsetAlphabets is a map of alphabets for named presets.
//
//   - alphanumeric: Latin in both cases + digits (safe everywhere, but lower
//     entropy per character).
//   - hex: lowercase hex digits (for secrets consumed as hex string).
//   - base64url: url-safe base64 alphabet (`-`/`_` instead of `+`/`/`, no `=`).
//   - ascii-printable-safe: PRINTABLE ASCII MINUS characters that break redis.conf /
//     users.acl / shell substitution: space, `"`, `'`, `#`, `\`, plus backtick
//     and `$` (protection from accidental interpolation in configs). Step default
//     — password must not break the target config.
var charsetAlphabets = map[string]string{
	charsetAlphanumeric: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789",
	charsetHex:          "0123456789abcdef",
	charsetBase64URL:    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_",
	// ascii-printable-safe is built from printable range 0x21..0x7E minus
	// excludedFromSafe — keep exclusion list in one place (safeAlphabet).
	charsetASCIIPrintableSafe: safeAlphabet(),
}

// excludedFromSafe is the set of characters excluded from ascii-printable-safe
// preset: they break redis.conf/users.acl (space/quotes/#/\) or trigger
// interpolation (backtick and $).
const excludedFromSafe = " \"'#\\`$"

// safeAlphabet builds ascii-printable-safe alphabet: printable ASCII 0x21..0x7E
// (!..~) minus excludedFromSafe.
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

// parsePolicy parses policy from params object (step top-level or override within
// target). Nil object → policy from defaults (length=32, charset=ascii-printable-safe).
// base is the default, overridden by provided fields (for per-target override over
// step-level default).
//
// charset and allowed_chars are mutually exclusive: specifying both is an error
// (alphabet ambiguity). Empty allowed_chars / unknown charset is an error.
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
		// <2 characters — generation would devolve to constant (0 entropy).
		return passwordPolicy{}, fmt.Errorf("policy: alphabet must have >= 2 distinct characters, got %d", len(p.alphabet))
	}
	return p, nil
}

// defaultPolicy is the base policy from defaults (length=32, ascii-printable-safe).
func defaultPolicy() passwordPolicy {
	return passwordPolicy{
		length:   defaultPasswordLength,
		alphabet: []rune(charsetAlphabets[defaultCharset]),
	}
}

// dedupeRunes removes duplicate runes, preserving order of first occurrence. Needed for
// allowed_chars: a duplicate would skew uniformity (symbol appears more often), so
// alphabet is deduplicated before selection.
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

// generate returns a password of length p.length, each character uniformly randomly
// selected from p.alphabet. Source is crypto/rand (NOT math/rand). Bias-free
// selection: rand.Int(crypto/rand, n) gives uniform index on [0,n) without modulo skew
// (internally uses rejection sampling). p.alphabet is guaranteed non-empty
// (parsePolicy/defaultPolicy).
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
