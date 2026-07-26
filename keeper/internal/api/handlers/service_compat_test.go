package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Guard tests for the compat view (ADR-0076(h)). The verdict is a BACKEND
// catalog value — the UI renders status/detail as given and must never re-derive
// compatibility from min/max, so the projection is pinned here.

func win(min, max string) *config.VersionWindow { return &config.VersionWindow{Min: min, Max: max} }

func catalog(entities ...config.CompatEntity) *serviceregistry.CompatCatalog {
	return &serviceregistry.CompatCatalog{SHA1: "deadbeef", Entities: entities}
}

func svcEntity(w *config.VersionWindow) config.CompatEntity {
	return config.CompatEntity{Kind: config.CompatEntityService, Name: "redis", Ref: "v1.0.0", Window: w}
}

func dstEntity(name string, w *config.VersionWindow) config.CompatEntity {
	return config.CompatEntity{Kind: config.CompatEntityDestiny, Name: name, Ref: "v2.1.0", Window: w}
}

// TestServiceCompatReply_Verdicts — the closed status set, one case each.
func TestServiceCompatReply_Verdicts(t *testing.T) {
	cases := []struct {
		name          string
		cat           *serviceregistry.CompatCatalog
		keeperVersion string
		wantStatus    string
		wantEnforced  bool
		wantEffective string // "" → effective_window must be null
	}{
		{
			name:          "inside_window",
			cat:           catalog(svcEntity(win("0.1.0", "0.3.0"))),
			keeperVersion: "v0.2.5",
			wantStatus:    CompatStatusOK,
			wantEnforced:  true,
			wantEffective: "[0.1.0, 0.3.0)",
		},
		{
			name:          "outside_window",
			cat:           catalog(svcEntity(win("0.1.0", "0.3.0"))),
			keeperVersion: "v0.4.1",
			wantStatus:    CompatStatusUnsupported,
			wantEnforced:  true,
			wantEffective: "[0.1.0, 0.3.0)",
		},
		{
			name:          "intersection_narrowest_wins",
			cat:           catalog(svcEntity(win("0.1.0", "0.9.0")), dstEntity("cluster", win("0.2.0", "0.4.0"))),
			keeperVersion: "v0.5.0",
			wantStatus:    CompatStatusUnsupported,
			wantEnforced:  true,
			wantEffective: "[0.2.0, 0.4.0)",
		},
		{
			name:          "backcompat_no_declaration",
			cat:           catalog(svcEntity(nil), dstEntity("cluster", nil)),
			keeperVersion: "v0.4.1",
			wantStatus:    CompatStatusOK,
			wantEnforced:  false,
			wantEffective: "",
		},
		{
			name:          "dev_build_not_enforced",
			cat:           catalog(svcEntity(win("5.0.0", "6.0.0"))),
			keeperVersion: config.DevVersionSentinel,
			wantStatus:    CompatStatusNotEnforced,
			wantEnforced:  false,
			wantEffective: "[5.0.0, 6.0.0)",
		},
		{
			name:          "bare_commit_hash_not_enforced",
			cat:           catalog(svcEntity(win("0.1.0", "0.3.0"))),
			keeperVersion: "abc1234",
			wantStatus:    CompatStatusNotEnforced,
			wantEnforced:  false,
			wantEffective: "[0.1.0, 0.3.0)",
		},
		{
			// An unsatisfiable intersection still blocks the run (every comparable
			// version fails one of the two declarations), so enforced stays true —
			// only the actionable advice changes from "re-pin keeper" to "fix the
			// declarations".
			name:          "empty_intersection_is_authoring_error",
			cat:           catalog(svcEntity(win("0.3.0", "")), dstEntity("cluster", win("", "0.3.0"))),
			keeperVersion: "v0.2.0",
			wantStatus:    CompatStatusWindowEmpty,
			wantEnforced:  true,
			wantEffective: "[0.3.0, 0.3.0)",
		},
		{
			name:          "empty_intersection_on_dev_build_not_enforced",
			cat:           catalog(svcEntity(win("0.3.0", "")), dstEntity("cluster", win("", "0.3.0"))),
			keeperVersion: config.DevVersionSentinel,
			wantStatus:    CompatStatusWindowEmpty,
			wantEnforced:  false,
			wantEffective: "[0.3.0, 0.3.0)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildServiceCompatReply("redis", "v1.0.0", tc.cat, tc.keeperVersion)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (detail: %s)", got.Status, tc.wantStatus, got.Detail)
			}
			if got.Enforced != tc.wantEnforced {
				t.Fatalf("enforced = %v, want %v", got.Enforced, tc.wantEnforced)
			}
			switch {
			case tc.wantEffective == "" && got.EffectiveWindow != nil:
				t.Fatalf("effective_window = %+v, want null (unbounded)", got.EffectiveWindow)
			case tc.wantEffective != "" && got.EffectiveWindow == nil:
				t.Fatalf("effective_window = null, want %s", tc.wantEffective)
			case tc.wantEffective != "" && got.EffectiveWindow.Display != tc.wantEffective:
				t.Fatalf("effective_window.display = %q, want %q", got.EffectiveWindow.Display, tc.wantEffective)
			}
			if got.Detail == "" {
				t.Fatal("detail is empty - the UI has nothing to show for this verdict")
			}
			if got.KeeperVersion != tc.keeperVersion {
				t.Fatalf("keeper_version = %q, want the raw build string %q", got.KeeperVersion, tc.keeperVersion)
			}
		})
	}
}

// TestServiceCompatReply_AttributesTheNarrowBound — the detail of an
// `unsupported` verdict must name the artifact that set the bound, not just the
// intersection: that is the difference between an actionable error and a riddle.
func TestServiceCompatReply_AttributesTheNarrowBound(t *testing.T) {
	cat := catalog(svcEntity(win("0.1.0", "0.9.0")), dstEntity("cluster", win("0.1.0", "0.3.0")))
	got := buildServiceCompatReply("redis", "v1.0.0", cat, "v0.4.1")

	if got.Status != CompatStatusUnsupported {
		t.Fatalf("status = %q, want unsupported", got.Status)
	}
	for _, want := range []string{"destiny", "cluster", "v2.1.0", "[0.1.0, 0.3.0)", "v0.4.1"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("detail %q is missing %q", got.Detail, want)
		}
	}
}

// TestServiceCompatReply_PerEntityContributions — every contributor is listed
// with its own window and verdict, so the UI can show WHERE a bound came from.
func TestServiceCompatReply_PerEntityContributions(t *testing.T) {
	cat := catalog(
		svcEntity(win("0.1.0", "0.9.0")),
		dstEntity("cluster", win("0.1.0", "0.3.0")),
		dstEntity("acl", nil),
	)
	got := buildServiceCompatReply("redis", "v1.0.0", cat, "v0.4.1")

	if len(got.Entities) != 3 {
		t.Fatalf("entities = %d, want 3 (service + 2 destinies)", len(got.Entities))
	}
	if got.Entities[0].Kind != config.CompatEntityService {
		t.Fatalf("entities[0].kind = %q, want the service first", got.Entities[0].Kind)
	}
	if !got.Entities[0].Compatible {
		t.Error("service window admits 0.4.1 - its per-entity verdict must be compatible")
	}
	if got.Entities[1].Compatible {
		t.Error("the destiny caps at 0.3.0 - its per-entity verdict must be incompatible")
	}
	if got.Entities[2].Window != nil {
		t.Errorf("an undeclared window must serialize as null, got %+v", got.Entities[2].Window)
	}
	if !got.Entities[2].Compatible {
		t.Error("an unbounded destiny is compatible by construction")
	}
	if got.Entities[1].Window.Display != "[0.1.0, 0.3.0)" {
		t.Errorf("window.display = %q, want the half-open form", got.Entities[1].Window.Display)
	}
}

// TestServiceCompatReply_KeeperReleaseIsTheComparedValue — the reply must show
// BOTH the raw build string (what the binary reports) and the release core (what
// actually got compared), or a `0.1.0-beta.1-12-gabc` verdict looks arbitrary.
func TestServiceCompatReply_KeeperReleaseIsTheComparedValue(t *testing.T) {
	cat := catalog(svcEntity(win("0.1.0", "0.3.0")))
	got := buildServiceCompatReply("redis", "v1.0.0", cat, "v0.1.0-beta.1-12-gabc1234-dirty")

	if got.Status != CompatStatusOK {
		t.Fatalf("status = %q, want ok (release core 0.1.0 is inside the window)", got.Status)
	}
	if got.KeeperRelease != "0.1.0" {
		t.Fatalf("keeper_release = %q, want the compared release core 0.1.0", got.KeeperRelease)
	}
	if got.KeeperVersion != "v0.1.0-beta.1-12-gabc1234-dirty" {
		t.Fatalf("keeper_version = %q, want the raw build string verbatim", got.KeeperVersion)
	}
}

// TestListServiceCompatTyped_NoListerConfigured — the endpoint degrades like its
// siblings: nil lister → 500 "not configured", service CRUD unaffected.
func TestListServiceCompatTyped_NoListerConfigured(t *testing.T) {
	h := &ServiceHandler{logger: discardSlog()}
	if _, err := h.ListServiceCompatTyped(t.Context(), "redis", ""); err == nil {
		t.Fatal("want an error when the compat lister is not configured")
	}
}

// fakeCompatLister — a CompatLister returning a fixed catalog (or a failure).
type fakeCompatLister struct {
	cat *serviceregistry.CompatCatalog
	err error
}

func (f fakeCompatLister) ListServiceCompat(context.Context, string, string, string) (*serviceregistry.CompatCatalog, error) {
	return f.cat, f.err
}

// TestEarlyCompatCheck — the registration-time convenience gate (ADR-0076(f)):
// a provably incompatible pin is a fast 422, everything uncertain is allowed
// through because the render path is the authority.
func TestEarlyCompatCheck(t *testing.T) {
	cases := []struct {
		name          string
		lister        ServiceCompatLister
		keeperVersion string
		wantReject    bool
		wantIn        string
	}{
		{
			name:          "outside_window_rejected",
			lister:        fakeCompatLister{cat: catalog(svcEntity(win("0.1.0", "0.3.0")))},
			keeperVersion: "v0.4.1",
			wantReject:    true,
			wantIn:        "[0.1.0, 0.3.0)",
		},
		{
			name:          "destiny_narrows_and_is_blamed",
			lister:        fakeCompatLister{cat: catalog(svcEntity(win("0.1.0", "0.9.0")), dstEntity("cluster", win("0.1.0", "0.3.0")))},
			keeperVersion: "v0.4.1",
			wantReject:    true,
			wantIn:        "cluster",
		},
		{
			name:          "empty_intersection_rejected",
			lister:        fakeCompatLister{cat: catalog(svcEntity(win("0.3.0", "")), dstEntity("cluster", win("", "0.3.0")))},
			keeperVersion: "v0.2.0",
			wantReject:    true,
			wantIn:        "compat_window_empty",
		},
		{
			name:          "inside_window_allowed",
			lister:        fakeCompatLister{cat: catalog(svcEntity(win("0.1.0", "0.3.0")))},
			keeperVersion: "v0.2.0",
			wantReject:    false,
		},
		{
			name:          "no_declaration_allowed",
			lister:        fakeCompatLister{cat: catalog(svcEntity(nil))},
			keeperVersion: "v0.4.1",
			wantReject:    false,
		},
		{
			name:          "dev_build_allowed",
			lister:        fakeCompatLister{cat: catalog(svcEntity(win("5.0.0", "6.0.0")))},
			keeperVersion: config.DevVersionSentinel,
			wantReject:    false,
		},
		{
			// Registration must not acquire a hard dependency on git reachability:
			// an unreachable repo behaves exactly as before this check existed.
			name:          "loader_failure_allowed",
			lister:        fakeCompatLister{err: errors.New("git unreachable")},
			keeperVersion: "v0.4.1",
			wantReject:    false,
		},
		{
			name:          "no_lister_allowed",
			lister:        nil,
			keeperVersion: "v0.4.1",
			wantReject:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ServiceHandler{compat: tc.lister, keeperVersion: tc.keeperVersion, logger: discardSlog()}
			err := h.EarlyCompatCheck(t.Context(), "service.register", "redis", "https://git.example/redis.git", "v1.0.0")
			if !tc.wantReject {
				if err != nil {
					t.Fatalf("EarlyCompatCheck = %v, want allow", err)
				}
				return
			}
			if err == nil {
				t.Fatal("EarlyCompatCheck = nil, want a 422 rejection")
			}
			d, ok := AsProblemDetails(err)
			if !ok {
				t.Fatalf("error %v is not a problem detail", err)
			}
			if !strings.Contains(d.Detail, tc.wantIn) {
				t.Fatalf("problem detail %q is missing %q", d.Detail, tc.wantIn)
			}
		})
	}
}
