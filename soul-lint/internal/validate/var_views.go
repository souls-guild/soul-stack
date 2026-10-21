package validate

// The views of `vars.*` each artifact forms, for the checks of var_graph.go
// (ADR-0082 amendment 2026-10-15, points 2, 5 and 12a):
//
//	validate-service:   the service rung — every layer file under vars/
//	validate-scenario:  service rung → scenario vars: → each task's block + task vars:
//	validate-destiny:   vars.yml → each task's block + task vars:
//
// Each command reports the findings of the layers it owns: a scenario's views carry
// the service rung only so that a scenario or task definition is judged against it,
// and a cycle a task closes through a service var is reported there.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/servicevars"
)

// The stub incarnation `validate-service` evaluates the service view on (point
// 12a). A manifest states no name of its own (NIM-726), so the service name is
// `--service-name` when given and a placeholder otherwise.
const (
	stubIncarnationID  = "stub-incarnation"
	stubServiceName    = "stub-service"
	stubServiceVersion = "stub-version"
)

// The Keeper's names for the rungs that are not files (keeper/internal/render/varview.go).
const (
	scenarioRungOrigin = "scenario vars:"
	taskRungOrigin     = "task vars:"
	destinyRungOrigin  = "vars.yml"
)

// loopDefaultAs is the loop binding a `loop:` without `as:` declares (docs/destiny/tasks.md §7).
const loopDefaultAs = "item"

// offlineVault stands in for the Keeper's Vault client, so `vault()` compiles in the
// task environment as it does in render. A read fails, and the compile check lets an
// evaluation error through.
type offlineVault struct{}

func (offlineVault) ReadKV(context.Context, string) (map[string]any, error) {
	return nil, errors.New("vault is not read offline")
}

// A construction failure leaves an engine nil, which disables the compile check of
// the layers that use it rather than failing the lint.
var (
	// taskVarsEngine is the environment render compiles a task's and a vars.yml's
	// definitions in: cel.New with a Vault reader (keeper/internal/render/varview.go,
	// taskRung and destinyBaseViews over Pipeline.cel).
	taskVarsEngine = sync.OnceValue(func() *cel.Engine {
		eng, _ := cel.New(cel.WithVault(offlineVault{}))
		return eng
	})
	scenarioVarsEngine = sync.OnceValue(func() *cel.Engine {
		eng, _ := cel.NewScenarioVars()
		return eng
	})
)

// scenarioRungScope is the reader a scenario `vars:` definition compiles for: a
// host task's run-level values. On the loop axis `compute.*` is refused for the
// reader (point 6), which is the reader's error, not the definition's.
var scenarioRungScope = cel.Vars{ComputeScope: cel.ComputeAvailable, HostScope: cel.HostBound}

// scenarioTaskScope is the context render compiles a scenario task's own vars in:
// hostVars for a host task, keeperVars for a keeper-side one
// (keeper/internal/render/cel_render.go, dispatch.go).
func scenarioTaskScope(t config.Task) cel.Vars {
	if config.IsKeeperSideTask(t) {
		return cel.Vars{AllowRegisterHosts: true, ComputeScope: cel.ComputeAvailable, HostScope: cel.HostFreeKeeper}
	}
	return cel.Vars{AllowHosts: true, ComputeScope: cel.ComputeAvailable, HostScope: cel.HostBound}
}

// destinyScope is hostVars of the destiny pass: no host accessor, no compute.
var destinyScope = cel.Vars{ComputeScope: cel.ComputeOutOfScopeDestiny, HostScope: cel.HostBound}

// withLoop declares a task's loop binding, which its own vars may read.
func withLoop(scope cel.Vars, loop *config.LoopSpec) cel.Vars {
	if loop == nil {
		return scope
	}
	as := loop.As
	if as == "" {
		as = loopDefaultAs
	}
	scope.Loop = map[string]any{as: nil}
	if loop.IndexAs != "" {
		scope.Loop[loop.IndexAs] = nil
	}
	return scope
}

// fileSite locates a definition at prefix+name in one file.
func fileSite(file string, data []byte, prefix string) func(string) varSite {
	position := func(p string) (int, int, bool) { return config.PositionIn(data, p) }
	return func(name string) varSite {
		return varSite{file: file, yamlPath: prefix + name, position: position}
	}
}

// serviceVarLayers is the service rung of the service rooted at root, as the static
// walk reads it: every layer any incarnation can read, in the Keeper's order, and the
// literal step files it lacks ([servicevars.EveryLayer]).
func serviceVarLayers(root string, owned bool) ([]*varLayer, []servicevars.StepFileMissingError, error) {
	layers, missing, err := servicevars.EveryLayer(os.DirFS(root))
	if err != nil {
		return nil, nil, err
	}
	out := make([]*varLayer, 0, len(layers))
	for _, l := range layers {
		file := filepath.Join(root, filepath.FromSlash(layerOriginFile(l.Origin)))
		data, _ := os.ReadFile(file)
		out = append(out, &varLayer{
			rung:        cel.RungService,
			origin:      l.Origin,
			defs:        l.Defs,
			strategy:    l.Strategy,
			conditional: l.Conditional,
			oneOf:       l.OneOf,
			owned:       owned,
			site:        fileSite(file, data, "$."),
			engine:      l.Engine,
			scope:       l.Base,
		})
	}
	return out, missing, nil
}

func serviceVarViewDiags(root, serviceName string, stub bool) []diag.Diagnostic {
	return serviceVarChecks(root, serviceName, stub).out
}

// serviceVarChecks checks the service view of the service rooted at root, and, when
// stub is set, evaluates it on the stub incarnation. stub is off when `_stack.yaml`
// cannot be read: stack_step_invalid already says why, and the static walk cannot
// assemble the rung either. A layer it cannot read otherwise is reported, and the
// stub evaluation, which would stop on the same file, is skipped.
func serviceVarChecks(root, serviceName string, stub bool) *varChecker {
	c := newVarChecker(true)
	layers, missing, err := serviceVarLayers(root, true)
	if err != nil {
		if stub {
			c.out = append(c.out, serviceRungUnreadable(root, root, err, ""))
		}
		return c
	}
	c.out = append(c.out, missingStepFileDiags(root, missing)...)
	c.check(&varView{layers: layers, perLayer: true})
	if stub {
		c.out = append(c.out, serviceStubDiags(root, serviceName, c, missing)...)
	}
	return c
}

// missingStepFileDiags reports each literal step file the service lacks, at the
// step: it names one path for every incarnation, so every incarnation that stacks
// the step fails its assembly on it, whatever the step's `when:` or `foreach:`.
func missingStepFileDiags(root string, missing []servicevars.StepFileMissingError) []diag.Diagnostic {
	if len(missing) == 0 {
		return nil
	}
	stackPath := filepath.Join(root, varsDirName, config.StackFileName)
	data, _ := os.ReadFile(stackPath)
	out := make([]diag.Diagnostic, 0, len(missing))
	for _, m := range missing {
		d := diag.Diagnostic{
			Level:    diag.LevelError,
			Phase:    diag.PhaseSemanticValidate,
			File:     stackPath,
			Code:     codeVarInvalid,
			Message:  fmt.Sprintf("%s/%s step %d: %v", varsDirName, config.StackFileName, m.Step, &m),
			Hint:     "every incarnation that stacks this step fails its assembly before its render: add the file, or tolerate its absence with optional: true",
			YAMLPath: fmt.Sprintf("$.stack[%d].file", m.Step-1),
		}
		if line, col, ok := config.PositionIn(data, d.YAMLPath); ok {
			d.Line, d.Column = line, col
		}
		out = append(out, d)
	}
	return out
}

// serviceRungUnreadable reports a service rung the static walk cannot assemble, on
// one line: at the layer file and its position when the error names them, at
// `vars/` otherwise. shown is root as the command displays its paths.
func serviceRungUnreadable(root, shown string, err error, consequence string) diag.Diagnostic {
	d := diag.Diagnostic{
		Level:   diag.LevelError,
		Phase:   diag.PhaseSemanticValidate,
		File:    filepath.Join(shown, varsDirName),
		Code:    codeVarInvalid,
		Message: "the service rung cannot be assembled: " + firstLine(strings.TrimPrefix(err.Error(), "servicevars: ")) + consequence,
		Hint:    "a run whose incarnation reads this layer fails before its render; the service's vars are checked once every layer reads",
	}
	var le *servicevars.LayerError
	if !errors.As(err, &le) {
		return d
	}
	d.File = filepath.Join(shown, filepath.FromSlash(le.File))
	cause := ": " + firstLine(le.Err.Error())
	if errors.Is(le.Err, config.ErrStackStepInvalid) {
		data, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(le.File)))
		if line, col, ok := config.PositionIn(data, "$._strategy"); ok {
			d.Line, d.Column = line, col
		}
	} else {
		parse := config.YAMLParseDiagnostic(d.File, le.Err)
		d.Line, d.Column, cause = parse.Line, parse.Column, " does not parse: "+firstLine(parse.Message)
	}
	d.Message = "the service rung cannot be assembled: " + le.File + cause + consequence
	return d
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// serviceStubDiags evaluates every name of the service view, assembled as the
// Keeper assembles it, on the stub incarnation: placeholder id, no covens, no
// traits (point 12a). A layer the stub does not select — a coven- or `when:`-gated
// step, a file composed from the placeholder id — is left to the static walk, which
// reads every conditional layer. A failure the static walk already reported is not
// repeated: a ring by its names, a definition by its name and layer, a step file in
// missing by its step.
func serviceStubDiags(root, serviceName string, static *varChecker, missing []servicevars.StepFileMissingError) []diag.Diagnostic {
	name := serviceName
	if name == "" {
		name = stubServiceName
	}
	inc := servicevars.IncarnationContext{
		ID:             stubIncarnationID,
		Service:        name,
		ServiceVersion: stubServiceVersion,
		Traits:         map[string]any{},
	}
	stub := fmt.Sprintf("a stub incarnation (service %q, id %q, no covens, no traits)", name, stubIncarnationID)
	hint := "a service var is evaluated for every incarnation: read an optional value through has() or default(), " +
		"e.g. default(incarnation.traits.<key>, …)"
	varsDir := filepath.Join(root, varsDirName)

	layers, err := servicevars.StubLayers(os.DirFS(root), inc)
	if err != nil {
		if gone := (*servicevars.StepFileMissingError)(nil); errors.As(err, &gone) && slices.Contains(missing, *gone) {
			return nil
		}
		return []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseSemanticValidate,
			File:    varsDir,
			Code:    codeVarInvalid,
			Message: fmt.Sprintf("the service view cannot be assembled for %s: %s", stub, strings.TrimPrefix(err.Error(), "servicevars: ")),
			Hint:    "a run assembles the service rung before its render, so this fails the run of every incarnation the step fails for",
		}}
	}
	view := cel.NewVarView(nil, layers...)
	c := newVarChecker(false)
	for _, n := range view.Names() {
		_, _, err := view.Lookup(n)
		if err == nil {
			continue
		}
		var cyc *cel.VarCycleError
		if errors.As(err, &cyc) {
			if !static.rings[ringKey(cyc.Path[:len(cyc.Path)-1])] {
				c.emit(stubCycleSite(root, layers, cyc), "", codeVarCycle, cyc.Error(),
					"evaluating the service view on the stub incarnation found this ring; "+
						"break the chain at one of: vars."+strings.Join(slices.Compact(slices.Sorted(slices.Values(cyc.Path))), ", vars."))
			}
			continue
		}
		s := varSite{file: varsDir}
		var ve *cel.VarError
		if errors.As(err, &ve) {
			if static.flagged[flagKey(ve.Name, ve.Origin)] {
				continue
			}
			file := filepath.Join(root, filepath.FromSlash(layerOriginFile(ve.Origin)))
			data, _ := os.ReadFile(file)
			s = fileSite(file, data, "$.")(ve.Name)
		}
		c.emit(s, "", codeVarInvalid, fmt.Sprintf("%v — evaluating the service view on %s", err, stub), hint)
	}
	return c.out
}

// stubCycleSite is the definition a cycle the stub evaluation raised is cited at: its
// first name's, the lowest one for a self-reference with nothing below it and the
// top one otherwise.
func stubCycleSite(root string, layers []cel.VarLayer, cyc *cel.VarCycleError) varSite {
	name := cyc.Path[0]
	self := len(cyc.Path) == 2
	at := -1
	for i, l := range layers {
		if _, ok := l.Defs[name]; ok && (at < 0 || !self) {
			at = i
		}
	}
	if at < 0 {
		return varSite{file: filepath.Join(root, varsDirName)}
	}
	file := filepath.Join(root, filepath.FromSlash(layerOriginFile(layers[at].Origin)))
	data, _ := os.ReadFile(file)
	return fileSite(file, data, "$.")(name)
}

// layerOriginFile is the file a service layer's origin names: the origin itself for
// a lexical layer, its tail for a `_stack.yaml` step ("vars/_stack.yaml step 2:
// vars/x.yaml").
func layerOriginFile(origin string) string {
	if i := strings.LastIndex(origin, ": "); i >= 0 {
		return origin[i+2:]
	}
	return origin
}

func scenarioVarViewDiags(scenarioPath string, src []byte, scn *config.ScenarioManifest) []diag.Diagnostic {
	if scn == nil {
		return nil
	}
	return scenarioVarChecks(scenarioPath, src, scn).out
}

// scenarioVarChecks checks the views of a scenario: its run-level view (the service
// rung under its `vars:`) and one per task carrying block or task vars. Each walks
// the service rung in S_all and S_base only ([varView.eachSelection]): the S_L of
// each conditional layer is validate-service's. Without a service around it — a
// scenario linted loose — or with a service rung that does not assemble, the names
// of the service rung are unknown, so no read is judged unknown and no
// self-reference finds nothing below.
func scenarioVarChecks(scenarioPath string, src []byte, scn *config.ScenarioManifest) *varChecker {
	known := scenarioServiceLevelDir(scenarioPath) != ""
	var run []*varLayer
	var unreadable []diag.Diagnostic
	if known {
		root := scenarioServiceRoot(scenarioPath)
		layers, _, err := serviceVarLayers(root, false)
		if err != nil {
			known = false
			unreadable = append(unreadable, serviceRungUnreadable(root, pathLike(scenarioPath, root), err,
				"; this scenario's vars are checked without it, and no read of theirs is judged unknown"))
		}
		run = layers
	}
	c := newVarChecker(known)
	c.out = unreadable
	if len(scn.Vars) > 0 {
		run = append(run, &varLayer{
			rung:   cel.RungScenario,
			origin: scenarioRungOrigin,
			defs:   scn.Vars,
			owned:  true,
			site:   fileSite(scenarioPath, src, "$.vars."),
			engine: scenarioVarsEngine(),
			scope:  scenarioRungScope,
		})
	}
	c.check(&varView{layers: run, lowerUnknown: !known})

	files := newTaskFiles()
	files.index(scenarioPath, src, scn.Tasks, "$.tasks")
	tasks, bodies := expandScenarioTasks(scenarioPath, scn)
	for _, b := range bodies {
		files.indexBody(b)
	}
	walkTaskVars(tasks, nil, func(t *config.Task, vars map[string]taskVar) {
		layer := files.layer(t, vars, scenarioTaskScope(*t))
		c.check(&varView{layers: append(slices.Clone(run), layer), lowerUnknown: !known})
	})
	return c
}

// destinyVarViewDiags checks the views of a destiny: its `vars.yml` alone and one
// per task carrying block or task vars. The destiny pass sees none of its caller's
// names, so these views are the whole of what its definitions can read. A
// `vars.yml` that does not parse is reported, and the tasks' views are checked
// without it, as unknown.
func destinyVarViewDiags(manifestPath string) []diag.Diagnostic {
	dir := filepath.Dir(manifestPath)
	c := newVarChecker(true)
	var base []*varLayer
	unknown := false
	varsPath := filepath.Join(dir, "vars.yml")
	if data, err := os.ReadFile(varsPath); err == nil {
		defs, err := config.LoadDestinyVarsFromBytes(varsPath, data)
		switch {
		case err != nil:
			c.unknownRefs, unknown = false, true
			c.out = append(c.out, config.YAMLParseDiagnostic(varsPath, err))
		case len(defs) > 0:
			base = append(base, &varLayer{
				rung:   cel.RungDestinyFile,
				origin: destinyRungOrigin,
				defs:   defs,
				owned:  true,
				site:   fileSite(varsPath, data, "$."),
				engine: taskVarsEngine(),
				scope:  destinyScope,
			})
		}
	}
	c.check(&varView{layers: base})

	tasksPath := filepath.Join(dir, destinyTasksDir, destinyTasksEntryFile)
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		return c.out
	}
	tasks, _, err := config.LoadDestinyTasksFromBytes(tasksPath, data, config.ValidateOptions{})
	if err != nil || len(tasks) == 0 {
		return c.out
	}
	files := newTaskFiles()
	files.index(tasksPath, data, tasks, "$")
	root, aerr := filepath.Abs(dir)
	if aerr != nil {
		root = dir
	}
	resolve := destinyIncludeResolver(root, dir)
	recording := func(name string) ([]byte, string, error) {
		body, display, err := resolve(name)
		if err == nil {
			files.indexBody(includedBody{path: display, data: body})
		}
		return body, display, err
	}
	if expanded, _ := config.ExpandIncludesInDestiny(tasks, recording); expanded != nil {
		tasks = expanded
	}
	walkTaskVars(tasks, nil, func(t *config.Task, vars map[string]taskVar) {
		c.check(&varView{layers: append(slices.Clone(base), files.layer(t, vars, destinyScope)), lowerUnknown: unknown})
	})
	return c.out
}

// taskVar is a definition of a task's rung and the task it is written in: the task
// itself, or the block it inherits the name from.
type taskVar struct {
	def  any
	task *config.Task
}

// walkTaskVars visits every task that renders, with its block + task vars merged
// as render merges them (mergeBlockInheritance: shallow, the nearer definition
// winning). A block renders no view of its own, so it is visited through its
// children.
func walkTaskVars(tasks []config.Task, inherited map[string]taskVar, visit func(*config.Task, map[string]taskVar)) {
	for i := range tasks {
		t := &tasks[i]
		vars := inherited
		if len(t.Vars) > 0 {
			vars = make(map[string]taskVar, len(inherited)+len(t.Vars))
			maps.Copy(vars, inherited)
			for name, def := range t.Vars {
				vars[name] = taskVar{def: def, task: t}
			}
		}
		if t.Block != nil {
			walkTaskVars(t.Block.Block, vars, visit)
			continue
		}
		if len(vars) > 0 {
			visit(t, vars)
		}
	}
}

// taskFiles locates a task's definitions after include expansion: a task carries
// its file and line, and each file's own task list maps that line back to the
// task's YAML path in it.
type taskFiles struct {
	data  map[string][]byte
	paths map[string]map[int]string
}

func newTaskFiles() *taskFiles {
	return &taskFiles{data: map[string][]byte{}, paths: map[string]map[int]string{}}
}

func (f *taskFiles) index(file string, data []byte, tasks []config.Task, prefix string) {
	f.data[file] = data
	lines := map[int]string{}
	var walk func(ts []config.Task, prefix string)
	walk = func(ts []config.Task, prefix string) {
		for i := range ts {
			p := fmt.Sprintf("%s[%d]", prefix, i)
			if _, dup := lines[ts[i].Source.Line]; !dup && ts[i].Source.Line > 0 {
				lines[ts[i].Source.Line] = p
			}
			if ts[i].Block != nil {
				walk(ts[i].Block.Block, p+".block")
			}
		}
	}
	walk(tasks, prefix)
	f.paths[file] = lines
}

// indexBody indexes an included body, a bare task list. Its own diagnostics are
// the expander's to report.
func (f *taskFiles) indexBody(b includedBody) {
	if _, done := f.paths[b.path]; done {
		return
	}
	tasks, _, _ := config.LoadDestinyTasksFromBytes(b.path, b.data, config.ValidateOptions{})
	f.index(b.path, b.data, tasks, "$")
}

func (f *taskFiles) site(t *config.Task, name string) varSite {
	file, line := t.Source.File, t.Source.Line
	if p, ok := f.paths[file][line]; ok {
		data := f.data[file]
		return varSite{file: file, yamlPath: p + ".vars." + name, position: func(p string) (int, int, bool) {
			return config.PositionIn(data, p)
		}}
	}
	return varSite{file: file, yamlPath: "$.vars." + name, position: func(string) (int, int, bool) {
		return line, 0, line > 0
	}}
}

// layer is the block + task rung of task t, compiled in scope with t's loop binding.
func (f *taskFiles) layer(t *config.Task, vars map[string]taskVar, scope cel.Vars) *varLayer {
	defs := make(map[string]any, len(vars))
	for name, v := range vars {
		defs[name] = v.def
	}
	return &varLayer{
		rung:   cel.RungTask,
		origin: taskRungOrigin,
		defs:   defs,
		owned:  true,
		site:   func(name string) varSite { return f.site(vars[name].task, name) },
		engine: taskVarsEngine(),
		scope:  withLoop(scope, t.Loop),
	}
}
