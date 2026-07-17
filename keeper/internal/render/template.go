package render

import (
	"errors"
	"fmt"
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
// {vars, self, role, essence} (templating.md §3.2). Soul reads it and passes it
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

// injectTemplateContent, for a core.file.rendered step, replaces the
// `template` path with literal `template_content` in already CEL-rendered
// params (A1, ADR-012(d)). text/template is NOT executed here — rendering
// happens on Soul (rendered.go).
//
// preloaded — content already read by the caller (renderTaskIter →
// resolveTemplateUsesInput) upon detecting a `.input` reference, so the file
// isn't read twice. Non-empty preloaded → used as-is (reader untouched). Empty
// preloaded (inline template, or a non-rendered module) → falls back to
// reading via reader from params.template, as before.
//
// Other modules pass through untouched (rt.Params left alone, "" RawTemplate).
// reader=nil for core.file.rendered with params.template and no preloaded is a
// handoff error (Keeper isn't configured to deliver content; this exact gap
// used to block the golden path in prod).
//
// params.template must be a path string (after the CEL phase; non-string is an
// error). After injection the template key is removed — Soul doesn't need the path.
func injectTemplateContent(rt *RenderedTask, reader TemplateReader, preloaded string) error {
	if rt.Module != moduleFileRendered {
		return nil
	}
	if rt.Params == nil {
		return fmt.Errorf("render: task %q (core.file.rendered): params empty - missing key %q", rt.Name, paramTemplate)
	}
	fields := rt.Params.GetFields()
	tv, ok := fields[paramTemplate]
	if !ok {
		// template_content is already set directly (inline template, no file) — skip.
		if _, has := fields[paramTemplateContent]; has {
			rt.RawTemplate = preloaded
			return nil
		}
		return fmt.Errorf("render: task %q (core.file.rendered): neither %q (path) nor %q (inline content)", rt.Name, paramTemplate, paramTemplateContent)
	}
	rel := tv.GetStringValue()
	if _, isStr := tv.GetKind().(*structpb.Value_StringValue); !isStr || rel == "" {
		return fmt.Errorf("render: task %q (core.file.rendered): %q must be a non-empty path string, got %v", rt.Name, paramTemplate, tv.AsInterface())
	}

	content := preloaded
	if content == "" {
		if reader == nil {
			return fmt.Errorf("render: task %q (core.file.rendered): TemplateReader not configured - Keeper cannot deliver template content %q (RenderInput.Templates=nil)", rt.Name, rel)
		}
		data, err := reader.Read(rel)
		if err != nil {
			return fmt.Errorf("render: task %q (core.file.rendered): %w", rt.Name, err)
		}
		content = string(data)
	}

	fields[paramTemplateContent] = structpb.NewStringValue(content)
	delete(fields, paramTemplate)
	rt.RawTemplate = content
	return nil
}
