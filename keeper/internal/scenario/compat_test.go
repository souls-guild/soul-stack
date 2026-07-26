package scenario

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Guard tests for the engine-compat gate (ADR-0076). The destiny half runs
// against a REAL local-fs git snapshot through the production loader — the
// window has to survive parse → snapshot → resolve, not just the comparison
// helpers.

const compatDestinyTasks = `- name: Lay down the marker file
  module: core.file.present
  params:
    path: /tmp/compat-marker
    content: "ok"
`

// compatDestinyRepo materializes a destiny repo whose destiny.yml carries the
// given `compat:` block (empty string → no block at all).
func compatDestinyRepo(t *testing.T, name, compatBlock string) string {
	t.Helper()
	// `file://` repos are production-forbidden; the loader gates them behind this
	// flag (artifact/scheme.go), same as the artifact package's own tests.
	t.Setenv("SOUL_STACK_ALLOW_FILE_REPOS", "1")
	dir := t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("destiny.yml", "name: "+name+"\n"+compatBlock)
	write("tasks/main.yml", compatDestinyTasks)

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("destiny with compat", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// resolveDestinyAt runs the production resolve path for a destiny declared at
// the given ref, with this keeper reporting rawVersion.
func resolveDestinyAt(t *testing.T, gitURL, name, rawVersion string) error {
	t.Helper()
	src := NewDestinySource(artifact.NewDestinyLoader(t.TempDir(), nil), fixedTemplateSource(gitURL))
	manifest := &config.ServiceManifest{
		Name:    "compat-svc",
		Destiny: []config.DependencyRef{{Name: name, Ref: "main", Git: gitURL}},
	}
	r := src.resolverFor(manifest, rawVersion, nil)
	_, err := r.Resolve(context.Background(), name)
	return err
}

// TestDestinyCompatWindow_InsideAndOutside — the headline behaviour: a destiny
// declaring `max: 0.3.0` is refused by a 0.4.1 keeper with a VERSIONED error,
// and accepted by a keeper inside the window. `max` is exclusive, so 0.3.0
// itself is out.
func TestDestinyCompatWindow_InsideAndOutside(t *testing.T) {
	gitURL := compatDestinyRepo(t, "redis", "compat:\n  keeper: {min: \"0.1.0\", max: \"0.3.0\"}\n")

	cases := []struct {
		name       string
		version    string
		wantReject bool
	}{
		{"inside_window", "v0.2.5", false},
		{"at_min_inclusive", "v0.1.0", false},
		{"prerelease_inside_by_release_core", "v0.2.0-beta.1", false},
		{"at_max_exclusive", "v0.3.0", true},
		{"above_window", "v0.4.1", true},
		{"below_window", "v0.0.9", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resolveDestinyAt(t, gitURL, "redis", tc.version)
			if !tc.wantReject {
				if err != nil {
					t.Fatalf("Resolve on keeper %s = %v, want the destiny to load", tc.version, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Resolve on keeper %s succeeded, want a versioned rejection", tc.version)
			}
			if !errors.Is(err, config.ErrKeeperVersionUnsupported) {
				t.Fatalf("Resolve err = %v, want ErrKeeperVersionUnsupported (not an opaque render failure)", err)
			}
			// The message must name the artifact, its window and this build —
			// otherwise the operator is back to guessing (ADR-0076(g)).
			msg := err.Error()
			for _, want := range []string{"destiny", "redis", "[0.1.0, 0.3.0)", tc.version} {
				if !strings.Contains(msg, want) {
					t.Fatalf("rejection %q is missing %q", msg, want)
				}
			}
		})
	}
}

// TestDestinyCompatWindow_BackcompatNoBlock — a destiny with no `compat:` block
// is unbounded: the feature is opt-in and existing definitions keep running on
// any keeper.
func TestDestinyCompatWindow_BackcompatNoBlock(t *testing.T) {
	gitURL := compatDestinyRepo(t, "redis", "")
	for _, v := range []string{"v0.0.1", "v0.4.1", "v9.9.9"} {
		if err := resolveDestinyAt(t, gitURL, "redis", v); err != nil {
			t.Fatalf("destiny without compat: rejected on keeper %s: %v", v, err)
		}
	}
}

// TestDestinyCompatWindow_DevBuildNotEnforced — the dev-build carve-out: a
// binary built without ldflags reports `0.0.0-dev`, which sorts below every
// declared min. Enforcing it would break plain `go build`/`go run` outright, so
// the window is skipped (loudly) instead.
func TestDestinyCompatWindow_DevBuildNotEnforced(t *testing.T) {
	gitURL := compatDestinyRepo(t, "redis", "compat:\n  keeper: {min: \"5.0.0\", max: \"6.0.0\"}\n")
	for _, v := range []string{config.DevVersionSentinel, "", "abc1234"} {
		if err := resolveDestinyAt(t, gitURL, "redis", v); err != nil {
			t.Fatalf("keeper version %q carries nothing to compare, must not be enforced: %v", v, err)
		}
	}
}

// TestServiceCompatEntity_FromManifest — the service half of the intersection is
// read off the manifest snapshot, attributed to the pinned ref.
func TestServiceCompatEntity_FromManifest(t *testing.T) {
	art := &artifact.ServiceArtifact{
		Ref: artifact.ServiceRef{Name: "redis", Ref: "v1.2.0"},
		Manifest: &config.ServiceManifest{
			Name:   "redis",
			Compat: &config.CompatConfig{Keeper: &config.VersionWindow{Min: "0.2.0", Max: "0.5.0"}},
		},
	}
	e := serviceCompatEntity(art)
	if e.Kind != config.CompatEntityService || e.Name != "redis" || e.Ref != "v1.2.0" {
		t.Fatalf("entity = %+v, want service/redis at v1.2.0", e)
	}
	if e.Window == nil || e.Window.Min != "0.2.0" || e.Window.Max != "0.5.0" {
		t.Fatalf("window = %v, want [0.2.0, 0.5.0)", e.Window)
	}

	// No manifest block → unbounded (the run gate is inert).
	art.Manifest.Compat = nil
	if w := serviceCompatEntity(art).Window; w != nil {
		t.Fatalf("service without compat: must read as unbounded, got %v", w)
	}
}

// TestCheckKeeperCompat_ServiceGate — the pre-render gate: inside → nil, outside
// → versioned error, unbounded / version-less → nil.
func TestCheckKeeperCompat_ServiceGate(t *testing.T) {
	windowed := config.CompatEntity{
		Kind: config.CompatEntityService, Name: "redis", Ref: "v1.0.0",
		Window: &config.VersionWindow{Min: "0.1.0", Max: "0.3.0"},
	}
	unbounded := config.CompatEntity{Kind: config.CompatEntityService, Name: "redis", Ref: "v1.0.0"}

	if err := checkKeeperCompat("v0.2.0", windowed, nil); err != nil {
		t.Fatalf("keeper inside window rejected: %v", err)
	}
	err := checkKeeperCompat("v0.4.1", windowed, nil)
	if !errors.Is(err, config.ErrKeeperVersionUnsupported) {
		t.Fatalf("keeper outside window = %v, want ErrKeeperVersionUnsupported", err)
	}
	if err := checkKeeperCompat("v0.4.1", unbounded, nil); err != nil {
		t.Fatalf("unbounded service rejected: %v", err)
	}
	if err := checkKeeperCompat(config.DevVersionSentinel, windowed, nil); err != nil {
		t.Fatalf("dev build must not be enforced: %v", err)
	}
}

// TestCompatIntersection_ServiceAndDestiny — the narrowest declaration wins over
// the whole set: a keeper inside the service window but outside a destiny's is
// rejected, and the destiny is the one blamed.
func TestCompatIntersection_ServiceAndDestiny(t *testing.T) {
	svc := config.CompatEntity{
		Kind: config.CompatEntityService, Name: "redis", Ref: "v1.0.0",
		Window: &config.VersionWindow{Min: "0.1.0", Max: "0.9.0"},
	}
	dst := config.CompatEntity{
		Kind: config.CompatEntityDestiny, Name: "cluster", Ref: "v2.1.0",
		Window: &config.VersionWindow{Min: "0.1.0", Max: "0.3.0"},
	}
	entities := []config.CompatEntity{svc, dst}

	effective := config.IntersectKeeperWindows(entities)
	if effective.Max != "0.3.0" {
		t.Fatalf("effective window = %s, want the destiny's tighter ceiling 0.3.0", effective.String())
	}

	// 0.4.1 satisfies the service but not the destiny: the gate order is
	// service-then-destiny, so the service check passes and the destiny's fails.
	if err := checkKeeperCompat("v0.4.1", svc, nil); err != nil {
		t.Fatalf("service window admits 0.4.1, gate must pass: %v", err)
	}
	err := checkKeeperCompat("v0.4.1", dst, nil)
	if !errors.Is(err, config.ErrKeeperVersionUnsupported) {
		t.Fatalf("destiny window excludes 0.4.1, want rejection, got %v", err)
	}
	if !strings.Contains(err.Error(), "cluster") {
		t.Fatalf("rejection %q must blame the destiny that set the bound", err.Error())
	}
}

// TestDestinyCompatWindow_MalformedBlockRejectedAtLoad — the gate cannot be
// bypassed with an unparseable window: a malformed `compat:` block is an
// error-level manifest diagnostic, so the destiny never loads at all (rather
// than loading with a window nothing can compare against).
func TestDestinyCompatWindow_MalformedBlockRejectedAtLoad(t *testing.T) {
	for _, block := range []string{
		"compat:\n  keeper: {min: \">=0.1.0\"}\n",               // operator syntax
		"compat:\n  keeper: {min: \"v0.1.0\"}\n",                // v prefix
		"compat:\n  keeper: {}\n",                               // no bounds
		"compat:\n  keeper: {min: \"0.4.0\", max: \"0.3.0\"}\n", // unsatisfiable
	} {
		gitURL := compatDestinyRepo(t, "redis", block)
		if err := resolveDestinyAt(t, gitURL, "redis", "v0.2.0"); err == nil {
			t.Fatalf("destiny with malformed compat block %q loaded, want a manifest error", block)
		}
	}
}
