package servicevars

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Layers assembles the service rung of the snapshot fsys for one incarnation. A
// `vars/_stack.yaml` declares the order and wins; without one the order is the
// lexical scan of the directory. The two are never combined: a stack that lists
// some of the files would leave the rest to be applied by a rule nobody wrote
// down.
//
// Lexical order is the base convention: `00-base.yaml` first, because once the
// directory listing IS the order the file name carries the precedence. This is
// why the base file is NOT `_default.yaml` — `_` is 0x5F, between 'Z' and 'a', so
// it sorts ahead of its siblings only while every one of them is lower-case.
//
// Not layers, and deliberately so: `_stack.yaml` (it declares the order;
// including it in that order would be circular) and subdirectories (not walked;
// a nested file is reachable only through a `_stack.yaml` step, and soul-lint
// reports an unreferenced one as `vars_dir_nested`).
//
// Every layer shares the package's one [cel.NewServiceVars] engine and the same
// Base, the incarnation and nothing else; the selectors are evaluated with
// [cel.NewServiceVarsSelector]. A missing `vars/` is no layers, not an error.
// logger nil discards the debug lines.
func Layers(fsys fs.FS, inc IncarnationContext, logger *slog.Logger) ([]cel.VarLayer, error) {
	a, err := newAssembly(fsys, inc, logger)
	if err != nil {
		return nil, err
	}
	stack, err := a.readStack()
	if err != nil {
		return nil, err
	}
	if stack != nil {
		return a.stackLayers(stack)
	}
	return a.lexical()
}

// LexicalLayers assembles the FILES ALONE — every `*.yaml`/`*.yml` directly inside
// `vars/`, in lexical order, `_strategy` honoured, `_stack.yaml` ignored entirely.
//
// For a reader that wants a service-level answer and has no incarnation to give:
// the directive catalog (`GET /v1/services/{id}/directives`) is one. A stack's
// conditionality is per-incarnation by construction — its steps read
// `incarnation.*` — so running it against an empty context would answer a
// question nobody asked, and reading one hard-coded file instead answers a
// narrower one than the service actually declares.
func LexicalLayers(fsys fs.FS, inc IncarnationContext, logger *slog.Logger) ([]cel.VarLayer, error) {
	a, err := newAssembly(fsys, inc, logger)
	if err != nil {
		return nil, err
	}
	return a.lexical()
}

// StubLayers is [Layers] for an incarnation whose identity is made up — soul-lint's
// stub (ADR-0082 amendment 2026-10-15, point 12a). A composed `file:` that reads
// `incarnation.*`, itself or through the binding of a `foreach:` that reads it, and
// renders to a file the snapshot lacks is skipped rather than refused: the path names
// the placeholder, not an incarnation anyone registered, and [EveryLayer] covers
// every file the step can name. Any other missing file is refused as [Layers]
// refuses it, a literal one whatever its `foreach:`.
func StubLayers(fsys fs.FS, inc IncarnationContext) ([]cel.VarLayer, error) {
	a, err := newAssembly(fsys, inc, nil)
	if err != nil {
		return nil, err
	}
	a.stub = true
	stack, err := a.readStack()
	if err != nil {
		return nil, err
	}
	if stack != nil {
		return a.stackLayers(stack)
	}
	return a.lexical()
}

// StaticLayer is a layer of [EveryLayer]. Conditional: some incarnation's assembly
// leaves it out — its step has a `when:`, or which files it stacks depends on
// `incarnation.*`. `optional:` alone does not make a layer conditional: the snapshot
// is the one the Keeper reads, and a file present in it is read. OneOf: its step is
// conditional, its `file:` is composed and it has no `foreach:`, so the step stacks
// at most one file per incarnation and no incarnation stacks two of its files.
type StaticLayer struct {
	cel.VarLayer
	Conditional bool
	OneOf       bool
}

// EveryLayer assembles every layer any incarnation can read, in the order [Layers]
// stacks them, for a static walk that must cover them all (ADR-0082 amendment
// 2026-10-15, point 12a). Without `_stack.yaml` that is [Layers]' lexical scan.
// With one it is each step's files, in step order and with the step's strategy,
// whatever its `when:`. A step whose `foreach:` and `file:` read nothing of
// `incarnation.*` is evaluated as the Keeper evaluates it, so its files are stacked
// in binding order. Otherwise a literal `file:` names one file, and a composed one
// every layer file under `vars/` its skeleton can match ([StepFileMatcher]),
// lexically, at that step's position; without a `foreach:` those files are
// alternatives (OneOf). A file no step can name is not a layer: the
// Keeper never reads it. A literal `file:` of a step that is not `optional:` and
// names a file the snapshot lacks is in missing, once per step, whatever the step's
// `when:` or `foreach:`: every incarnation that stacks the step fails its assembly
// on it. Any other missing file, or one a `file:` places outside `vars/`, is left to
// the evaluation of a stack that selects it. A layer's Base carries no incarnation.
func EveryLayer(fsys fs.FS) (layers []StaticLayer, missing []StepFileMissingError, err error) {
	a, err := newAssembly(fsys, IncarnationContext{}, nil)
	if err != nil {
		return nil, nil, err
	}
	stack, err := a.readStack()
	if err != nil {
		return nil, nil, err
	}
	if stack == nil {
		lexical, err := a.lexical()
		if err != nil {
			return nil, nil, err
		}
		layers := make([]StaticLayer, 0, len(lexical))
		for _, l := range lexical {
			layers = append(layers, StaticLayer{VarLayer: l})
		}
		return layers, nil, nil
	}
	var candidates []string
	for i, step := range stack.Stack {
		rels, fixed := a.fixedFiles(step)
		if !fixed {
			if rels, err = a.reachableFiles(step.File, &candidates); err != nil {
				return nil, nil, err
			}
		}
		_, literal := stepLiteral(step.File)
		oneOf := !fixed && !literal && step.Foreach == ""
		reported := false
		for _, rel := range rels {
			full := path.Join(varsDir, rel)
			raw, found, err := a.readLayer(full)
			if err != nil {
				return nil, nil, fmt.Errorf("servicevars: %s step %d: %w", stackFile, i+1, err)
			}
			if !found {
				if literal && !step.Optional && !reported {
					missing = append(missing, StepFileMissingError{Step: i + 1, File: rel})
					reported = true
				}
				continue
			}
			layer, err := a.layer(fmt.Sprintf("%s/%s step %d: %s", varsDir, stackFile, i+1, full), raw, step.Strategy)
			if err != nil {
				return nil, nil, fmt.Errorf("servicevars: %s step %d: %w", stackFile, i+1, &LayerError{File: full, Err: err})
			}
			layers = append(layers, StaticLayer{VarLayer: layer, Conditional: step.When != "" || !fixed, OneOf: oneOf})
		}
	}
	return layers, missing, nil
}

// fixedFiles is the files step stacks for every incarnation, in the Keeper's order,
// when neither its `foreach:` nor its `file:` reads `incarnation.*`; fixed is false
// otherwise, and when the Keeper's evaluation of either fails, which the evaluation
// of a stack that selects the step reports.
func (a *assembly) fixedFiles(step config.ServiceVarsStackStep) (rels []string, fixed bool) {
	if a.readsIncarnation(step.Foreach) || a.readsIncarnation(step.File) {
		return nil, false
	}
	binds, err := a.stepBindings(step)
	if err != nil {
		return nil, false
	}
	for _, bind := range binds {
		sel := a.base
		sel.Loop = bind
		rel, err := a.stepFile(step, sel)
		if err != nil {
			return nil, false
		}
		rels = append(rels, rel)
	}
	return rels, true
}

// reachableFiles is every file a `file:` can name, as a path relative to `vars/`:
// a literal one its own, unless it leaves `vars/`; a composed one every layer file
// its skeleton matches, lexically. candidates caches the layer files for the steps
// of one stack.
func (a *assembly) reachableFiles(file string, candidates *[]string) ([]string, error) {
	if literal, ok := stepLiteral(file); ok {
		if leavesVarsDir(literal) {
			return nil, nil
		}
		return []string{literal}, nil
	}
	if *candidates == nil {
		all, err := a.nestedLayerFiles()
		if err != nil {
			return nil, err
		}
		*candidates = all
	}
	match := StepFileMatcher(file)
	var rels []string
	for _, rel := range *candidates {
		if match(rel) {
			rels = append(rels, rel)
		}
	}
	return rels, nil
}

// nestedLayerFiles lists, sorted, every layer file under `vars/` at any depth, as a
// path relative to `vars/`: the files a composed `file:` can render to.
func (a *assembly) nestedLayerFiles() ([]string, error) {
	rels := []string{}
	err := fs.WalkDir(a.fsys, varsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() || !isYAML(name) || (path.Dir(p) == varsDir && (name == stackFile || name == stackFileMisspelt)) {
			return nil
		}
		rels = append(rels, strings.TrimPrefix(p, varsDir+"/"))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("servicevars: walk %s: %w", varsDir, err)
	}
	sort.Strings(rels)
	return rels, nil
}

// assembly is one call of [Layers], [StubLayers], [LexicalLayers] or [EveryLayer].
type assembly struct {
	fsys   fs.FS
	logger *slog.Logger
	// engine compiles the layers' definitions, selector the steps' selectors.
	engine, selector *cel.Engine
	// base is every layer's Base and every selector's activation, a selector
	// adding its `foreach:` binding as Loop.
	base cel.Vars
	// stub: the incarnation is [StubLayers]'.
	stub bool
}

func newAssembly(fsys fs.FS, inc IncarnationContext, logger *slog.Logger) (*assembly, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	engine, err := serviceVarsEngine()
	if err != nil {
		return nil, fmt.Errorf("servicevars: cel engine: %w", err)
	}
	selector, err := selectorEngine()
	if err != nil {
		return nil, fmt.Errorf("servicevars: cel selector engine: %w", err)
	}
	return &assembly{fsys: fsys, logger: logger, engine: engine, selector: selector, base: cel.Vars{Incarnation: inc.celMap()}}, nil
}

func (a *assembly) lexical() ([]cel.VarLayer, error) {
	files, err := a.layerFiles()
	if err != nil {
		return nil, err
	}
	layers := make([]cel.VarLayer, 0, len(files))
	for _, rel := range files {
		raw, found, err := a.readLayer(rel)
		if err != nil {
			return nil, fmt.Errorf("servicevars: %w", err)
		}
		if !found {
			continue
		}
		layer, err := a.layer(rel, raw, config.StrategyDeep)
		if err != nil {
			return nil, fmt.Errorf("servicevars: %s: %w", rel, &LayerError{File: rel, Err: err})
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// layerFiles lists the layer files of `vars/` in lexical order — `*.yaml`/`*.yml`
// entries only, `_stack.yaml` excluded, subdirectories not descended into — as
// paths relative to the snapshot root. A missing directory gives (nil, nil).
func (a *assembly) layerFiles() ([]string, error) {
	entries, err := fs.ReadDir(a.fsys, varsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A snapshot still on the RETIRED layout is refused rather than
			// resolved to nothing (ADR-0082). This is the dangerous window of the
			// migration: a repo whose scenarios were rewritten to `vars.` while the
			// directory rename was missed resolves zero vars, and every
			// `default(vars.X, y)` / `has(vars.X)` then takes its FALLBACK — runs go
			// green on default conf_dirs and default mirror URLs, with no error, no
			// log and no lint. Absence of both directories is the honest "this
			// service has no vars".
			if a.hasRetiredLayout() {
				return nil, fmt.Errorf("servicevars: %s/ is the retired layout (ADR-0082) and %s/ does not exist — "+
					"rename the directory and its base file to %s/00-base.yaml", retiredDir, varsDir, varsDir)
			}
			a.logger.Debug("servicevars: no vars directory, skipping", "dir", varsDir)
			return nil, nil
		}
		return nil, fmt.Errorf("servicevars: read vars dir: %w", err)
	}

	var rels []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			a.logger.Debug("servicevars: nested directory is not walked", "dir", name)
			continue
		}
		if name == stackFileMisspelt {
			return nil, fmt.Errorf("%w: %s is not read — the pipeline file is %s",
				config.ErrStackStepInvalid, stackFileMisspelt, stackFile)
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
// `essence/` directory. Only consulted when `vars/` is absent.
func (a *assembly) hasRetiredLayout() bool {
	info, err := fs.Stat(a.fsys, retiredDir)
	return err == nil && info.IsDir()
}

// isYAML reports whether the file name carries a YAML extension. Both spellings
// are accepted, and the comparison is case-INSENSITIVE: a `.YAML` layer that
// silently did not load is the same failure as one that loaded and was wrong.
func isYAML(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
}

// readLayer reads and parses a YAML layer by its path relative to the snapshot
// root.
//
// The found flag is separate from the map on purpose: a file that exists and
// parses to nothing — empty, or comments only — is INDISTINGUISHABLE from a
// missing one by its map alone, and only one of the two is worth an error. A
// caller that conflated them told an author their file did not exist and advised
// `optional: true`, which would then silently skip a file placed on purpose.
func (a *assembly) readLayer(rel string) (layer map[string]any, found bool, err error) {
	// Before the read, because what an FS does with an invalid path varies: an
	// os-backed one answers a bare fs.ErrInvalid, and fstest.MapFS answers
	// fs.ErrNotExist, which the branch below turns into a silent skip. A listed
	// file can carry such a name only by not being valid UTF-8.
	if !fs.ValidPath(rel) {
		return nil, false, fmt.Errorf("layer %q: the file name is not a valid path (it must be valid UTF-8); rename the file", rel)
	}
	data, err := fs.ReadFile(a.fsys, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			a.logger.Debug("servicevars: layer missing, skipping", "layer", rel)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read layer %q: %w", rel, err)
	}
	if err := yaml.Unmarshal(data, &layer); err != nil {
		return nil, false, fmt.Errorf("parse layer %q: %w", rel, &LayerError{File: rel, Err: err})
	}
	return layer, true, nil
}

// LayerError is a layer file that is not a layer: it does not parse, or declares
// `_strategy` wrongly. File is relative to the snapshot root. The text is Err's, the
// wrapping already names the file.
type LayerError struct {
	File string
	Err  error
}

func (e *LayerError) Error() string { return e.Err.Error() }

func (e *LayerError) Unwrap() error { return e.Err }

// layer makes a file's map a service-rung layer: `_strategy` lifted out and
// resolved per name over stepStrategy, the definitions left RAW.
func (a *assembly) layer(origin string, raw map[string]any, stepStrategy string) (cel.VarLayer, error) {
	defs, perKey, fileWide, err := takeLayerStrategy(raw)
	if err != nil {
		return cel.VarLayer{}, err
	}
	if stepStrategy == "" {
		stepStrategy = config.StrategyDeep
	}
	var strategy map[string]string
	if len(defs) > 0 {
		strategy = make(map[string]string, len(defs))
		for name := range defs {
			s := stepStrategy
			if fileWide != "" {
				s = fileWide
			}
			if k, ok := perKey[name]; ok {
				s = k
			}
			strategy[name] = s
		}
	}
	return cel.VarLayer{
		Rung:     cel.RungService,
		Origin:   origin,
		Defs:     defs,
		Strategy: strategy,
		Engine:   a.engine,
		Base:     a.base,
	}, nil
}

// takeLayerStrategy lifts the reserved `_strategy` key out of a layer and returns
// the layer without it. A malformed declaration is refused rather than ignored:
// `_strategy: shallow` silently deep-merging is the fail-open
// [config.ErrStackStepInvalid] exists to prevent, and a stray `_strategy` left in
// the map would surface later as a var nobody declared.
func takeLayerStrategy(layer map[string]any) (rest map[string]any, perKey map[string]string, fileWide string, err error) {
	raw, present := layer[strategyKey]
	if !present {
		return layer, nil, "", nil
	}
	rest = make(map[string]any, len(layer)-1)
	for k, v := range layer {
		if k != strategyKey {
			rest[k] = v
		}
	}
	switch t := raw.(type) {
	case string:
		if !validStrategy(t) {
			return nil, nil, "", fmt.Errorf("%w: %s: %q is not deep or replace", config.ErrStackStepInvalid, strategyKey, t)
		}
		return rest, nil, t, nil
	case map[string]any:
		perKey = make(map[string]string, len(t))
		for _, k := range slices.Sorted(maps.Keys(t)) {
			v := t[k]
			s, ok := v.(string)
			if !ok || !validStrategy(s) {
				return nil, nil, "", fmt.Errorf("%w: %s.%s: %v is not deep or replace", config.ErrStackStepInvalid, strategyKey, k, v)
			}
			perKey[k] = s
		}
		return rest, perKey, "", nil
	default:
		return nil, nil, "", fmt.Errorf("%w: %s must be a string or a map of key→strategy, got %T", config.ErrStackStepInvalid, strategyKey, raw)
	}
}

func validStrategy(s string) bool { return s == config.StrategyDeep || s == config.StrategyReplace }
