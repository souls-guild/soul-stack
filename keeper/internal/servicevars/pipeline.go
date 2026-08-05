package servicevars

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/goccy/go-yaml"
)

// Resolve assembles the effective vars map of a service: every `*.yaml`/`*.yml`
// file directly inside `<ServiceDir>/vars/`, deep-merged in lexical order
// (ADR-0082 §3). A later file wins.
//
// The base layer is `00-base.yaml` by convention, and the convention is the sort
// order rather than the name: numbering makes the sequence legible without
// knowing where a leading character falls in ASCII. This is exactly why the base
// file is NOT `_default.yaml` — `_` is 0x5F, between 'Z' and 'a', so
// `_default.yaml` sorts ahead of its siblings only while every one of them is
// lower-case.
//
// Not layers, and deliberately so:
//
//   - `_stack.yaml` — the declarative pipeline (NIM-413). It declares the order;
//     including it in that order would be circular.
//   - subdirectories — NOT walked. A nested directory is reachable only through
//     an explicit `_stack.yaml` step; an unreferenced one contributes
//     nothing, which soul-lint is to report as `vars_dir_nested` (NIM-416, not
//     yet built — until then a stray directory is silent).
//
// A missing `vars/` directory is not an error: the service simply carries no
// vars of its own. Only read failures and invalid YAML are errors.
// A `vars/_stack.yaml` declares the order explicitly and wins; without one, the
// order is the lexical scan of the directory. The two are never combined: a stack
// that lists some of the files would leave the rest to be applied by a rule
// nobody wrote down.
func (r *Resolver) Resolve(in ResolveInput) (map[string]any, error) {
	stack, err := r.readStack(in.ServiceDir)
	if err != nil {
		return nil, err
	}
	if stack != nil {
		return r.resolveStack(in, stack)
	}

	return r.lexical(in.ServiceDir)
}

// lexical is the directory-order assembly: the default mode, and the whole of
// [Resolver.ResolveLexical].
func (r *Resolver) lexical(serviceDir string) (map[string]any, error) {
	files, err := r.layerFiles(serviceDir)
	if err != nil {
		return nil, err
	}
	result := make(map[string]any)
	for _, rel := range files {
		layer, _, err := r.readLayer(serviceDir, rel)
		if err != nil {
			return nil, err
		}
		// A layer file may declare its own merge strategy (`_strategy`) — in
		// lexical mode that is the only place it can be said.
		result, err = applyLayer(result, layer, strategyDeep)
		if err != nil {
			return nil, fmt.Errorf("servicevars: %s: %w", rel, err)
		}
	}
	return result, nil
}

// ResolveLexical assembles the service's vars from the FILES ALONE — every
// `*.yaml`/`*.yml` directly inside `vars/`, in lexical order, `_strategy`
// honoured, `_stack.yaml` ignored entirely.
//
// For readers that want a service-level answer and have no incarnation to give:
// the directive catalog (`GET /v1/services/{name}/directives`) is one. A stack's
// conditionality is per-incarnation by construction — its steps read
// `incarnation.*` — so running it against an empty context would answer a
// question nobody asked, and reading one hard-coded file instead answers a
// narrower one than the service actually declares.
func (r *Resolver) ResolveLexical(serviceDir string) (map[string]any, error) {
	return r.lexical(serviceDir)
}

// layerFiles lists the layer files of `<serviceDir>/vars/` in lexical order —
// regular `*.yaml`/`*.yml` entries only, `_stack.yaml` excluded, subdirectories
// not descended into. Returns paths relative to serviceDir. A missing directory
// gives (nil, nil).
func (r *Resolver) layerFiles(serviceDir string) ([]string, error) {
	full, err := securejoin.SecureJoin(serviceDir, varsDir)
	if err != nil {
		return nil, fmt.Errorf("servicevars: unsafe vars dir path: %w", err)
	}

	entries, err := os.ReadDir(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A snapshot still on the RETIRED layout is refused rather than
			// resolved to nothing (ADR-0082). This is the dangerous window of the
			// migration: a repo whose scenarios were rewritten to `vars.` while the
			// directory rename was missed resolves zero vars, and the 255
			// `default(vars.X, y)` / `has(vars.X)` sites across the shipped
			// examples then take their FALLBACK — runs go green on default
			// conf_dirs and default mirror URLs, with no error, no log and no lint.
			// Absence of both directories is the honest "this service has no vars".
			if retired, rerr := r.hasRetiredLayout(serviceDir); rerr == nil && retired {
				return nil, fmt.Errorf("servicevars: %s/ is the retired layout (ADR-0082) and %s/ does not exist — "+
					"rename the directory and its base file to %s/00-base.yaml", retiredDir, varsDir, varsDir)
			}
			r.logger.Debug("servicevars: no vars directory, skipping", "dir", varsDir)
			return nil, nil
		}
		return nil, fmt.Errorf("servicevars: read vars dir: %w", err)
	}

	var rels []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			r.logger.Debug("servicevars: nested directory is not walked", "dir", name)
			continue
		}
		if name == stackFileMisspelt {
			return nil, fmt.Errorf("%w: %s is not read — the pipeline file is %s",
				ErrStackStepInvalid, stackFileMisspelt, stackFile)
		}
		if name == stackFile || !isYAML(name) {
			continue
		}
		rels = append(rels, path.Join(varsDir, name))
	}
	sort.Strings(rels)
	return rels, nil
}

// hasRetiredLayout reports whether the snapshot still carries the pre-ADR-0082
// `essence/` directory. Only consulted when `vars/` is absent, and only to turn
// "no vars" into an error that names the cause.
func (r *Resolver) hasRetiredLayout(serviceDir string) (bool, error) {
	full, err := securejoin.SecureJoin(serviceDir, retiredDir)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// isYAML reports whether the file name carries a YAML extension. Both spellings
// are accepted so a service is not tripped up by the one it happens to use, and
// the comparison is case-INSENSITIVE: a `.YAML` layer that silently did not load
// is the same failure as one that loaded and was wrong.
func isYAML(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
}

// readLayer reads and parses a YAML layer by its relative path inside
// serviceDir. The path is resolved via securejoin (escaping serviceDir is
// excluded).
//
// The found flag is separate from the map on purpose: a file that exists and
// parses to nothing — empty, or comments only — is INDISTINGUISHABLE from a
// missing one by its map alone, and only one of the two is worth an error. A
// caller that conflated them told an author their file did not exist and advised
// `optional: true`, which would then silently skip a file placed on purpose.
func (r *Resolver) readLayer(serviceDir, rel string) (layer map[string]any, found bool, err error) {
	full, err := securejoin.SecureJoin(serviceDir, rel)
	if err != nil {
		return nil, false, fmt.Errorf("servicevars: unsafe layer path %q: %w", rel, err)
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.logger.Debug("servicevars: layer missing, skipping", "layer", rel)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("servicevars: read layer %q: %w", rel, err)
	}

	if err := yaml.Unmarshal(data, &layer); err != nil {
		return nil, false, fmt.Errorf("servicevars: parse layer %q: %w", rel, err)
	}
	return layer, true, nil
}
