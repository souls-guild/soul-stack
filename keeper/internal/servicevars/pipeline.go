package servicevars

import (
	"io/fs"
	"os"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/shared/cel"
	assembly "github.com/souls-guild/soul-stack/shared/servicevars"
)

// View is the service view of one incarnation: the service rung alone, no parent
// (ADR-0082 amendment 2026-10-15, point 7). A `vars/_stack.yaml` declares the
// layers when present, else every layer file in lexical order. Nothing is
// evaluated but the stack's selectors; a definition is evaluated when the view is
// asked for it.
func (r *Resolver) View(in ResolveInput) (*cel.VarView, error) {
	layers, err := assembly.Layers(snapshotFS(in.ServiceDir), in.Incarnation, r.logger)
	if err != nil {
		return nil, err
	}
	return cel.NewVarView(nil, layers...), nil
}

// LexicalView is the service view from the FILES ALONE, `_stack.yaml` ignored
// ([assembly.LexicalLayers] says why), against an empty incarnation. For a reader
// with no incarnation in hand: the directive catalog.
func (r *Resolver) LexicalView(serviceDir string) (*cel.VarView, error) {
	layers, err := assembly.LexicalLayers(snapshotFS(serviceDir), IncarnationContext{}, r.logger)
	if err != nil {
		return nil, err
	}
	return cel.NewVarView(nil, layers...), nil
}

// ViewOf is a single-layer service view over definitions the caller already
// holds: an L0 case's `fixtures.vars`, which stands for the whole rung, or a test.
func ViewOf(defs map[string]any, inc IncarnationContext) (*cel.VarView, error) {
	layer, err := assembly.NewLayer("service vars", defs, inc)
	if err != nil {
		return nil, err
	}
	return cel.NewVarView(nil, layer), nil
}

// snapshotFS reads a service snapshot through securejoin: every path, symlinks
// included, resolves inside the root, so a link pointing out of the snapshot is
// re-rooted under it instead of followed — and the file behind it is not read.
type snapshotFS string

func (root snapshotFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	full, err := securejoin.SecureJoin(string(root), name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return os.Open(full)
}
