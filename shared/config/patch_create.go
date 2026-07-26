package config

// Create-on-write for a scalar at an absent yaml path — the prerequisite of the
// SettingsStore overlay ([ADR-0073](docs/adr/0073-keeper-runtime-config-pg.md)):
// optional blocks are legal and common (an absent `toll:` means "enabled with
// defaults"), so an override must be able to reach a key inside a block the file
// never mentions.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// PatchKeeperOrCreate is [PatchKeeper] that CREATES the path instead of
// returning [ErrPathNotFound]. Missing intermediate mappings are created too
// (`$.tempo.voyage_create.rate` on a file with no `tempo:` block).
//
// Deliberately a separate entry point rather than a change to [PatchKeeper]:
// for the write-back consumer a nonexistent path is a typo worth a 404, while
// for the overlay it is the normal case.
//
// Extra restrictions on top of the [PatchKeeper] contract:
//
//   - the path must be a plain chain of map keys (`$.a.b.c`); indices,
//     wildcards and quoted keys are rejected — create-on-write for sequences has
//     no consumer and would need a merge policy of its own;
//   - an existing NON-mapping node on the way (`toll: 5` for
//     `$.toll.threshold`) is an error, never a silent replacement of the scalar
//     by a mapping.
func PatchKeeperOrCreate(doc *Document, yamlPath string, value any) error {
	err := patchOne(doc, yamlPath, value)
	if !errors.Is(err, ErrPathNotFound) && !errors.Is(err, errPathUnresolved) {
		return err
	}
	return createOne(doc, yamlPath, value)
}

// createOne inserts `value` at an absent `yamlPath`. Strategy: find the deepest
// EXISTING mapping on the path and merge into it a fragment holding only the
// missing suffix. Walking bottom-up matters — the first existing node is the
// real conflict point, so a scalar in the middle is reported instead of being
// overwritten.
func createOne(doc *Document, yamlPath string, value any) error {
	if doc == nil {
		return errors.New("config: Document is nil")
	}
	segs, err := createPathSegments(yamlPath)
	if err != nil {
		return err
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	if doc.file == nil {
		return errors.New("config: Document has no AST file (parse failed; cannot patch)")
	}

	anchor, rest, err := deepestMapping(doc.file, segs)
	if err != nil {
		return err
	}

	// Fragment relative to the anchor: one key per level, so marshaling a
	// map is order-stable.
	var nested any = value
	for i := len(rest) - 1; i >= 0; i-- {
		nested = map[string]any{rest[i]: nested}
	}
	raw, err := yaml.Marshal(nested)
	if err != nil {
		return fmt.Errorf("config: cannot marshal value for %q: %w", yamlPath, err)
	}
	frag, err := parser.ParseBytes(raw, 0)
	if err != nil || len(frag.Docs) == 0 || frag.Docs[0].Body == nil {
		return fmt.Errorf("config: cannot build fragment for %q: %w", yamlPath, err)
	}

	// ast.Merge appends missing keys and replaces the value of an existing one
	// — which is what turns a `toll:` with an empty (null) value into a block.
	if err := ast.Merge(anchor, frag.Docs[0].Body); err != nil {
		return fmt.Errorf("config: cannot create %q: %w", yamlPath, err)
	}

	doc.mutated = true
	return nil
}

// deepestMapping returns the deepest existing mapping node on the path and the
// remaining (missing) key suffix to be merged into it. `$` — the document root
// — is the last resort.
func deepestMapping(file *ast.File, segs []string) (*ast.MappingNode, []string, error) {
	for i := len(segs) - 1; i >= 0; i-- {
		prefix := "$." + strings.Join(segs[:i], ".")
		if i == 0 {
			prefix = "$"
		}
		p, err := yaml.PathString(prefix)
		if err != nil {
			return nil, nil, fmt.Errorf("config: invalid yaml path %q: %w", prefix, err)
		}
		node, err := p.FilterFile(file)
		if err != nil {
			if yaml.IsNotFoundNodeError(err) {
				continue
			}
			return nil, nil, fmt.Errorf("config: cannot resolve path %q: %w", prefix, err)
		}
		switch n := node.(type) {
		case *ast.MappingNode:
			return n, segs[i:], nil
		case *ast.NullNode:
			// `toll:` with no value — not a mapping we can merge into, but the
			// key does exist, so its parent replaces the null wholesale.
			continue
		default:
			return nil, nil, fmt.Errorf("config: cannot create %q: %q already holds a non-mapping value (kind=%s)",
				"$."+strings.Join(segs, "."), prefix, node.Type().String())
		}
	}
	return nil, nil, errors.New("config: document root is not a mapping; cannot create a path in it")
}

// createPathSegments splits `$.a.b.c` into map keys, rejecting everything
// create-on-write does not support.
func createPathSegments(yamlPath string) ([]string, error) {
	if strings.TrimSpace(yamlPath) == "" {
		return nil, errors.New("config: yaml path is empty")
	}
	if !strings.HasPrefix(yamlPath, "$.") {
		return nil, fmt.Errorf("config: yaml path must start with '$.': got %q", yamlPath)
	}
	segs := strings.Split(strings.TrimPrefix(yamlPath, "$."), ".")
	for _, s := range segs {
		if s == "" {
			return nil, fmt.Errorf("config: yaml path %q has an empty segment", yamlPath)
		}
		if strings.ContainsAny(s, "[]*'\"") {
			return nil, fmt.Errorf("config: yaml path %q is not creatable: only plain map keys are supported", yamlPath)
		}
	}
	return segs, nil
}
