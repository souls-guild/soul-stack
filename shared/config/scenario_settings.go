package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ScenarioSettings is the `settings:` block (docs/scenario/settings.md): how a run
// behaves, declared by the scenario itself. It is written at the scenario level, on
// a top-level task, on a `block:` and on a block child at any depth, and each level
// is merged key by key over the one above ([MergeSettings]). A nil field is a key
// that level did not write.
//
// Parsed, validated and merged only — nothing reads the values yet (epic NIM-926).
// Merges copy the pointers, so a merged value is shared with the parsed tasks it
// came from: never write through a field.
// Unrelated to the Keeper's cluster-wide settings (`SettingsStore`).
type ScenarioSettings struct {
	OnFailure       *string   // OnFailureScenario | OnFailureHost
	Tolerate        *Tolerate // still nil after every level = no limit
	SoulTimeout     *time.Duration
	TaskTimeout     *time.Duration // 0 = no limit
	ScenarioTimeout *time.Duration
	SerialScope     *string // SerialScopeTask | SerialScopeBlock
}

// Tolerate is `tolerate:`: the run ends once more than N hosts — or more than N
// percent of the run's hosts, when Percent is set — were dropped under radius `host`.
type Tolerate struct {
	N       int
	Percent bool
}

const (
	OnFailureScenario = "scenario"
	OnFailureHost     = "host"
	SerialScopeTask   = "task"
	SerialScopeBlock  = "block"
)

// DefaultScenarioSettings is every key at its default (settings.md §2), the root
// the scenario's own settings merge over.
func DefaultScenarioSettings() ScenarioSettings {
	return ScenarioSettings{
		OnFailure:       new(OnFailureScenario),
		SoulTimeout:     new(time.Minute),
		TaskTimeout:     new(time.Duration(0)),
		ScenarioTimeout: new(24 * time.Hour),
		SerialScope:     new(SerialScopeTask),
	}
}

// MergeSettings merges child over parent key by key: a key the child wrote wins,
// every other key is the parent's.
func MergeSettings(parent, child ScenarioSettings) ScenarioSettings {
	out := parent
	if child.OnFailure != nil {
		out.OnFailure = child.OnFailure
	}
	if child.Tolerate != nil {
		out.Tolerate = child.Tolerate
	}
	if child.SoulTimeout != nil {
		out.SoulTimeout = child.SoulTimeout
	}
	if child.TaskTimeout != nil {
		out.TaskTimeout = child.TaskTimeout
	}
	if child.ScenarioTimeout != nil {
		out.ScenarioTimeout = child.ScenarioTimeout
	}
	if child.SerialScope != nil {
		out.SerialScope = child.SerialScope
	}
	return out
}

// OwnSettings is the level this task contributes, with its `timeout:` folded in as
// the `task_timeout` it is shorthand for (settings.md §2.3); an explicit
// `settings.task_timeout` wins.
func (t Task) OwnSettings() ScenarioSettings {
	var s ScenarioSettings
	if t.Settings != nil {
		s = *t.Settings
	}
	if s.TaskTimeout == nil && t.Timeout != "" {
		if d, err := ParseDuration(t.Timeout); err == nil {
			s.TaskTimeout = &d
		}
	}
	return s
}

// InheritSettings is the `settings:` row of block inheritance: the child's own level
// merged over the block's. Each side's `timeout:` is folded into its own level BEFORE
// the merge — the child's beats a `task_timeout` the block set, and the block's is
// the block's `task_timeout` (settings.md §2.3), which folding either over the merged
// result would get backwards. A block with neither key leaves the child as written.
func InheritSettings(block, child Task) *ScenarioSettings {
	if block.Settings == nil && block.Timeout == "" {
		return child.Settings
	}
	merged := MergeSettings(block.OwnSettings(), child.OwnSettings())
	return &merged
}

// InheritTimeout is the `timeout:` a block hands a child on the wire, which until
// task timeouts are read from `settings:` (NIM-949) is the only one the Soul
// applies. The child's own `timeout:` wins; a child whose own `settings:` writes
// `task_timeout` keeps none, so the wire carries no value a lower level's
// `task_timeout` overrode — that level's reaches the Soul with NIM-949, like every
// other `settings.task_timeout`. The one disagreement left is a level writing both
// spellings with different values: the wire carries its `timeout:`, the merge its
// `task_timeout`, and `timeout_shadowed_by_task_timeout` warns, as on a task.
// child is the child as written, before [InheritSettings] filled its settings
// with the block's.
func InheritTimeout(block, child Task) string {
	if child.Timeout != "" {
		return child.Timeout
	}
	if child.Settings != nil && child.Settings.TaskTimeout != nil {
		return ""
	}
	return block.Timeout
}

// UnmarshalYAML decodes through the same [decodeSettings] the validators run. The
// diagnostics are dropped: the validators report them, knowing the level.
func (s *ScenarioSettings) UnmarshalYAML(node ast.Node) error {
	if decoded, _ := decodeSettings(node, "", true); decoded != nil {
		*s = *decoded
	}
	return nil
}

// settingKind decides where a key may be written (settings.md §2). Only run-kind
// is restricted; the others differ in who reads them, not where they are written.
type settingKind int

const (
	settingTask settingKind = iota
	settingBarrier
	settingBlock
	settingRun
)

type settingKey struct {
	name string
	kind settingKind
	// instead names the task-level key to use when a run-kind key is written below
	// the scenario level.
	instead string
	decode  func(v ast.Node, path string, s *ScenarioSettings) []diag.Diagnostic
	written func(s ScenarioSettings) bool
}

// settingKeys is the whole schema of `settings:`. A new key declares its kind here,
// and the kind is its whole placement rule (settings.md §3).
var settingKeys = []settingKey{
	{name: "on_failure", kind: settingTask, decode: func(v ast.Node, path string, s *ScenarioSettings) []diag.Diagnostic {
		var d []diag.Diagnostic
		s.OnFailure, d = settingEnum(v, path, OnFailureScenario, OnFailureHost)
		return d
	}, written: func(s ScenarioSettings) bool { return s.OnFailure != nil }},
	{name: "tolerate", kind: settingBarrier, decode: func(v ast.Node, path string, s *ScenarioSettings) []diag.Diagnostic {
		var d []diag.Diagnostic
		s.Tolerate, d = settingTolerate(v, path)
		return d
	}, written: func(s ScenarioSettings) bool { return s.Tolerate != nil }},
	{name: "soul_timeout", kind: settingTask, decode: func(v ast.Node, path string, s *ScenarioSettings) []diag.Diagnostic {
		var d []diag.Diagnostic
		s.SoulTimeout, d = settingDuration(v, path, false)
		return d
	}, written: func(s ScenarioSettings) bool { return s.SoulTimeout != nil }},
	{name: "task_timeout", kind: settingTask, decode: func(v ast.Node, path string, s *ScenarioSettings) []diag.Diagnostic {
		var d []diag.Diagnostic
		s.TaskTimeout, d = settingDuration(v, path, true)
		return d
	}, written: func(s ScenarioSettings) bool { return s.TaskTimeout != nil }},
	{name: "scenario_timeout", kind: settingRun, instead: "task_timeout", decode: func(v ast.Node, path string, s *ScenarioSettings) []diag.Diagnostic {
		var d []diag.Diagnostic
		s.ScenarioTimeout, d = settingDuration(v, path, false)
		return d
	}, written: func(s ScenarioSettings) bool { return s.ScenarioTimeout != nil }},
	{name: "serial_scope", kind: settingBlock, decode: func(v ast.Node, path string, s *ScenarioSettings) []diag.Diagnostic {
		var d []diag.Diagnostic
		s.SerialScope, d = settingEnum(v, path, SerialScopeTask, SerialScopeBlock)
		return d
	}, written: func(s ScenarioSettings) bool { return s.SerialScope != nil }},
}

func lookupSettingKey(name string) (settingKey, bool) {
	for _, k := range settingKeys {
		if k.name == name {
			return k, true
		}
	}
	return settingKey{}, false
}

func settingKeyNames() string {
	names := make([]string, len(settingKeys))
	for i, k := range settingKeys {
		names[i] = k.name
	}
	return strings.Join(names, ", ")
}

// decodeSettings is the one parser and validator of a `settings:` value, shared by
// the YAML decode and both validators, so what a Keeper accepts and what soul-lint
// reports cannot drift. atScenario is false on every level below the scenario.
func decodeSettings(node ast.Node, path string, atScenario bool) (*ScenarioSettings, []diag.Diagnostic) {
	switch n := node.(type) {
	case nil, *ast.NullNode:
		return nil, nil
	case *ast.MappingNode:
		s := &ScenarioSettings{}
		var out []diag.Diagnostic
		for _, kv := range n.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			keyPath := path + "." + tok.Value
			k, known := lookupSettingKey(tok.Value)
			switch {
			case !known:
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "unknown_key",
					Message:  `unknown field "` + tok.Value + `" in settings:`,
					Hint:     "settings: takes " + settingKeyNames() + " — see docs/scenario/settings.md §2",
					YAMLPath: keyPath,
				}))
			case k.kind == settingRun && !atScenario:
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "settings_run_key_below_scenario",
					Message:  fmt.Sprintf("%s: bounds the whole run and is written only in the scenario's own settings:, not on a task or a block", k.name),
					Hint:     fmt.Sprintf("move it to the scenario's top-level settings:, or use %s: to bound this task", k.instead),
					YAMLPath: keyPath,
				}))
			default:
				out = append(out, k.decode(kv.Value, keyPath, s)...)
			}
		}
		return s, out
	default:
		return nil, []diag.Diagnostic{diagAt(lineOf(node), colOf(node), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "settings: must be a mapping",
			Hint:     "settings: { on_failure: host, task_timeout: 30m }",
			YAMLPath: path,
		})}
	}
}

func settingEnum(v ast.Node, path string, allowed ...string) (*string, []diag.Diagnostic) {
	sn, ok := v.(*ast.StringNode)
	if !ok {
		return nil, []diag.Diagnostic{settingTypeMismatch(v, path, "must be one of "+strings.Join(allowed, " | "))}
	}
	for _, a := range allowed {
		if sn.Value == a {
			return new(sn.Value), nil
		}
	}
	return nil, []diag.Diagnostic{diagAt(lineOf(v), colOf(v), diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:     "enum_invalid",
		Message:  fmt.Sprintf("%q is not one of %s", sn.Value, strings.Join(allowed, " | ")),
		YAMLPath: path,
	})}
}

var reTolerancePercent = regexp.MustCompile(`^(0|[1-9][0-9]*)%$`)

func settingTolerate(v ast.Node, path string) (*Tolerate, []diag.Diagnostic) {
	outOfRange := func(got string) []diag.Diagnostic {
		return []diag.Diagnostic{diagAt(lineOf(v), colOf(v), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "value_out_of_range",
			Message:  fmt.Sprintf("tolerate: %s must be a host count >= 0 or a percent \"N%%\" of 0..100", got),
			Hint:     `tolerate: 2 | tolerate: "10%"`,
			YAMLPath: path,
		})}
	}
	switch n := v.(type) {
	case *ast.IntegerNode:
		count, err := strconv.Atoi(n.GetToken().Value)
		if err != nil || count < 0 {
			return nil, outOfRange(n.GetToken().Value)
		}
		return &Tolerate{N: count}, nil
	case *ast.StringNode:
		if !reTolerancePercent.MatchString(n.Value) {
			return nil, outOfRange(strconv.Quote(n.Value))
		}
		pct, err := strconv.Atoi(strings.TrimSuffix(n.Value, "%"))
		if err != nil || pct > 100 {
			return nil, outOfRange(strconv.Quote(n.Value))
		}
		return &Tolerate{N: pct, Percent: true}, nil
	default:
		return nil, []diag.Diagnostic{settingTypeMismatch(v, path, `must be a host count or a percent string "N%"`)}
	}
}

// settingDuration reads the `duration` convention ([ParseDuration]). A bare YAML 0
// is accepted where zero is ("no limit" — task_timeout only); the spec writes it
// that way.
func settingDuration(v ast.Node, path string, zeroAllowed bool) (*time.Duration, []diag.Diagnostic) {
	var d time.Duration
	switch n := v.(type) {
	case *ast.IntegerNode:
		if n.GetToken().Value != "0" {
			return nil, []diag.Diagnostic{settingTypeMismatch(v, path, "must be a duration string such as 30s, 5m, 1h30m or 2d")}
		}
	case *ast.StringNode:
		parsed, err := ParseDuration(n.Value)
		if err != nil {
			return nil, []diag.Diagnostic{diagAt(lineOf(v), colOf(v), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "duration_invalid",
				Message:  fmt.Sprintf("invalid duration %q: %v", n.Value, err),
				Hint:     "examples: 30s, 5m, 1h30m, 2d",
				YAMLPath: path,
			})}
		}
		d = parsed
	default:
		return nil, []diag.Diagnostic{settingTypeMismatch(v, path, "must be a duration string such as 30s, 5m, 1h30m or 2d")}
	}
	if d < 0 || (d == 0 && !zeroAllowed) {
		want := "positive"
		if zeroAllowed {
			want = "0 (no limit) or positive"
		}
		return nil, []diag.Diagnostic{diagAt(lineOf(v), colOf(v), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "value_out_of_range",
			Message:  fmt.Sprintf("duration %s must be %s", d, want),
			YAMLPath: path,
		})}
	}
	return &d, nil
}

func settingTypeMismatch(v ast.Node, path, want string) diag.Diagnostic {
	return diagAt(lineOf(v), colOf(v), diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		Code:     "type_mismatch",
		Message:  path[strings.LastIndexByte(path, '.')+1:] + ": " + want,
		YAMLPath: path,
	})
}

// validateScenarioSettings checks the scenario-level `settings:`, the one level
// where run-kind keys belong.
func validateScenarioSettings(root *ast.MappingNode) []diag.Diagnostic {
	_, out := decodeSettings(findValueNode(root, "settings"), "$.settings", true)
	return out
}

// validateTaskSettings checks a task's `settings:`: refused outright where the block
// cannot be written (settings.md §3), otherwise checked as a level below the
// scenario. A task writing both `timeout:` and a different `settings.task_timeout`
// is warned about: by §2.3 the explicit key wins, but render still puts `timeout:`
// on the wire until task timeouts are read from settings (NIM-949).
func validateTaskSettings(present map[string]*ast.MappingValueNode, pathPrefix string) []diag.Diagnostic {
	kv, ok := present["settings"]
	if !ok {
		return nil
	}
	path := pathPrefix + ".settings"
	tok := kv.Key.GetToken()
	refuse := func(code, why, hint string) []diag.Diagnostic {
		return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     code,
			Message:  "settings: is not allowed " + why,
			Hint:     hint,
			YAMLPath: path,
		})}
	}
	if addr, isKeeper := keeperSideTask(present); isKeeper {
		return refuse("settings_on_keeper_invalid",
			fmt.Sprintf("on a keeper-side task (%s) — it gathers no hosts at a barrier, and a keeper-side failure always ends the run", keeperSideBecause(addr)),
			"drop settings: here; a keeper-side step takes the scenario's settings and changes none of them")
	}
	if _, isAssert := present["assert"]; isAssert {
		return refuse("settings_on_assert_invalid",
			"on an assert: — it fails the render, not a host, so there is nothing for the settings to govern",
			"drop settings: here; put it on the tasks the assert guards, or on the scenario")
	}
	if _, isInclude := present["include"]; isInclude {
		return refuse("settings_on_include_invalid",
			"on an include: — each task it splices in is its own step, so the block would not mean what it means on a block:",
			"write settings: on the tasks inside the included file, or wrap the include in a block: and put settings: there")
	}
	s, out := decodeSettings(kv.Value, path, false)
	if s == nil || s.TaskTimeout == nil {
		return out
	}
	tkv, hasTimeout := present["timeout"]
	if !hasTimeout {
		return out
	}
	sn, isStr := tkv.Value.(*ast.StringNode)
	if !isStr {
		return out
	}
	if d, err := ParseDuration(sn.Value); err == nil && d != *s.TaskTimeout {
		ttok := tkv.Key.GetToken()
		out = append(out, diagAt(ttok.Position.Line, ttok.Position.Column, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSemanticValidate,
			Code:     "timeout_shadowed_by_task_timeout",
			Message:  fmt.Sprintf("timeout: %s and settings.task_timeout: %s on the same task disagree — settings.task_timeout is the one that bounds the task (docs/scenario/settings.md §2.3), though until task timeouts are read from settings the Soul still applies timeout:", sn.Value, *s.TaskTimeout),
			Hint:     "keep one spelling: timeout: is shorthand for settings.task_timeout",
			YAMLPath: pathPrefix + ".timeout",
		}))
	}
	return out
}

// reDestinyTaskSettingsPath matches a task's own `settings:` key in a destiny
// file — `$[i]`, through any nesting of blocks — and what lies under it.
var reDestinyTaskSettingsPath = regexp.MustCompile(`^\$\[\d+\](\.block\[\d+\])*\.settings(\.|$)`)

// dropScenarioSettingsDiags removes what the shared task validator said about a
// destiny task's `settings:`: it judged the key as a scenario's, so its codes
// and hints are about scenario placement, and the one true diagnostic in a
// destiny is [validateSettingsInDestiny]'s.
func dropScenarioSettingsDiags(diags []diag.Diagnostic) []diag.Diagnostic {
	out := diags[:0]
	for _, d := range diags {
		if d.Code == "timeout_shadowed_by_task_timeout" || reDestinyTaskSettingsPath.MatchString(d.YAMLPath) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// validateSettingsInDestiny refuses `settings:` in a destiny's tasks, at any depth:
// a destiny is not a scenario and has no such key — its tasks inherit the applier's
// settings (settings.md §3 rule 6).
func validateSettingsInDestiny(item ast.Node, pathPrefix string) []diag.Diagnostic {
	m, ok := item.(*ast.MappingNode)
	if !ok {
		return nil
	}
	var out []diag.Diagnostic
	for _, kv := range m.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "settings":
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "settings" — a destiny has no settings:`,
				Hint:     "a destiny's tasks take the settings of the scenario task that applies it; bound one task with its own timeout:",
				YAMLPath: pathPrefix + ".settings",
			}))
		case "block":
			if seq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
				for i, child := range seq.Values {
					out = append(out, validateSettingsInDestiny(child, fmt.Sprintf("%s.block[%d]", pathPrefix, i))...)
				}
			}
		}
	}
	return out
}
