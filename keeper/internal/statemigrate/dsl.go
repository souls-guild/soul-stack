package statemigrate

// Migration — a single state_schema migration step (`migrations/<NNN>_to_<MMM>.yml`),
// transforming incarnation.state from FromVersion to ToVersion. A pure
// function state_v<N> → state_v<M> ([ADR-019], [docs/migrations.md]).
type Migration struct {
	FromVersion int
	ToVersion   int
	Description string
	Transform   []Op
}

// Chain — an ordered chain of migration steps (001→002→003…). [Apply]
// runs it sequentially; the ToVersion of one step must match the
// FromVersion of the next (validated in [Apply]).
type Chain []*Migration

// Op — one operation of the transform: block. A discriminated union: exactly
// one of the fields is non-nil ([docs/migrations.md §"transform: operations"]). move —
// a historical alias for rename (shared code path applyRename, see apply.go).
type Op struct {
	Rename  *RenameOp  // rename / move: move the value from → to
	Set     *SetOp     // set: write value to path
	Delete  *DeleteOp  // delete: remove the value at path (missing — no-op)
	Foreach *ForeachOp // foreach: structural loop over a list/map values
}

// RenameOp — `rename`/`move`: { from: <path>, to: <path> }. Moves a value;
// existing to → error (explicit delete before rename).
type RenameOp struct {
	From string
	To   string
}

// SetOp — `set`: write Value to Path. Value is an arbitrary YAML value
// (map/list/scalar) with possible `${ … }` CEL interpolations in string
// leaves (recursive traversal, apply.go). An existing Path is overwritten.
type SetOp struct {
	Path  string
	Value any
}

// DeleteOp — `delete`: { path: <path> }. Non-existent path → no-op.
type DeleteOp struct {
	Path string
}

// ForeachOp — `foreach`: a structural loop. In is a CEL expression yielding a list
// or a map; over a list, As is bound to the ELEMENT, over a map — to the VALUE. Do is
// a nested transform list, executed on each iteration with As added to
// scope. Nested foreach is allowed.
type ForeachOp struct {
	In string
	As string
	Do []Op
}
