package oracle

import (
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// WhereEvaluator evaluates a where-CEL predicate of a Decree over the payload of a Portent event.
// The activation context is a single root `event` (form
// `{"data": <legacy-payload>, "file_changed": {...}, "service_down": {...}, ...}`),
// so a Decree author writes predicates like:
//   - `event.data.severity == "critical"`            (legacy via Struct);
//   - `event.file_changed.path.startsWith("/etc/")` (typed-field-access, V5-1).
//
// Backward-compat (ADR-030 amendment 2026-05-26): while Soul sends BOTH branches
// (data + typed), both where-CEL styles work; after S5-final hard-cut `data`
// will disappear, only typed-access will remain. Snapshot of payload types (file_changed /
// service_down / port_closed / disk_full / process_absent / http_unhealthy /
// custom) — mirror of built-in core-beacon + branch for plugin-beacon (V5-2).
//
// A separate minimal CEL-env (one declared `event`) is held locally in
// oracle, NOT in shared/cel: those env-modes (contextVars / flowControlVars /
// migrationVars) declare fixed roots scenario/destiny/migration — none have `event`,
// and extending their set would break the render-contract (cross-package).
// where-CEL Decree is an isolated predicate over untrusted payload; it has a
// sandbox-env without vault()/now()/other context-roots (default-deny + subject already
// filtered by host).
//
// Type-mismatch (CEL expects file_changed, gets service_down) — fail-safe
// no-match: with typed Activation, a missing branch expands to no-such-key,
// cel-go returns runtime-error, Eval converts to `false` (default-deny).
//
// Compiled programs are cached by expression text: the set of Decrees
// is small, and one Portent can match multiple Decrees with the same where.
type WhereEvaluator struct {
	env   *cel.Env
	mu    sync.RWMutex
	cache map[string]cel.Program
}

// NewWhereEvaluator assembles a sandbox-CEL environment for where-predicates of a Decree.
// A single root `event` is declared (DynType — all payload branches expand
// dynamically, without proto-introspection in env: typed-messages arrive as
// map[string]any in Activation, and cel-go resolves selectors field-by-field).
// Errors are only possible on incompatible cel-go configuration (programmatic,
// not user-driven).
func NewWhereEvaluator() (*WhereEvaluator, error) {
	env, err := cel.NewEnv(
		cel.StdLib(),
		cel.Variable("event", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("oracle: создание where-CEL окружения: %w", err)
	}
	return &WhereEvaluator{env: env, cache: make(map[string]cel.Program)}, nil
}

// Eval evaluates the predicate expr over a legacy data-branch (PortentEvent.data,
// Struct→map, without typed payload). Preserved for backward-compat with handlers
// that collect only data-map. Return values:
//   - (true, nil)  — predicate is true (match);
//   - (false, nil) — predicate is false or returns non-bool result (no match;
//     default-deny — non-boolean result is interpreted as "did not match");
//   - (false, err) — expression compilation error (malformed where_cel in Decree).
//
// Evaluation errors (e.g., accessing a missing key with no-such-key
// semantics in cel) are NOT function errors: cel-go returns error on runtime problems,
// we interpret them as no-match (false) — untrusted payload should not trigger
// action due to accidental shape matching. Only compile errors
// (syntactically invalid Decree) are raised for diagnostics.
func (w *WhereEvaluator) Eval(expr string, data map[string]any) (bool, error) {
	return w.evalActivation(expr, map[string]any{"event": map[string]any{"data": data}})
}

// EvalEvent evaluates the predicate expr over a full Portent-event, including typed
// payload (V5-1, ADR-030 amendment 2026-05-26). Activation form is
// `event = {"data": <legacy-map>, "<typed-branch>": {field: value, ...}}` —
// both where-CEL styles work simultaneously in the hand-off period. evt==nil → false
// (default-deny on missing payload). Return semantics are the same as Eval.
func (w *WhereEvaluator) EvalEvent(expr string, evt *keeperv1.PortentEvent) (bool, error) {
	if evt == nil {
		// Without event, there is nowhere to evaluate the predicate — default-deny.
		_, err := w.program(expr)
		if err != nil {
			return false, err
		}
		return false, nil
	}
	return w.evalActivation(expr, map[string]any{"event": buildEventActivation(evt)})
}

func (w *WhereEvaluator) evalActivation(expr string, activation map[string]any) (bool, error) {
	prg, err := w.program(expr)
	if err != nil {
		return false, err
	}
	out, _, evalErr := prg.Eval(activation)
	if evalErr != nil {
		// Runtime error (no-such-key, etc.) → no-match (default-deny).
		return false, nil
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, nil
	}
	return b, nil
}

// buildEventActivation assembles the activation map `event` from PortentEvent: legacy
// data-branch + typed payload-branch (by oneof variant). Multiple branches
// simultaneously are not allowed (oneof guarantees exactly one), but both forms
// (event.data.* and event.<typed_branch>.*) are available in one activation.
func buildEventActivation(evt *keeperv1.PortentEvent) map[string]any {
	act := make(map[string]any, 2)
	if d := evt.GetData(); d != nil {
		act["data"] = d.AsMap()
	} else {
		// CEL no-such-key on event.data.* → runtime error → no-match (default-deny).
		// Empty map here is more explicit than missing key: predicates `has(event.data.x)` and
		// `event.data.x == "y"` both behave the same — no match.
		act["data"] = map[string]any{}
	}
	switch p := evt.GetPayload().(type) {
	case *keeperv1.PortentEvent_FileChanged:
		act["file_changed"] = map[string]any{
			"path":   p.FileChanged.GetPath(),
			"sha256": p.FileChanged.GetSha256(),
		}
	case *keeperv1.PortentEvent_ServiceDown:
		act["service_down"] = map[string]any{
			"service":     p.ServiceDown.GetService(),
			"active":      p.ServiceDown.GetActive(),
			"init_system": p.ServiceDown.GetInitSystem(),
		}
	case *keeperv1.PortentEvent_PortClosed:
		act["port_closed"] = map[string]any{
			"host": p.PortClosed.GetHost(),
			"port": int64(p.PortClosed.GetPort()),
		}
	case *keeperv1.PortentEvent_DiskFull:
		act["disk_full"] = map[string]any{
			"path":         p.DiskFull.GetPath(),
			"used_percent": p.DiskFull.GetUsedPercent(),
			"threshold":    p.DiskFull.GetThreshold(),
		}
	case *keeperv1.PortentEvent_ProcessAbsent:
		act["process_absent"] = map[string]any{
			"pattern": p.ProcessAbsent.GetPattern(),
		}
	case *keeperv1.PortentEvent_HttpUnhealthy:
		act["http_unhealthy"] = map[string]any{
			"url":    p.HttpUnhealthy.GetUrl(),
			"status": int64(p.HttpUnhealthy.GetStatus()),
		}
	case *keeperv1.PortentEvent_Custom:
		// Plugin-beacon (V5-2): payload is an arbitrary Struct, projected as
		// map[string]any to match the style of other branches.
		if p.Custom != nil {
			act["custom"] = p.Custom.AsMap()
		} else {
			act["custom"] = map[string]any{}
		}
	case *keeperv1.PortentEvent_Inotify:
		// core.beacon.inotify (V5-3): repeated InotifyEvent is projected as
		// []map[string]any so that where-CEL can filter with predicates like
		// `event.inotify.events.exists(e, e.type == "created")`.
		ino := p.Inotify
		events := make([]any, 0, len(ino.GetEvents()))
		for _, e := range ino.GetEvents() {
			events = append(events, map[string]any{
				"type": e.GetType(),
				"file": e.GetFile(),
				"at":   e.GetAt(),
			})
		}
		act["inotify"] = map[string]any{
			"path":   ino.GetPath(),
			"events": events,
			"count":  int64(ino.GetCount()),
		}
	}
	return act
}

// CompileCheck compiles the predicate expr without evaluating it and caches
// the result. Used by the management Service ([Service.CreateDecree]) for
// fail-fast where-CEL validation on create: an invalid predicate is rejected with 422,
// not turned into a runtime surprise on the first Portent (default-deny would
// have swallowed a malformed predicate as no-match without operator diagnostics).
// Nil return means the predicate is syntactically correct.
func (w *WhereEvaluator) CompileCheck(expr string) error {
	_, err := w.program(expr)
	return err
}

func (w *WhereEvaluator) program(expr string) (cel.Program, error) {
	w.mu.RLock()
	prg, ok := w.cache[expr]
	w.mu.RUnlock()
	if ok {
		return prg, nil
	}

	ast, iss := w.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("oracle: компиляция where-CEL %q: %w", expr, iss.Err())
	}
	prg, err := w.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("oracle: сборка программы where-CEL %q: %w", expr, err)
	}

	w.mu.Lock()
	w.cache[expr] = prg
	w.mu.Unlock()
	return prg, nil
}
