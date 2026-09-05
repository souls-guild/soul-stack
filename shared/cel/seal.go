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
//   - transitively — vars.<x>/compute.<x> whose own value is sealed: the render
//     pass runs this same detector over the RAW `vars:`/`compute:` text and hands
//     the names in below, before it walks any params;
//   - register.<x> whose payload holds a declared secret the render boundary
//     resolved out of a `vault:` reference ([ADR-0083] §6).
//
// Detection is whole-cell: one expression branch reading a secret is enough to
// taint the whole cell. Both branches of a ternary like
// `has(input.tls_cert) ? input.tls_cert : x` are visited (whole-value taint;
// mixing a literal with a secret yields a sealed result). This is an AST walk,
// not a single-ident match.
//
// SealSources — the tainted ADDRESSES of the CEL activation, for one detection
// pass. Empty → detect catches only vault().
//
// An address is what a BINDING is: a name the activation puts in scope,
// optionally one field under it. That vocabulary is complete rather than chosen,
// because [Vars.activation] is the ONE place a name enters scope — an expression
// cannot read something that is not addressed there.
//
// ★ Why this is an address set and not one map per root. It used to be exactly
// that — SecretInputs / SealedVars / SealedCompute / SealedRegisters, read by a
// switch over root names — and three hops were found unsealed in one review
// (NIM-811 `vars:` and `compute:`, NIM-822/NIM-823 a `loop:` bind). Every one was
// the same failure, not three: a binding kind the switch did not name fell
// through to `return false`. A per-root field makes a new kind of binding
// SILENTLY unsealed, which is the worst default a taint system can have. Here a
// new binding kind is representable with no change to this file at all, so the
// detector can no longer be the part that is out of date.
//
// What that does NOT remove is someone having to COMPUTE that a binding holds a
// secret; no shape derives that from the binding's existence. It moves that
// obligation to one table, [activationRoots], which is checked against the real
// activation by a test — so an unclassified root FAILS instead of defaulting to
// "not a secret".
type SealSources struct {
	// Fields — tainted `<root>.<field>` addresses, spelled by [FieldAddr]:
	// "input.password" (declared secret:true in the pass schema), "vars.pw" /
	// "compute.token" (their own expression read a secret), "register.acl" (a
	// payload carrying a DECLARED secret the render boundary resolved out of a
	// `vault:` reference, [ADR-0083] §6).
	//
	// The producers fill these from RAW text, never from a resolved value:
	// `compute:` resolves once per run but a task's `vars:` resolve per task PER
	// HOST, while the collection they feed runs once per task. Reading a resolved
	// value would make the mark host-variant and the walk it feeds host-invariant.
	Fields map[string]bool

	// Roots — bindings tainted WHOLE: the name itself is the address and
	// everything under it is secret, including a bare read of the name with no
	// field at all.
	//
	// A `loop:` variable is what this exists for and could not be served by
	// Fields: its name is author-chosen (`loop.as:`, defaulting to `item`), so it
	// can never be a fixed root, and its elements are not addressable one field
	// at a time — the taint is decided from the `items:` expression, so it covers
	// whatever shape the element turns out to have.
	Roots map[string]bool
}

// FieldAddr spells a `<root>.<field>` address. The ONE spelling of it: a
// producer and the detector drift the moment there are two, and the drift is
// silent — the set fills with addresses nothing ever looks up. Neither part can
// contain a dot (both are CEL identifiers), so the join is unambiguous.
func FieldAddr(root, field string) string { return root + "." + field }

// rootSealability — what the seal can say about one FIXED activation root.
type rootSealability int

const (
	// sealedByField — the root holds a map whose individual fields may be
	// tainted; the address is [FieldAddr](root, field).
	sealedByField rootSealability = iota
	// neverSecret — nothing under this root is a secret SOURCE, for the reason
	// recorded beside it. This is a claim, not a gap: a value here either is not
	// a credential at all, or is a `vault:` reference that the vault-origin
	// masking layer catches by CONTENT rather than by provenance.
	neverSecret
	// unsealedGap — this root CAN carry a plaintext secret and no producer marks
	// it yet. It exists so the table cannot be made to look complete by writing a
	// comfortable reason: a gap says so and cites its ticket, and the difference
	// between "we decided" and "we have not got there" stays legible.
	//
	// ★ This value was added because the first version of this table did the
	// wrong thing. `incarnation` was written down as neverSecret on a reason that
	// turned out to be half true, and a half-true reason in a completeness table
	// is worse than no table — it is a false green that a reader will trust.
	unsealedGap
)

// activationRoots classifies every FIXED name [Vars.activation] can put in
// scope. Loop binds are deliberately absent — their names are author-chosen, a
// fixed table could not list them, and they arrive through [SealSources.Roots].
//
// ★ This table is the completeness check, not documentation. TestActivationRoots
// AreClassified compares it against what activation() really builds, in both
// modes, in both directions. Adding a name to the activation without deciding
// whether it can carry a secret FAILS there — which is the one thing that was
// missing when the loop bind stayed unsealed through three passes over this file.
var activationRoots = map[string]rootSealability{
	"input":    sealedByField,
	"vars":     sealedByField,
	"compute":  sealedByField,
	"register": sealedByField,

	// ⚠ OPEN, NIM-826 — and stated here rather than assumed, because the first
	// version of this row got it wrong in the safe-looking direction.
	//
	// `incarnation.<id|service|service_version|host_count>` is registry metadata
	// and carries nothing. `incarnation.state` is the problem, and the two state
	// markers are NOT the same ([ADR-0083] §1): `type: secret` means the value
	// lives in Vault and never in state, so reading it takes home a `vault:` ref
	// the vault-origin masking layer catches by content — but `secret: true` on a
	// state property means the value LIVES IN STATE and is masked on the way out,
	// so `${ incarnation.state.<field> }` reads plaintext and this seal does not
	// mark it. Both markers stay; neither replaces the other.
	//
	// Closing it needs the field addressable — `selectBaseField` sees only the
	// pair ("incarnation", "state"), one level too shallow — and the state schema
	// carried into render. See NIM-826.
	"incarnation": unsealedGap,
	// Host facts the agent collected (os/kernel/cpu/memory/network/sid/covens/
	// role/choirs/traits). Operator-set traits are authored plaintext — the same
	// class as a plaintext literal typed into a scenario, which is a stated bound
	// of the seal rather than a provenance it could derive.
	"soulprint": neverSecret,
	// migration mode only ([NewMigration]). Not a claim that state holds no
	// plaintext secret — NIM-826 says it can — but that no SealSources is ever
	// built in this mode: a migration is a pure function of state, its output is
	// state rather than params, and there is no params walk here to feed.
	"state": neverSecret,
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
		case ast.IdentKind:
			// A WHOLE-binding read: `${ item }` with no field, which reaches no
			// Select node and so was visible to nothing here before NIM-822.
			// Only a [SealSources.Roots] binding can be secret this way — a
			// fixed root is tainted per field, and its bare read is judged by
			// [activationRoots] instead (see readsSecretAddr).
			if sources.Roots[n.AsIdent()] {
				found = true
			}
		case ast.SelectKind:
			if base, field, ok := selectBaseField(n); ok && readsSecretAddr(base, field, sources) {
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
//
// `register.hosts.<name>` (NIM-711) is the one shape that must NOT fall through
// to its sub-nodes: the level below is `register.hosts`, whose field is `hosts`
// — a name reserved at parse ([scenario_task.go], register_name_reserved) and so
// never a sealed `register.<name>` address. Left alone, a sealed register read
// across hosts would be seen as two unsealed hops and the cell holding every
// host's secret would go unmasked. The two hops are flattened here so the taint
// is decided on the register name the author actually read.
func selectBaseField(n ast.Expr) (base, field string, ok bool) {
	s := n.AsSelect()
	if s.IsTestOnly() {
		return "", "", false
	}
	op := s.Operand()
	if op.Kind() != ast.IdentKind {
		if isRegisterHosts(op) {
			return "register", s.FieldName(), true
		}
		return "", "", false
	}
	return op.AsIdent(), s.FieldName(), true
}

// readsSecretAddr reports whether `base.field` addresses a taint: either the
// whole binding `base` is tainted, or the address `base.field` is.
//
// ★ No root is named here, and that is the point rather than an economy. The
// predecessor switched on `input`/`vars`/`compute`/`register` and answered false
// for everything else, so each new kind of binding arrived unsealed and silent.
// Whether a root can be tainted at all is declared in [activationRoots] and
// checked against the real activation by a test; what IS tainted right now is
// whatever the producer put in the set.
func readsSecretAddr(base, field string, sources SealSources) bool {
	return sources.Roots[base] || sources.Fields[FieldAddr(base, field)]
}
