package handlers

import (
	"net/http"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Guard tests for the label model of ADR-080 at the handler boundary: a label
// attached to an incarnation reaches its hosts, a label attached to a host is
// nobody else's to overwrite, and attaching one is gated by what the operator
// already holds.

// A trait set on the INCARNATION must expose its member hosts. This is the whole
// point of preferring the incarnation as the place to label: it covers hosts that
// join later, without any per-host stamping.
func TestGetSoul_InheritedTrait_GrantsVisibility(t *testing.T) {
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID:          "db-01.example.com",
			Transport:    soul.TransportAgent,
			Status:       soul.StatusConnected,
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
			// No labels of its own — everything comes from the incarnation.
		},
		inheritedTraits: []byte(`[{"owner":"dba"}]`),
	}
	h := NewSoulHandler(pool, fakeScoper{exprs: []string{"trait.owner=dba"}}, nil, nil)

	rec := doGetSoulScoped(t, h, "db-01.example.com", "archon-dba")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a trait on the incarnation must expose its hosts; body=%s", rec.Code, rec.Body.String())
	}
}

// The same for the Coven axis, and specifically for the incarnation's NAME: the
// incarnation-side resolver already treats the name as a coven tag, so host
// visibility has to agree or `coven=<incarnation>` would show the incarnation
// while hiding everything in it.
func TestGetSoul_InheritedIncarnationName_GrantsVisibility(t *testing.T) {
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID:          "db-01.example.com",
			Transport:    soul.TransportAgent,
			Status:       soul.StatusConnected,
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
		inheritedCovens: []string{"redis-prod"},
	}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"redis-prod"}}, nil, nil)

	rec := doGetSoulScoped(t, h, "db-01.example.com", "archon-dba")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the incarnation name must reach its hosts; body=%s", rec.Code, rec.Body.String())
	}
}

// Inheritance widens, never narrows: a host still reaches its own labels when it
// belongs to no incarnation at all.
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

// The key held on BOTH sides: `owner=dba` on the incarnation, `owner=bobik` on
// the host. Neither wins — both grant, so both operators see the host. A
// precedence rule would silently revoke one of them.
func TestGetSoul_KeyOnBothSides_EitherValueGrants(t *testing.T) {
	newPool := func() *fakeReadPool {
		return &fakeReadPool{
			soul: &soul.Soul{
				SID:          "db-01.example.com",
				Transport:    soul.TransportAgent,
				Status:       soul.StatusConnected,
				Traits:       map[string]any{"owner": "bobik"},
				RegisteredAt: time.Now().UTC().Truncate(time.Second),
			},
			inheritedTraits: []byte(`[{"owner":"dba"}]`),
		}
	}

	for _, tc := range []struct{ aid, scope string }{
		{"archon-dba", "trait.owner=dba"},
		{"archon-bobik", "trait.owner=bobik"},
	} {
		h := NewSoulHandler(newPool(), fakeScoper{exprs: []string{tc.scope}}, nil, nil)
		rec := doGetSoulScoped(t, h, "db-01.example.com", tc.aid)
		if rec.Code != http.StatusOK {
			t.Errorf("scope %q: status = %d, want 200 — both values of a contested key must grant; body=%s",
				tc.scope, rec.Code, rec.Body.String())
		}
	}
}

// Inheritance must not become a blanket grant: an unrelated pair still hides the
// host (fail-closed, and 404 rather than 403 — we do not leak existence).
func TestGetSoul_UnrelatedTrait_StillHidden(t *testing.T) {
	pool := &fakeReadPool{
		soul: &soul.Soul{
			SID:          "db-01.example.com",
			Transport:    soul.TransportAgent,
			Status:       soul.StatusConnected,
			RegisteredAt: time.Now().UTC().Truncate(time.Second),
		},
		inheritedTraits: []byte(`[{"owner":"dba"}]`),
	}
	h := NewSoulHandler(pool, fakeScoper{exprs: []string{"trait.owner=someone-else"}}, nil, nil)

	rec := doGetSoulScoped(t, h, "db-01.example.com", "archon-eve")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — inheritance must not grant an unrelated pair; body=%s", rec.Code, rec.Body.String())
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
