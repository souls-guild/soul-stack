package scenario

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

func TestDestinySource_ResolveURL(t *testing.T) {
	s := NewDestinySource(nil, fixedTemplateSource("file:///tmp/keeper-dev/destiny/{name}"))
	got, err := s.resolveURL("pilot-flat", "")
	if err != nil {
		t.Fatalf("resolveURL: %v", err)
	}
	if want := "file:///tmp/keeper-dev/destiny/pilot-flat"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

// TestDestinySource_ResolveURL_GitOverride — a per-entry git override is used
// as-is, the default_destiny_source template is ignored.
func TestDestinySource_ResolveURL_GitOverride(t *testing.T) {
	s := NewDestinySource(nil, fixedTemplateSource("file:///tmp/keeper-dev/destiny/{name}"))
	got, err := s.resolveURL("pilot-flat", "git@github.com:custom/destiny-special.git")
	if err != nil {
		t.Fatalf("resolveURL: %v", err)
	}
	if want := "git@github.com:custom/destiny-special.git"; got != want {
		t.Errorf("url = %q, want override %q", got, want)
	}
}

// TestDestinySource_ResolveURL_GitOverrideEmptyTemplate — override works even
// when default_destiny_source isn't set (no name-only dependencies).
func TestDestinySource_ResolveURL_GitOverrideEmptyTemplate(t *testing.T) {
	s := NewDestinySource(nil, fixedTemplateSource(""))
	got, err := s.resolveURL("pilot-flat", "https://git.example/destiny-special.git")
	if err != nil {
		t.Fatalf("resolveURL override without a template: %v", err)
	}
	if want := "https://git.example/destiny-special.git"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

func TestDestinySource_ResolveURL_EmptyTemplate(t *testing.T) {
	s := NewDestinySource(nil, fixedTemplateSource(""))
	if _, err := s.resolveURL("x", ""); err == nil {
		t.Fatal("expected an error on empty default_destiny_source without a git override")
	}
}

// TestDestinySource_ResolveURL_NilSource — a nil template source is treated as
// an empty template (no panic): a name-only dependency with no git override →
// error.
func TestDestinySource_ResolveURL_NilSource(t *testing.T) {
	s := NewDestinySource(nil, nil)
	if _, err := s.resolveURL("x", ""); err == nil {
		t.Fatal("expected an error on a nil template source without a git override")
	}
}

func TestDestinySource_ResolveURL_NoPlaceholder(t *testing.T) {
	s := NewDestinySource(nil, fixedTemplateSource("https://git.example/destiny.git"))
	if _, err := s.resolveURL("x", ""); err == nil {
		t.Fatal("expected an error on a template without {name}")
	}
}

// mutableTemplateSource — a [DestinyTemplateSource] whose value can change
// between resolves (models hot-reload of a keeper_settings scalar).
type mutableTemplateSource struct{ v string }

func (s *mutableTemplateSource) DefaultDestinySource() string { return s.v }

// TestDestinySource_ResolveURL_Lazy — the template is read LAZILY on every
// resolve: a source change after construction is visible immediately
// (hot-reload contract C2, ADR-029). If the constructor copied the string, the
// second resolve would return the stale URL.
func TestDestinySource_ResolveURL_Lazy(t *testing.T) {
	src := &mutableTemplateSource{v: "file:///old/{name}"}
	s := NewDestinySource(nil, src)

	got, err := s.resolveURL("svc", "")
	if err != nil {
		t.Fatalf("resolveURL #1: %v", err)
	}
	if want := "file:///old/svc"; got != want {
		t.Fatalf("url #1 = %q, want %q", got, want)
	}

	src.v = "file:///new/{name}"
	got, err = s.resolveURL("svc", "")
	if err != nil {
		t.Fatalf("resolveURL #2: %v", err)
	}
	if want := "file:///new/svc"; got != want {
		t.Errorf("url #2 = %q, want %q (template is not read lazily)", got, want)
	}
}

// TestDestinyResolver_RefFromManifest — resolverFor takes ref from
// service.yml::destiny[]; a destiny outside the list is rejected before loading.
func TestDestinyResolver_RefFromManifest(t *testing.T) {
	src := NewDestinySource(nil, fixedTemplateSource("file:///tmp/destiny/{name}"))
	manifest := &config.ServiceManifest{
		Name: "pilot-destiny",
		Destiny: []config.DependencyRef{
			{Name: "pilot-flat", Ref: "v1.0.0"},
		},
	}
	r := src.resolverFor(manifest)

	if got := r.deps["pilot-flat"].Ref; got != "v1.0.0" {
		t.Errorf("ref = %q, want v1.0.0", got)
	}
	// destiny outside service.yml::destiny[] → resolve error without touching the loader.
	_, err := r.Resolve(t.Context(), "ghost")
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("Resolve(ghost) err = %v, want 'not declared'", err)
	}
}

// TestDestinyResolver_GitOverrideFromManifest — per-entry git from
// service.yml::destiny[] is forwarded to resolveURL and wins over
// default_destiny_source.
func TestDestinyResolver_GitOverrideFromManifest(t *testing.T) {
	src := NewDestinySource(nil, fixedTemplateSource("file:///tmp/destiny/{name}"))
	manifest := &config.ServiceManifest{
		Name: "pilot-destiny",
		Destiny: []config.DependencyRef{
			{Name: "pilot-flat", Ref: "v1.0.0", Git: "git@github.com:custom/destiny-special.git"},
		},
	}
	r := src.resolverFor(manifest)

	dep := r.deps["pilot-flat"]
	got, err := r.source.resolveURL(dep.Name, dep.Git)
	if err != nil {
		t.Fatalf("resolveURL: %v", err)
	}
	if want := "git@github.com:custom/destiny-special.git"; got != want {
		t.Errorf("url = %q, want override %q", got, want)
	}
}
