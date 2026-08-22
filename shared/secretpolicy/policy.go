// Package secretpolicy describes the rules for generating one secret: how many
// characters and from which alphabet.
//
// Unlike "entropy in bytes" (an unpredictable final string length for the author),
// a policy specifies the FINAL string length in characters and an explicit set of
// allowed characters — the author sees in YAML exactly what will end up in the secret.
//
// Two callers parse the same grammar and must not drift apart: the keeper-side module
// `core.vault.kv-present` (params object) and the CEL function `generate_secret()`
// (map argument, ADR-0083 §9). Both land here.
package secretpolicy

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// Policy is a parsed generation policy. Construct it with [Default] or [Parse] —
// a zero Policy has an empty alphabet and generates nothing.
type Policy struct {
	// Length is the final password length IN CHARACTERS (not bytes of entropy).
	Length int
	// Alphabet is the set of allowed characters (a rune slice for bias-free index
	// selection). Guaranteed non-empty after [Parse] and [Default].
	Alphabet []rune
}

// Policy defaults. DefaultLength is 32 characters: with the redis-safe alphabet
// (~90 characters) that is ~207 bits of entropy — with headroom for a password.
// DefaultCharset is the redis.conf-safe preset (see [Alphabets]).
const (
	DefaultLength  = 32
	DefaultCharset = CharsetASCIIPrintableSafe

	// MinLength / MaxLength bound `length` (protection from 0/negative and from a
	// huge value filling KV/metrics). 8 is the minimum reasonable threshold for a
	// generated password; 1024 characters is a notionally excessive ceiling.
	MinLength = 8
	MaxLength = 1024
)

// Names of the charset presets (the value of the `charset` key).
const (
	CharsetAlphanumeric       = "alphanumeric"
	CharsetHex                = "hex"
	CharsetBase64URL          = "base64url"
	CharsetASCIIPrintableSafe = "ascii-printable-safe"
)

// Alphabets maps a preset name to its alphabet.
//
//   - alphanumeric: Latin in both cases + digits (safe everywhere, but lower
//     entropy per character).
//   - hex: lowercase hex digits (for secrets consumed as a hex string).
//   - base64url: the url-safe base64 alphabet (`-`/`_` instead of `+`/`/`, no `=`).
//   - ascii-printable-safe: PRINTABLE ASCII MINUS the characters that break
//     redis.conf / users.acl / shell substitution: space, `"`, `'`, `#`, `\`, plus
//     backtick and `$` (protection from accidental interpolation in configs). The
//     default — a password must not break the target config.
var Alphabets = map[string]string{
	CharsetAlphanumeric: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789",
	CharsetHex:          "0123456789abcdef",
	CharsetBase64URL:    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_",
	// ascii-printable-safe is built from the printable range 0x21..0x7E minus
	// excludedFromSafe — keep the exclusion list in one place (safeAlphabet).
	CharsetASCIIPrintableSafe: safeAlphabet(),
}

// excludedFromSafe is the set of characters excluded from the ascii-printable-safe
// preset: they break redis.conf/users.acl (space/quotes/#/\) or trigger
// interpolation (backtick and $).
const excludedFromSafe = " \"'#\\`$"

// safeAlphabet builds the ascii-printable-safe alphabet: printable ASCII 0x21..0x7E
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

// Default is the base policy (length=32, ascii-printable-safe).
func Default() Policy {
	return Policy{
		Length:   DefaultLength,
		Alphabet: []rune(Alphabets[DefaultCharset]),
	}
}

// Parse reads a policy from a generic map (a `core.vault.kv-present` params object
// converted with Struct.AsMap, or the map argument of `generate_secret()`). A nil or
// empty map yields base unchanged, so a caller can layer a per-target override over a
// step-level default.
//
// charset and allowed_chars are mutually exclusive: giving both is an error (alphabet
// ambiguity). An empty allowed_chars or an unknown charset is an error.
func Parse(m map[string]any, base Policy) (Policy, error) {
	p := base

	if err := rejectUnknownKeys(m); err != nil {
		return Policy{}, err
	}

	n, ok, err := optInt(m, "length")
	if err != nil {
		return Policy{}, err
	}
	if ok {
		if n < MinLength || n > MaxLength {
			return Policy{}, fmt.Errorf("policy.length: want %d..%d characters, got %d", MinLength, MaxLength, n)
		}
		p.Length = int(n)
	}

	charset, hasCharset, err := optString(m, "charset")
	if err != nil {
		return Policy{}, err
	}
	allowed, hasAllowed, err := optString(m, "allowed_chars")
	if err != nil {
		return Policy{}, err
	}
	// Presence, not non-emptiness: `charset: "hex"` next to `allowed_chars: ""` is the
	// same ambiguity as two non-empty values, and reading it as "hex wins" picks one of
	// the two things the author wrote and drops the other silently.
	if hasCharset && hasAllowed {
		return Policy{}, fmt.Errorf("policy: charset and allowed_chars are mutually exclusive")
	}
	switch {
	case hasAllowed:
		p.Alphabet = dedupeRunes(allowed)
	case hasCharset:
		alpha, known := Alphabets[charset]
		if !known {
			return Policy{}, fmt.Errorf("policy.charset: unknown %q (want %s/%s/%s/%s)", charset,
				CharsetAlphanumeric, CharsetHex, CharsetBase64URL, CharsetASCIIPrintableSafe)
		}
		p.Alphabet = []rune(alpha)
	}
	if len(p.Alphabet) < 2 {
		// <2 characters — generation would devolve to a constant (0 entropy).
		return Policy{}, fmt.Errorf("policy: alphabet must have >= 2 distinct characters, got %d", len(p.Alphabet))
	}
	return p, nil
}

// knownKeys — the whole policy grammar. A key outside it is an author error, not an
// extension point: silently ignoring `charsett` would hand out secrets from the
// DEFAULT alphabet while the YAML says otherwise, and nothing would report it.
var knownKeys = map[string]bool{
	"length":        true,
	"charset":       true,
	"allowed_chars": true,
}

// rejectUnknownKeys fails on any key outside [knownKeys]. All offenders are reported
// at once and in sorted order — map iteration is unordered, and an error text that
// varies run to run on the same input is not diagnosable.
func rejectUnknownKeys(m map[string]any) error {
	var unknown []string
	for k := range m {
		if !knownKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("policy: unknown key(s) %s (want length/charset/allowed_chars)", strings.Join(unknown, ", "))
}

// optString reads an optional string key, reporting whether it was PRESENT. A missing
// key and an explicit null are both "absent" (structpb null decodes to a nil any) —
// parity with util.OptStringParam.
//
// Present-but-empty is a third answer and the caller needs it: collapsing `charset: ""`
// into "absent" hands back the default alphabet for a value the author plainly meant
// something by, and `allowed_chars: ""` — a deliberate "nothing is allowed" — would come
// back as a generated secret over the default alphabet instead of a refusal.
func optString(m map[string]any, key string) (string, bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("param %q: expected string, got %T", key, v)
	}
	return s, true, nil
}

// optInt reads an optional integer key. JSON/structpb carry every number as a float64,
// CEL carries it as an int64, and a hand-built map may hold a plain int — all three are
// accepted, a fractional value is not (parity with util.OptIntParam).
func optInt(m map[string]any, key string) (int64, bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch n := v.(type) {
	case int64:
		return n, true, nil
	case int:
		return int64(n), true, nil
	case float64:
		if n != float64(int64(n)) {
			return 0, false, fmt.Errorf("param %q: expected integer, got %v", key, n)
		}
		return int64(n), true, nil
	default:
		return 0, false, fmt.Errorf("param %q: expected integer, got %T", key, v)
	}
}

// dedupeRunes removes duplicate runes, preserving the order of first occurrence. Needed
// for allowed_chars: a duplicate would skew uniformity (a symbol appears more often), so
// the alphabet is deduplicated before selection.
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

// Generate returns a password of length p.Length, each character selected uniformly at
// random from p.Alphabet. The source is crypto/rand (NOT math/rand). Selection is
// bias-free: rand.Int gives a uniform index on [0,n) without modulo skew (it uses
// rejection sampling internally). p.Alphabet is guaranteed non-empty by [Parse]/[Default].
func (p Policy) Generate() (string, error) {
	if len(p.Alphabet) < 2 {
		return "", fmt.Errorf("policy: alphabet must have >= 2 distinct characters, got %d", len(p.Alphabet))
	}
	n := big.NewInt(int64(len(p.Alphabet)))
	out := make([]rune, p.Length)
	for i := range out {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		out[i] = p.Alphabet[idx.Int64()]
	}
	return string(out), nil
}
