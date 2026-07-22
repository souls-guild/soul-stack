package augur

// AugurRequest authorization resolve (augur.md §6) — the broker's enforcement
// point. A pure function of (sid, omen_name, query) + registry readers:
// decides whether the request is allowed, and which Rite allowed it. The
// actual value fetch (vault-broker) and sending the reply are separate layers
// (broker.go / grpc-handler).
//
// Slice C (MVP-1) — source_type vault / prometheus / elk, delegate=false.
// Delegation (delegate=true, MVP-2) yields Denied (see [Resolve]).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// Decision — the outcome of [Resolve]. Allowed=false → access denied, Reason
// carries a human-readable cause (for AugurReply.error and the audit
// payload). When Allowed=true, Omen and Query are filled in — Query is the
// normalized (via ParseRef) logical path the broker reads from Vault.
//
// Query when Allowed=true is the canonical value the broker fetches by:
//   - vault: the NORMALIZED logical-path (mount/rel), not the original
//     raw query — enforcement and fetch must work off the same canonical
//     value (otherwise secret//x would bypass the allow-list);
//   - prometheus: the promQL as-is (exact-match against Rite.allow.queries);
//   - elk: the index as-is (exact-match against Rite.allow.indices).
type Decision struct {
	Allowed bool
	Reason  string
	Omen    *Omen
	Query   string
}

// denied — constructor for a deny decision (default-deny). Reason is passed
// through to AugurReply.error and the `augur.access_denied` audit event; it
// carries no secret values.
func denied(reason string) *Decision { return &Decision{Allowed: false, Reason: reason} }

// OmenReader / RiteReader / CovenReader — narrow registry surfaces needed by
// resolve. Narrowing (instead of passing a *pgxpool.Pool) isolates
// enforcement from CRUD and allows a fake in unit tests without spinning up
// PG. The real implementations are closures over [SelectOmenByName] /
// [SelectRitesBySubject] / the soul registry (see the grpc-handler wire-up).
type OmenReader interface {
	OmenByName(ctx context.Context, name string) (*Omen, error)
}

type RiteReader interface {
	RitesBySubject(ctx context.Context, sid string, covens []string) ([]*Rite, error)
}

// CovenReader resolves covens by SID from the AUTHORITATIVE registry
// (souls.coven[]), NOT from the request payload (augur.md §6.2: covens are
// taken from the registry, not from the AugurRequest). Returns
// [ErrSubjectUnknown] when the Soul isn't in the registry.
type CovenReader interface {
	CovensBySID(ctx context.Context, sid string) ([]string, error)
}

// ErrSubjectUnknown — the SID wasn't found in the souls registry. Resolve
// treats it as Denied (no authoritative subject → no grant), not as ERROR:
// for the broker this is a normal denial, not a Keeper failure.
var ErrSubjectUnknown = errors.New("augur: sid not found in souls registry")

// Resolve — the authorization resolve (augur.md §6). default-deny: any
// failed check → Decision{Allowed:false} + Reason, without reading the
// secret.
//
// Steps:
//  1. The Omen exists (OmenByName). No → denied.
//  2. source_type ∈ {vault, prometheus, elk}. unknown → denied.
//  3. The delegate branch of Slice C — only delegate=false (broker).
//     delegate=true (MVP-2) → denied (minting/issuing a cred is a separate
//     slice).
//  4. covens by SID from the registry (CovensBySID) — the authoritative
//     source.
//  5. A Rite is found (RitesBySubject) for this Omen. No → denied.
//  6. query ∈ Rite.allow, an EXACT match by source_type shape:
//     vault      — paths, AFTER normalizing the vault path (otherwise
//     secret//x would bypass the allow-list);
//     prometheus — queries, exact-match of the raw promQL;
//     elk        — indices, exact-match of the raw index.
//     No → denied.
//
// Returns an error (not a Decision) only on a reader infrastructure failure
// (PG unavailable, etc.): the caller then returns AugurReply{status:ERROR}.
// A semantic authorization denial is Decision{Allowed:false}, error=nil.
//
// sid comes from the mTLS peer cert on the caller's side (grpc-handler), not
// from the AugurRequest — what arrives here is already authoritative.
func Resolve(
	ctx context.Context,
	omens OmenReader,
	rites RiteReader,
	covens CovenReader,
	sid, omenName, query string,
) (*Decision, error) {
	omen, err := omens.OmenByName(ctx, omenName)
	if err != nil {
		if errors.Is(err, ErrOmenNotFound) {
			return denied(fmt.Sprintf("omen %q not found", omenName)), nil
		}
		return nil, fmt.Errorf("augur: resolve omen %q: %w", omenName, err)
	}

	switch omen.SourceType {
	case SourceVault, SourcePrometheus, SourceELK:
		// supported in this slice.
	default:
		return denied(fmt.Sprintf("unknown source_type %q", omen.SourceType)), nil
	}

	covenList, err := covens.CovensBySID(ctx, sid)
	if err != nil {
		if errors.Is(err, ErrSubjectUnknown) {
			return denied("subject not registered"), nil
		}
		return nil, fmt.Errorf("augur: resolve covens for sid %q: %w", sid, err)
	}

	candidates, err := rites.RitesBySubject(ctx, sid, covenList)
	if err != nil {
		return nil, fmt.Errorf("augur: resolve rites for sid %q: %w", sid, err)
	}

	// The canonical query value that both enforcement and fetch use. For
	// vault — the normalized logical-path (otherwise secret//x bypasses the
	// allow-list, see vault.normalizeLogical); for prom/elk — the query as-is
	// (exact-match of the raw promQL/index).
	wantQuery, perr := canonicalQuery(omen.SourceType, query)
	if perr != nil {
		return denied(fmt.Sprintf("invalid query for source_type %q", omen.SourceType)), nil
	}

	for _, r := range candidates {
		if r.Omen != omenName {
			continue
		}
		if r.Delegate {
			// MVP-2 — no cred/token issuance here.
			continue
		}
		allowed, aerr := riteAllows(omen.SourceType, r, wantQuery)
		if aerr != nil {
			// A broken allow-JSONB on this Rite — skip it (doesn't fail the whole
			// resolve: another Rite on the same Omen may be valid). Insert-time
			// validation (ValidateAllow) makes this rare, but the DB could have come
			// from a migration/manual edit.
			continue
		}
		if allowed {
			return &Decision{Allowed: true, Omen: omen, Query: wantQuery}, nil
		}
	}

	return denied("no rite grants this query"), nil
}

// canonicalQuery brings a raw query to its canonical form by source_type
// shape: vault — the normalized logical-path (normalizeQueryPath); prom/elk
// — the query unchanged (exact-match of the raw value). An empty prom/elk
// query is rejected (nothing to match).
func canonicalQuery(src SourceType, query string) (string, error) {
	switch src {
	case SourceVault:
		return normalizeQueryPath(query)
	case SourcePrometheus, SourceELK:
		if query == "" {
			return "", fmt.Errorf("augur: empty query")
		}
		return query, nil
	default:
		return "", fmt.Errorf("augur: unknown source_type %q", src)
	}
}

// riteAllows — EXACT-matches wantQuery against Rite.allow by source_type shape.
func riteAllows(src SourceType, r *Rite, wantQuery string) (bool, error) {
	switch src {
	case SourceVault:
		return riteAllowsVaultPath(r, wantQuery)
	case SourcePrometheus:
		return riteAllowsExact[allowPrometheus](r, wantQuery, func(a allowPrometheus) []string { return a.Queries })
	case SourceELK:
		return riteAllowsExact[allowELK](r, wantQuery, func(a allowELK) []string { return a.Indices })
	default:
		return false, fmt.Errorf("augur: unknown source_type %q", src)
	}
}

// riteAllowsExact — EXACT-matches want against the list of allow values
// extracted from Rite.allow via generic unmarshaling by source_type shape
// (prom: queries; elk: indices). The comparison is a strict string
// comparison — we do no promQL/index normalization (promQL normalization is
// semantically nontrivial and could itself become an allow-list bypass
// vector; exact-match is the security-conservative default).
func riteAllowsExact[T any](r *Rite, want string, pick func(T) []string) (bool, error) {
	var a T
	if err := json.Unmarshal(r.Allow, &a); err != nil {
		return false, fmt.Errorf("augur: rite %d allow unmarshal: %w", r.ID, err)
	}
	for _, v := range pick(a) {
		if v == want {
			return true, nil
		}
	}
	return false, nil
}

// normalizeQueryPath brings a raw vault query to its canonical logical-path
// through the same mechanism as allow paths: the query may arrive as a
// logical path (`secret/keeper/x`) and needs normalizing via ParseRef.
// ParseRef expects a `vault:` prefix, so we add it when the query doesn't
// have one (Soul sends a logical path, not a vault-ref).
func normalizeQueryPath(query string) (string, error) {
	ref := query
	if !hasVaultPrefix(query) {
		ref = "vault:" + query
	}
	return vault.ParseRef(ref)
}

// riteAllowsVaultPath — EXACT-matches the normalized wantPath against
// Rite.allow. allow.paths are normalized through the same normalizeQueryPath:
// the operator could have written the path as `vault:secret/x`, `secret/x`,
// or with an extra slash — the comparison uses the canonical value on both
// sides (otherwise a typo in allow means a silent bypass/miss).
func riteAllowsVaultPath(r *Rite, wantPath string) (bool, error) {
	var a allowVault
	if err := json.Unmarshal(r.Allow, &a); err != nil {
		return false, fmt.Errorf("augur: rite %d allow unmarshal: %w", r.ID, err)
	}
	for _, p := range a.Paths {
		got, err := normalizeQueryPath(p)
		if err != nil {
			// A broken path in allow — skip just this one, check the rest.
			continue
		}
		if got == wantPath {
			return true, nil
		}
	}
	return false, nil
}

func hasVaultPrefix(s string) bool {
	const p = "vault:"
	return len(s) >= len(p) && s[:len(p)] == p
}
