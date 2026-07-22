package statemigrate

import "fmt"

// ParseError — a migration file parse error (`NNN_to_MMM.yml`): YAML
// syntax, missing/conflicting operation discriminator, invalid field shape.
// Code — a snake_case class identifier (stable for tests/diagnostics),
// symmetric with the config layer's error codes ([destiny_tasks.go]).
type ParseError struct {
	Code string // migration_* (see constants below)
	Msg  string
}

func (e *ParseError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Msg) }

// ParseError codes. All prefixed with migration_ (the migration DSL domain).
const (
	CodeYAMLParse        = "migration_yaml_parse_error"   // invalid YAML
	CodeEmptyDocument    = "migration_empty_document"     // empty file
	CodeVersionMissing   = "migration_version_missing"    // missing from_version/to_version
	CodeVersionInvalid   = "migration_version_invalid"    // to_version != from_version+1, etc.
	CodeOpDiscriminator  = "migration_op_discriminator"   // not exactly one operation key
	CodeOpFieldMissing   = "migration_op_field_missing"   // operation is missing a required field
	CodeForeachMissingAs = "migration_foreach_missing_as" // foreach without as:
)

// EvalError — an error applying an operation to state: an untraversable path,
// a rename-to conflict, an unresolvable CEL interpolation, a non-iterable foreach.
// Class — a snake_case class; Path — the operation's address (for diagnostics); Err —
// the wrapped root cause (a CEL error, etc.), may be nil.
type EvalError struct {
	Class string
	Path  string
	Msg   string
	Err   error
}

func (e *EvalError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s [%s]: %s", e.Class, e.Path, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Class, e.Msg)
}

func (e *EvalError) Unwrap() error { return e.Err }

// EvalError classes.
const (
	ClassRenameToExists = "migration_rename_to_exists" // rename/move into an already-existing to
	ClassPathTraverse   = "migration_path_traverse"    // intermediate path segment is not a map
	ClassPathSegment    = "migration_path_segment"     // invalid/unresolvable path segment
	ClassForeachType    = "migration_foreach_type"     // in: yielded neither a list nor a map
	ClassCELInterp      = "migration_cel_interp"       // ${ … } resolution error
	ClassChainVersion   = "migration_chain_version"    // version gap in the chain
)
