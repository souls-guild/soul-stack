package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// Canonical JSON, and why it has to be canonical.
//
// The document is hashed and signed (ADR-026 Sigil): the bytes Keeper hashes at sign
// time must be the bytes Soul re-hashes at verify time, and `soul-mod verify` compares
// a stamped payload against a freshly generated one byte for byte. So serialization
// must be a pure function of the value — no map iteration order, no field order drift,
// no emitter settings leaking in.
//
// The rules are: object keys sorted ascending by their UTF-8 bytes, no insignificant
// whitespace, no HTML escaping, minimal string escaping, number literals as produced
// by encoding/json. Everything goes through [Marshal] — the artifact's `schema`
// subcommand and the `soul-mod` tool call the same function, there is no second
// encoder to keep in step.

// Marshal renders a document as canonical JSON.
//
// It marshals through encoding/json first (so struct tags, omitempty and omitzero
// apply exactly once), then re-emits the resulting tree with sorted keys. Re-emitting
// rather than relying on struct field order is deliberate: field order is a thing a
// later edit changes without noticing, and the guarantee here has to survive that.
//
// Marshal is idempotent — parsing its output with [Unmarshal] and marshalling again
// yields the same bytes.
func Marshal(doc Document) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("schema: marshal document: %w", err)
	}
	// UseNumber keeps number literals exactly as encoding/json wrote them, so the
	// re-emit step cannot introduce a float round-trip of its own.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("schema: re-read document: %w", err)
	}
	out, err := appendCanonical(make([]byte, 0, len(raw)+64), tree)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Unmarshal parses a canonical document. Decoding is strict: an unknown key is an
// error, not a passthrough, which is what makes adding a field to [Param] a
// forward-compat event rather than a silent one.
//
// Unmarshal does NOT validate — call [Validate] on the result.
func Unmarshal(data []byte) (Document, error) {
	var doc Document
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("schema: parse document: %w", err)
	}
	// Exactly one JSON value per document: trailing content means the payload was
	// concatenated with something, which a reader must not accept quietly.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		return Document{}, fmt.Errorf("schema: parse document: unexpected trailing content")
	}
	return doc, nil
}

// IsCanonical reports whether data is exactly what [Marshal] produces for the document
// it encodes. Used by `soul-mod stamp`, which stores the artifact's own bytes verbatim
// and must refuse to stamp anything it could not have produced itself.
func IsCanonical(data []byte) (bool, error) {
	doc, err := Unmarshal(data)
	if err != nil {
		return false, err
	}
	canon, err := Marshal(doc)
	if err != nil {
		return false, err
	}
	return bytes.Equal(canon, data), nil
}

func appendCanonical(dst []byte, v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		if t {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case json.Number:
		// The literal came from encoding/json, so it is well-formed; the check
		// guards against a hand-built tree reaching this path.
		if _, iErr := t.Int64(); iErr != nil {
			if _, fErr := t.Float64(); fErr != nil {
				return nil, fmt.Errorf("schema: invalid number %q: %w", t.String(), fErr)
			}
		}
		return append(dst, t.String()...), nil
	case string:
		return appendString(dst, t), nil
	case []any:
		dst = append(dst, '[')
		for i, e := range t {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			if dst, err = appendCanonical(dst, e); err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendString(dst, k)
			dst = append(dst, ':')
			var err error
			if dst, err = appendCanonical(dst, t[k]); err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	default:
		return nil, fmt.Errorf("schema: unexpected value of type %T in document tree", v)
	}
}

// appendString writes a JSON string with the minimal escape set: quote, backslash and
// the C0 controls. Everything else — including `<`, `>`, `&` and any non-ASCII rune —
// is written as-is, so the output is plain UTF-8 rather than encoding/json's
// HTML-escaped form.
func appendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			dst = append(dst, '\\', '"')
		case c == '\\':
			dst = append(dst, '\\', '\\')
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c == '\b':
			dst = append(dst, '\\', 'b')
		case c == '\f':
			dst = append(dst, '\\', 'f')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0')
			const hex = "0123456789abcdef"
			dst = append(dst, hex[c>>4], hex[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"')
}

// version is a parsed plain MAJOR.MINOR.PATCH. The SDK carries no semver dependency
// on purpose: plugin authors link this module, and three integers do not justify one.
type version struct{ major, minor, patch int }

// parseVersion accepts exactly the grammar a compat bound uses (ADR-0076(c)): plain
// MAJOR.MINOR.PATCH, no `v` prefix, no pre-release suffix, no leading zeros.
func parseVersion(s string) (version, bool) {
	var v version
	parts := [3]*int{&v.major, &v.minor, &v.patch}
	start, seen := 0, 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '.' {
			continue
		}
		if seen == 3 {
			return version{}, false
		}
		seg := s[start:i]
		if seg == "" || (len(seg) > 1 && seg[0] == '0') {
			return version{}, false
		}
		for j := 0; j < len(seg); j++ {
			if seg[j] < '0' || seg[j] > '9' {
				return version{}, false
			}
		}
		n, err := strconv.Atoi(seg)
		if err != nil {
			return version{}, false
		}
		*parts[seen] = n
		seen++
		start = i + 1
	}
	if seen != 3 {
		return version{}, false
	}
	return v, true
}
