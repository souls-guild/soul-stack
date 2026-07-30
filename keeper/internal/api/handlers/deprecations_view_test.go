package handlers

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// The survey's own logic is covered where it lives (scenario.DeprecationScanner).
// What is NOT covered there is the last hop — the projection into the wire
// shapes — and that hop is exactly the one that fails silently: a field the
// mapper forgets is not a compile error and not a test failure, it is an API
// that answers without it. NIM-243 lost half its feature to precisely this
// (`paramsToInputSchema` dropped keys it did not enumerate, and the symptom read
// as "the backend does not send it").
//
// So these two pin field-completeness rather than behaviour: every field the
// scanner produces has to arrive on the view. Written as a fully-populated
// struct with no zero values, so a dropped field shows up as an empty string
// rather than passing by coincidence (NIM-308).

func TestToDeprecationUsageViews_CarriesEveryField(t *testing.T) {
	in := []scenario.DeprecationUsage{{
		Module:     "community.redis",
		Param:      "address",
		Deprecated: plugin.DeprecatedDef{Since: "0.4.0", RemovedIn: "0.6.0", Use: "addr"},
		Sites: []scenario.DeprecationSite{{
			Incarnation:    "redis-prod",
			Service:        "redis",
			ServiceVersion: "v1.2.0",
			Scenario:       "add_user",
			Where:          "tasks[3].params.address",
		}},
	}}

	got := toDeprecationUsageViews(in)
	if len(got) != 1 {
		t.Fatalf("views = %d, want 1", len(got))
	}
	u := got[0]
	for _, c := range []struct{ field, got, want string }{
		{"Module", u.Module, "community.redis"},
		{"Param", u.Param, "address"},
		{"Since", u.Since, "0.4.0"},
		{"RemovedIn", u.RemovedIn, "0.6.0"},
		{"Use", u.Use, "addr"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q — the mapper drops it, so the API answers without it", c.field, c.got, c.want)
		}
	}
	if len(u.Sites) != 1 {
		t.Fatalf("sites = %d, want 1 — a usage with no site is a finding nobody can act on", len(u.Sites))
	}
	s := u.Sites[0]
	for _, c := range []struct{ field, got, want string }{
		{"Incarnation", s.Incarnation, "redis-prod"},
		{"Service", s.Service, "redis"},
		{"ServiceVersion", s.ServiceVersion, "v1.2.0"},
		{"Scenario", s.Scenario, "add_user"},
		{"Where", s.Where, "tasks[3].params.address"},
	} {
		if c.got != c.want {
			t.Errorf("site.%s = %q, want %q — these are the coordinates the operator navigates by", c.field, c.got, c.want)
		}
	}
}

func TestToDeprecationGapViews_CarriesEveryField(t *testing.T) {
	in := []scenario.DeprecationGap{{
		Scope:        "module",
		Subject:      "community.mongo",
		Reason:       "plugin_namespace",
		Detail:       "no manifest for this module is available here",
		Incarnations: []string{"mongo-a", "mongo-b"},
	}}

	got := toDeprecationGapViews(in)
	if len(got) != 1 {
		t.Fatalf("views = %d, want 1", len(got))
	}
	g := got[0]
	for _, c := range []struct{ field, got, want string }{
		{"Scope", g.Scope, "module"},
		{"Subject", g.Subject, "community.mongo"},
		{"Reason", g.Reason, "plugin_namespace"},
		{"Detail", g.Detail, "no manifest for this module is available here"},
	} {
		if c.got != c.want {
			t.Errorf("gap.%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	// The affected list is the whole point of a gap: it turns "something was not
	// checked" into "these incarnations were not checked".
	if len(g.Incarnations) != 2 || g.Incarnations[0] != "mongo-a" || g.Incarnations[1] != "mongo-b" {
		t.Errorf("gap.Incarnations = %v, want [mongo-a mongo-b]", g.Incarnations)
	}
}

// An empty input must map to an empty, NON-nil slice. The difference is visible
// on the wire — `[]` versus `null` — and a client that branches on one of them
// reads the other as an error. Both mappers already allocate; this keeps that
// deliberate rather than incidental.
func TestDeprecationViews_EmptyInputStaysAnEmptyList(t *testing.T) {
	if got := toDeprecationUsageViews(nil); got == nil {
		t.Error("usage views: nil input produced nil, which serializes as null rather than []")
	}
	if got := toDeprecationGapViews(nil); got == nil {
		t.Error("gap views: nil input produced nil, which serializes as null rather than []")
	}
}
