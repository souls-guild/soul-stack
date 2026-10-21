package render

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"google.golang.org/protobuf/types/known/structpb"
)

// isNotExist recognizes "file not found" through the securejoin reader's
// wrapping (it wraps os.ReadFile via %w). Only this condition triggers the
// scenario-local→service-level fallback; other I/O errors are never masked.
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// moduleFileRendered — the step address for which Keeper delivers literal
// template content (A1, ADR-012(d)). Only for it does params.template (path)
// get replaced with params.template_content (content).
const moduleFileRendered = "core.file.rendered"

// paramTemplate / paramTemplateContent — params keys of the core.file.rendered
// step. template — path to a `.tmpl` (author input in scenario/destiny);
// template_content — literal content that Keeper injects after reading the
// file, which Soul then renders itself (rendered.go reads template_content).
const (
	paramTemplate        = "template"
	paramTemplateContent = "template_content"
)

// paramRenderContext — the params key under which Keeper delivers the
// assembled per-host root of the core.file.rendered text/template context:
// {vars, self, role} (templating.md §3.2). Soul reads it and passes it
// as the ROOT to text/template (rendered.go). No proto changes needed (A1,
// ADR-012(d)) — it rides inside RenderedTask.params.
const paramRenderContext = "render_context"

// paramVars — the author-facing params key of the core.file.rendered step:
// values the author surfaces for the template (templating.md §6,
// `params.vars`). Keeper CEL-renders it, moves it under render_context.vars
// (the §3.2 root) and DELETES it from params — Soul reads the root only from
// render_context.
const paramVars = "vars"

// TemplateReader reads the literal content of a `.tmpl` file from an artifact
// snapshot by relative path (as recorded in `params.template`, e.g.
// `templates/redis.conf.tmpl`). Implementations must guard against traversal
// (`../`/absolute path/symlink escape) and resolve two-tier
// scenario-local→service-level where ADR-009 requires it.
//
// Symmetric to [DestinyResolver]: a narrow per-run interface, with a
// snapshot-backed prod implementation (see [SnapshotTemplateReader]) and a
// hermetic Trial L0 fixture-backed reader over a local case tree.
type TemplateReader interface {
	Read(relPath string) ([]byte, error)
}

// snapshotReadFunc reads a file from a snapshot by relative path with
// securejoin protection. A targeted injection point for
// artifact.readSnapshotFile (unexported) that avoids pulling a render→artifact
// dependency: the caller (run.go) passes a closure.
type snapshotReadFunc func(relPath string) ([]byte, error)

// SnapshotTemplateReader — prod implementation of [TemplateReader] over a
// materialized snapshot. Two-tier resolve (ADR-009, orchestration.md §6):
// scenario-local `scenario/<name>/<relPath>` first, then service-level
// `<relPath>` — the nearer tier fully shadows the farther one (no merge).
//
// scenarioPrefix sets the scenario-local directory (`scenario/<name>`); empty
// means a single-tier resolve (destiny pass: `.tmpl` files sit right under the
// destiny snapshot root, destiny has no scenario-local tier). read is a
// securejoin-backed read from the snapshot (traversal guarded at every tier).
type SnapshotTemplateReader struct {
	read           snapshotReadFunc
	scenarioPrefix string
}

// NewSnapshotTemplateReader builds a reader over a snapshot. read is required
// (securejoin-backed read from the specific snapshot). scenarioPrefix is the
// scenario-local tier directory (`scenario/<name>`); "" → single-tier resolve.
func NewSnapshotTemplateReader(read snapshotReadFunc, scenarioPrefix string) *SnapshotTemplateReader {
	return &SnapshotTemplateReader{read: read, scenarioPrefix: scenarioPrefix}
}

// Read resolves relPath in two tiers. At each tier the path is read via
// securejoin-backed read (traversal clamped by the snapshot reader). Missing
// file at the scenario-local tier → fallback to service-level; missing at both
// → a not-found error. An I/O error (not "file missing") at any tier is never
// masked.
func (r *SnapshotTemplateReader) Read(relPath string) ([]byte, error) {
	if r.scenarioPrefix != "" {
		local := r.scenarioPrefix + "/" + relPath
		data, err := r.read(local)
		if err == nil {
			return data, nil
		}
		if !isNotExist(err) {
			return nil, fmt.Errorf("render: reading template %q (scenario-local %q): %w", relPath, local, err)
		}
	}
	data, err := r.read(relPath)
	if err != nil {
		return nil, fmt.Errorf("render: reading template %q (service-level): %w", relPath, err)
	}
	return data, nil
}

// templateFile is what a core.file.rendered step delivers, read once before the
// per-host loop ([Pipeline.resolveTemplateUsesInput]).
type templateFile struct {
	// path is the `template:` path the content was read from, resolved on the
	// run-level view; "" for inline template_content, which names no file.
	path    string
	content string
	// readsInput — the template reads the root `.input.*` (Variant B, ADR-010 §3.2).
	readsInput bool
}

// injectTemplateContent, for a core.file.rendered step, replaces the
// `template` path with literal `template_content` in already CEL-rendered
// params (A1, ADR-012(d)). text/template is NOT executed here — rendering
// happens on Soul (rendered.go).
//
// The content is tf's and nothing is read here. params.template is the cell as
// renderParams resolved it on the task's view, which also holds the task's own
// `vars:`; the file is chosen on the run-level view without them (ADR-0082
// amendment 2026-10-15, point 5), so reading that cell again could deliver another
// file — and an empty file is content like any other.
//
// Other modules pass through untouched (rt.Params left alone, "" RawTemplate).
// After injection the template key is removed — Soul doesn't need the path.
func injectTemplateContent(rt *RenderedTask, tf templateFile) error {
	if rt.Module != moduleFileRendered {
		return nil
	}
	if rt.Params == nil {
		return fmt.Errorf("render: task %q (core.file.rendered): params empty - missing key %q", rt.Name, paramTemplate)
	}
	fields := rt.Params.GetFields()
	if _, ok := fields[paramTemplate]; !ok {
		// template_content is already set directly (inline template, no file) — skip.
		if _, has := fields[paramTemplateContent]; has {
			rt.RawTemplate = tf.content
			return nil
		}
		return fmt.Errorf("render: task %q (core.file.rendered): neither %q (path) nor %q (inline content)", rt.Name, paramTemplate, paramTemplateContent)
	}
	if tf.path == "" {
		return fmt.Errorf("render: task %q (core.file.rendered): %q was not read before the render", rt.Name, paramTemplate)
	}

	setTemplateContent(fields, tf.content)
	// ★ And on EVERY per-host struct (NIM-908). The substitution mutates a struct in
	// place, and rt.Params is only the first host's; a task with per-host params
	// ([RenderedTask.ParamsBySID]) would otherwise ship `template:` — a path Soul
	// cannot read — to every host but that one, and `core.file.rendered` fails its
	// param validation there. The template PATH is host-invariant (pilot contract,
	// resolveTemplateUsesInput reads it once before the per-host loop), so one
	// content serves them all. rt.Params may be the same pointer as one of these;
	// applying it twice is a no-op.
	for _, st := range rt.ParamsBySID {
		if st != nil {
			setTemplateContent(st.GetFields(), tf.content)
		}
	}
	rt.RawTemplate = tf.content
	return nil
}

// setTemplateContent replaces the `template:` path with the literal
// `template_content:` Keeper read for it, in one params struct.
func setTemplateContent(fields map[string]*structpb.Value, content string) {
	fields[paramTemplateContent] = structpb.NewStringValue(content)
	delete(fields, paramTemplate)
}

// templateReadError is a failed read of a `template:` path an expression built. The
// reader's own error names the RESOLVED path at every level it wraps, and that path
// can be built from a secret; being no CEL error, it is never judged by withhold
// (failure.go). So it quotes the path as the author wrote it, over a cause naming no
// path.
type templateReadError struct {
	raw   string
	cause error
}

func (e *templateReadError) Error() string {
	return fmt.Sprintf("render: reading template %q: %v", e.raw, e.cause)
}

func (e *templateReadError) Unwrap() error { return e.cause }

// pathFreeCause is the part of a read error that names no path: the errno under an
// [fs.PathError], else only whether the file was missing.
func pathFreeCause(err error) error {
	var pe *fs.PathError
	switch {
	case errors.As(err, &pe):
		return pe.Err
	case isNotExist(err):
		return fs.ErrNotExist
	default:
		return errors.New("read failed")
	}
}
