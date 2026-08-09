package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Gate (b) of every trait write, in one place (NIM-587).
//
// A trait pair is not a description, it is a GRANT. `trait.<key>` is a live scope
// dimension on the read side for both objects that carry traits — hosts
// (`soulScopeColumns.Traits`) and incarnations (`incScopeColumns.Traits`) — so
// stamping `tier=gold` on an object hands every `trait.tier=gold` role sight of
// it, and running rights with it. An operator who does not hold that pair must
// therefore not be able to attach it, or the write is a way to widen someone
// else's purview (and, through a role they also hold, their own).
//
// Gate (a) — the target objects lie inside the operator's scope — is a different
// question and is asked elsewhere (route middleware for the typed REST surfaces,
// the OR-Check in the MCP tools, the WHERE predicate for the bulk soul write).
// Gate (a) says "you may write to THIS object"; gate (b) says "you may write THIS
// LABEL". Holding the object does not imply holding the label: that inference was
// written down as fact in the MCP incarnation.traits-set header and was the hole
// NIM-587 opened on.
//
// The single home is the point of the ticket. Before it, this gate existed as two
// copies (REST souls, MCP souls) and was absent from all four incarnation write
// paths — the exact shape of NIM-401/NIM-529, where one surface is fixed and the
// rest keep the defect. Every surface now calls THIS function, so a fixed copy and
// a forgotten copy are no longer possible; the agreement guard
// (mcp.TestIntegration_TraitWriteGate_AllSurfacesAgree) drives all four write
// surfaces against one Postgres-checked expectation and names the odd one out if
// a surface stops calling this.
//
// Placement mirrors [ScreenIncarnationCreateScope]: package handlers owns the
// screening functions that REST and MCP share, and mcp already imports handlers
// for exactly that.

// ErrTraitScopeUnavailable — the gate could not be EVALUATED: the resolver does
// not carry the trait projection, or Postgres could not canonicalize the payload.
// Fail-closed, and deliberately distinct from a refusal: the operator did nothing
// wrong, the deployment cannot answer the question, and the surfaces render it as
// a 500 rather than a 422. Waving the write through instead is precisely what the
// gate exists to prevent.
var ErrTraitScopeUnavailable = errors.New("handlers: trait-scope gate unavailable")

// TraitPairOutOfScopeError — the refusal: one concrete pair the caller may not
// attach. It carries the pair rather than a formatted string so the surfaces
// render ONE message text from ONE place; a per-surface string is how the two
// soul copies would have drifted apart in wording even while agreeing in verdict.
type TraitPairOutOfScopeError struct {
	Key   string
	Value string
}

func (e *TraitPairOutOfScopeError) Error() string {
	return "trait " + e.Key + "=" + e.Value + " is outside operator trait-scope"
}

// ScreenTraitPairsInScope is gate (b): every pair in `traits` must lie inside the
// caller's own trait-scope for (resource, action) — the pair-level mirror of "the
// coven label you assign must be one you hold".
//
// A list value is checked ELEMENT-WISE. Each element is a pair in its own right on
// the read side, so one out-of-scope element grants exactly as much as a whole
// out-of-scope key would (NIM-522: a scope value names the whole stored value, and
// for an array that is each scalar element).
//
// The pairs are read off the payload AS POSTGRES WILL STORE IT
// ([soul.CanonicalTraitPayload] → [rbac.TraitPairTexts]), never off the decoded Go
// map: the read-side dimension this gate measures against is answered by `->>`
// over the stored jsonb, and only jsonb knows the text it will yield. Rendering
// the value in Go printed `1e+06` for a plain `1000000` and refused an operator
// the pair it plainly holds (NIM-529).
//
// An unrestricted caller passes without touching the database. Everyone else pays
// one `SELECT $1::jsonb`, which reads no row and opens no transaction — so the
// gate is safe to run on a dry-run path, where it MUST still run or dry-run would
// report a `matched` for a write that cannot happen.
//
// resolver nil, or a resolver without the trait projection, or a failed
// canonicalization → [ErrTraitScopeUnavailable]. Fail-closed on a missing
// dependency, the same choice [ScreenIncarnationCreateScope] makes.
//
// Errors are returned, not rendered: REST answers 422/500 through problem+json and
// MCP through its own codes, but both take the refusal TEXT from
// [TraitPairOutOfScopeError.Error], so the two surfaces cannot disagree about what
// they told the operator.
func ScreenTraitPairsInScope(
	ctx context.Context,
	db soul.ExecQueryRower,
	resolver PurviewResolver,
	aid, resource, action string,
	traits map[string]any,
) error {
	if resolver == nil {
		return fmt.Errorf("%w: no purview resolver", ErrTraitScopeUnavailable)
	}
	scoper, ok := resolver.(traitScoper)
	if !ok {
		return fmt.Errorf("%w: resolver lacks TraitScope", ErrTraitScopeUnavailable)
	}
	allowed, unrestricted := scoper.TraitScope(aid, resource, action)
	if unrestricted {
		return nil
	}
	if db == nil {
		return fmt.Errorf("%w: no database handle for canonicalization", ErrTraitScopeUnavailable)
	}
	raw, err := soul.CanonicalTraitPayload(ctx, db, traits)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrTraitScopeUnavailable, err)
	}
	pairs := rbac.TraitPairTexts(raw)
	for _, key := range sortedMapKeys(pairs) {
		for _, elem := range pairs[key] {
			if !slices.Contains(allowed[key], elem) {
				return &TraitPairOutOfScopeError{Key: key, Value: elem}
			}
		}
	}
	return nil
}

// sortedMapKeys — deterministic iteration so the rejected pair reported to the
// operator is stable across calls, and identical on every surface.
func sortedMapKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
