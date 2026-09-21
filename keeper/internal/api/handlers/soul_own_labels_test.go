package handlers

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Guard tests for the label model at the handler boundary (NIM-281): a label
// lives only where an operator attached it. A host reaches its own labels and
// nothing else — belonging to an incarnation lends it none — and attaching one
// is gated by what the operator already holds.

// The single-object gate reads the row's own columns. It must not widen them by
// joining membership: a host visible through GET must be a host the list would
// have shown, and the list predicate is `souls.coven && $1` over the row alone.
func TestGetSoul_ReadPath_NeverJoinsMembership(t *testing.T) {
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID:          "db-01.example.com",
			Transport:    soul.TransportAgent,
			Status:       soul.StatusConnected,
			Coven:        []string{"db"},
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
	}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"db"}}, nil, nil)

	rec := doGetSoulScoped(t, h, "db-01.example.com", "archon-dba")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the host carries the operator's own tag; body=%s", rec.Code, rec.Body.String())
	}
	if len(pool.seenSQL) == 0 {
		t.Fatal("no SQL recorded — the fake stopped seeing the read path, the guard below proves nothing")
	}
	for _, sql := range pool.seenSQL {
		if strings.Contains(sql, "incarnation_membership") || strings.Contains(sql, "incarnation_traits") {
			t.Errorf("read path consults membership to decide visibility:\n%s", sql)
		}
	}
}

// A trait attached to the INCARNATION is the incarnation's, not its hosts'. A
// member carrying no `owner` pair of its own stays hidden from `trait.owner=dba`
// — 404, not 403: we do not leak existence.
func TestGetSoul_IncarnationTrait_DoesNotReachMembers(t *testing.T) {
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID:          "db-01.example.com",
			Transport:    soul.TransportAgent,
			Status:       soul.StatusConnected,
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
			// Member of an incarnation labeled `owner=dba`, and labeled with
			// nothing of its own.
		},
	}
	h := NewSoulHandler(pool, fakeScoper{exprs: []string{"trait.owner=dba"}}, nil, nil)

	rec := doGetSoulScoped(t, h, "db-01.example.com", "archon-dba")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a trait on the incarnation must not expose its hosts; body=%s", rec.Code, rec.Body.String())
	}
}

// The same on the Coven axis, and specifically for the incarnation's NAME: the
// name is a membership fact, spelled `incarnation=<name>`, and reaches hosts
// only through `incarnation_membership`. It is not a tag on them, so a scope of
// `coven=redis-prod` shows exactly the hosts an operator tagged `redis-prod`.
func TestGetSoul_IncarnationName_IsNotAHostTag(t *testing.T) {
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID:          "db-01.example.com",
			Transport:    soul.TransportAgent,
			Status:       soul.StatusConnected,
			Coven:        []string{"db"}, // member of redis-prod, tagged only `db`
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
	}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"redis-prod"}}, nil, nil)

	rec := doGetSoulScoped(t, h, "db-01.example.com", "archon-dba")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — the incarnation name is not a tag on its members; body=%s", rec.Code, rec.Body.String())
	}
}

// A host reaches its own labels with no incarnation in the picture at all — the
// grant is the pair on the row, and it needs no membership to work.
func TestGetSoul_OwnTrait_GrantsWithoutMembership(t *testing.T) {
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID:          "spare-01.example.com",
			Transport:    soul.TransportAgent,
			Status:       soul.StatusConnected,
			Traits:       map[string]any{"owner": "bobik"},
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
		// No membership.
	}
	h := NewSoulHandler(pool, fakeScoper{exprs: []string{"trait.owner=bobik"}}, nil, nil)

	rec := doGetSoulScoped(t, h, "spare-01.example.com", "archon-bobik")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a host label must grant on its own; body=%s", rec.Code, rec.Body.String())
	}
}

// An own pair grants that pair and nothing around it: a different value under
// the same key still hides the host (fail-closed).
func TestGetSoul_UnrelatedTrait_StillHidden(t *testing.T) {
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID:          "db-01.example.com",
			Transport:    soul.TransportAgent,
			Status:       soul.StatusConnected,
			Traits:       map[string]any{"owner": "dba"},
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
	}
	h := NewSoulHandler(pool, fakeScoper{exprs: []string{"trait.owner=someone-else"}}, nil, nil)

	rec := doGetSoulScoped(t, h, "db-01.example.com", "archon-eve")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — an unrelated pair must not grant; body=%s", rec.Code, rec.Body.String())
	}
}

// Gate (b): an operator may only attach pairs it is itself scoped on. Without
// this, holding `soul.traits-assign` would be enough to hand a host to a foreign
// role by stamping its pair.
func TestAssignTraits_PairOutsideTraitScope_422(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}, exprs: []string{"trait.owner=bobik"}}, nil, nil)

	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "merge",
		Traits:   map[string]any{"owner": "dba"}, // not the operator's to give away
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (pair outside trait-scope); body=%s", rec.Code, rec.Body.String())
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("UPDATE executed for a pair outside the operator's trait-scope")
	}
}

// A list value is checked element-wise — one out-of-scope element grants exactly
// as much as a whole out-of-scope key would.
func TestAssignTraits_ListValueWithOutOfScopeElement_422(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}, exprs: []string{"trait.owners=bobik"}}, nil, nil)

	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "merge",
		Traits:   map[string]any{"owners": []any{"bobik", "dba"}},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (one list element outside trait-scope); body=%s", rec.Code, rec.Body.String())
	}
}

// Removing a key is ungated on the pair, mirroring coven-assign: gate (a) already
// confines it to the operator's own hosts, and clearing a label grants nothing.
func TestAssignTraits_Remove_NotGatedByTraitScope(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)

	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "remove",
		Keys:     []string{"owner"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (remove is gated on hosts, not on pairs); body=%s", rec.Code, rec.Body.String())
	}
}
