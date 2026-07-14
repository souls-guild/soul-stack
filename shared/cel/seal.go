package cel

import (
	"github.com/google/cel-go/common/ast"
)

// seal / sealed-paths ([ADR-010] §7.4): render-time provenance/taint. A params
// cell is marked sealed when its CEL expression READS a secret source:
//
//   - input.<name>, where <name> is declared secret:true in the run's active
//     input schema (scenario-input on scenario, destiny-input on destiny);
//   - vault(...) — a Vault KV read;
//   - transitively — vars.<x>/compute.<x> whose own value is sealed (their
//     expressions were already detected on their resolve phase).
//
// Detection is whole-cell: one expression branch reading a secret is enough to
// taint the whole cell. Both branches of a ternary like
// `has(input.tls_cert) ? input.tls_cert : x` are visited (whole-value taint;
// mixing a literal with a secret yields a sealed result). This is an AST walk,
// not a single-ident match.
//
// SealSources — what counts as a secret source while walking one expression.
// Empty secret-input with empty var/compute → detect catches only vault().
type SealSources struct {
	// SecretInputs holds names of input params declared secret:true in the
	// run's active schema. `input.<name>` for a name in the set → cell sealed.
	SecretInputs map[string]bool

	// SealedVars / SealedCompute hold names of vars.*/compute.* whose VALUE is
	// already sealed (transitivity): their own expression read a secret on the
	// vars/compute resolve phase. `vars.<x>`/`compute.<x>` for x in the set →
	// cell sealed.
	SealedVars    map[string]bool
	SealedCompute map[string]bool
}

// vaultMacroName / vaultExpandedName — the names under which vault() appears in
// the AST. Before macro-expansion (parseNoMacro) it is a plain call `vault`;
// after env.Parse it is `__vault_read` (see expandVaultMacro in vault.go). The
// detector parses without macros (parseNoMacro), so it catches `vault`;
// `__vault_read` is kept for an already-expanded tree (the internal identifier
// is forbidden to authors, but the walk is cheap).
const (
	vaultMacroName    = "vault"
	vaultExpandedName = vaultFuncName // "__vault_read"
)

// DetectSealed reports whether the interpolated string raw (with `${ … }`
// blocks) reads any secret source per sources — i.e. whether the cell holding
// this value must be marked sealed. Each `${expr}` block is parsed without
// macros (parseNoMacro) and walked with PostOrderVisit; the first secret
// reference → true (whole-cell taint). Strings without `${ … }` (plain literal)
// and blocks with no secret references → false. A block parse error (broken CEL
// is caught separately by the eval phase) → the block is skipped, not sealed:
// the detector does not duplicate validation, it only marks taint on valid
// expressions.
func (e *Engine) DetectSealed(raw string, sources SealSources) bool {
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		return false
	}
	for _, s := range segs {
		if !s.expr {
			continue
		}
		if e.exprReadsSecret(s.text, sources) {
			return true
		}
	}
	return false
}

// exprReadsSecret parses one CEL expression (without the `${ }` wrapper) without
// macros and returns true if anywhere in the tree it reads a secret source per
// sources.
func (e *Engine) exprReadsSecret(expr string, sources SealSources) bool {
	parsed, perr := e.parseNoMacro(expr)
	if perr != nil {
		return false
	}
	found := false
	ast.PostOrderVisit(parsed.Expr(), ast.NewExprVisitor(func(n ast.Expr) {
		if found {
			return
		}
		switch n.Kind() {
		case ast.CallKind:
			if isVaultCall(n) {
				found = true
			}
		case ast.SelectKind:
			if base, field, ok := selectBaseField(n); ok && readsSecretSelect(base, field, sources) {
				found = true
			}
		}
	}))
	return found
}

// isVaultCall reports a node of the form vault(...) (global call `vault`, before
// macro-expansion) or an already-expanded __vault_read(...). A member call
// (`x.vault()`) does not count — that is not our function.
func isVaultCall(n ast.Expr) bool {
	c := n.AsCall()
	if c.IsMemberFunction() {
		return false
	}
	name := c.FunctionName()
	return name == vaultMacroName || name == vaultExpandedName
}

// selectBaseField extracts (base-ident, field) from a Select node of the form
// `<ident>.<field>` (e.g. `input.password`). PostOrderVisit visits nested
// Selects (`params.vars.tls_cert`) level by level: the `vars.tls_cert` pair
// arrives as its own Select (operand=ident `vars`), so the top-context
// identifier (input/vars/compute) plus the next field name are detected at this
// level. Returns ok=false when the operand is not a bare ident (e.g. a call
// result): then the secret source is determined by its own sub-node in the walk.
func selectBaseField(n ast.Expr) (base, field string, ok bool) {
	s := n.AsSelect()
	if s.IsTestOnly() {
		return "", "", false
	}
	op := s.Operand()
	if op.Kind() != ast.IdentKind {
		return "", "", false
	}
	return op.AsIdent(), s.FieldName(), true
}

// readsSecretSelect reports whether the (base, field) pair addresses a secret
// source:
//   - input.<field>, field ∈ SecretInputs;
//   - vars.<field>, field ∈ SealedVars;
//   - compute.<field>, field ∈ SealedCompute.
func readsSecretSelect(base, field string, sources SealSources) bool {
	switch base {
	case "input":
		return sources.SecretInputs[field]
	case "vars":
		return sources.SealedVars[field]
	case "compute":
		return sources.SealedCompute[field]
	}
	return false
}
