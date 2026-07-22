package tmpl

import (
	"fmt"
	"strings"
	"text/template"

	goyaml "github.com/goccy/go-yaml"
)

// customFuncs — Soul Stack's own functions added to the FuncMap on top of the
// sprig allowlist. These are NOT sprig functions: `toYaml`/`fromYaml` are absent
// from upstream sprig (Helm-only), so they are implemented here via goccy/go-yaml.
// They are counted in the allowlist invariant separately from sprig
// ([templating.md §3.3]).
//
// [templating.md §3.3]: docs/templating.md
func customFuncs() template.FuncMap {
	return template.FuncMap{
		"toYaml":   toYaml,
		"fromYaml": fromYaml,
	}
}

// toYaml serializes a value to YAML. Unlike the Helm variant (which swallows the
// error and returns an empty string), here the error is propagated and fails the
// render normally — silently substituting garbage into a config is more dangerous
// than a failed step ([templating.md §10]).
//
// The trailing newline from the goccy encoder is trimmed: inside a template the
// result is usually embedded into a larger YAML, and an extra `\n` breaks
// indentation.
func toYaml(v any) (string, error) {
	out, err := goyaml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("toYaml: %w", err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// fromYaml parses a YAML string into an arbitrary structure (map/list/scalar),
// available further in the template via indexing. A parse error fails the render
// ([templating.md §10]).
func fromYaml(s string) (any, error) {
	var v any
	if err := goyaml.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("fromYaml: %w", err)
	}
	return v, nil
}
