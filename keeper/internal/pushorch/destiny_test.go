package pushorch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// stubTemplate is a fake DestinyTemplateSource with a fixed template.
type stubTemplate string

func (s stubTemplate) DefaultDestinySource() string { return string(s) }

// stubLoader is a fake DestinyArtifactLoader: records the last call and
// returns the given artifact.
type stubLoader struct {
	lastRef artifact.DestinyRef
	out     *artifact.DestinyArtifact
	err     error
}

func (s *stubLoader) Load(_ context.Context, ref artifact.DestinyRef) (*artifact.DestinyArtifact, error) {
	s.lastRef = ref
	return s.out, s.err
}

func TestPushDestinyResolver_ResolveURLFromTemplate(t *testing.T) {
	loader := &stubLoader{
		out: &artifact.DestinyArtifact{
			LocalDir: t.TempDir(),
			Manifest: &config.DestinyManifest{Name: "redis-base"},
			Tasks:    []config.Task{},
		},
	}
	r := newPushDestinyResolver(loader, stubTemplate("git@github.com:org/destiny-{name}.git"), "redis-base", "v1.4.0")

	resolved, err := r.Resolve(context.Background(), "redis-base")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Name != "redis-base" {
		t.Errorf("resolved.Name = %q, want redis-base", resolved.Name)
	}

	wantURL := "git@github.com:org/destiny-redis-base.git"
	if loader.lastRef.Git != wantURL {
		t.Errorf("loader.Git = %q, want %q", loader.lastRef.Git, wantURL)
	}
	if loader.lastRef.Ref != "v1.4.0" {
		t.Errorf("loader.Ref = %q, want v1.4.0", loader.lastRef.Ref)
	}
	if loader.lastRef.Name != "redis-base" {
		t.Errorf("loader.Name = %q, want redis-base", loader.lastRef.Name)
	}
}

func TestPushDestinyResolver_EmptyTemplate(t *testing.T) {
	r := newPushDestinyResolver(&stubLoader{}, stubTemplate(""), "redis-base", "v1.0.0")
	_, err := r.Resolve(context.Background(), "redis-base")
	if err == nil {
		t.Fatal("expected error on empty default_destiny_source")
	}
	if !strings.Contains(err.Error(), "default_destiny_source") {
		t.Errorf("error = %v, want mention default_destiny_source", err)
	}
}

func TestPushDestinyResolver_TemplateWithoutPlaceholder(t *testing.T) {
	r := newPushDestinyResolver(&stubLoader{}, stubTemplate("git@github.com:org/destiny-fixed.git"), "redis-base", "v1.0.0")
	_, err := r.Resolve(context.Background(), "redis-base")
	if err == nil {
		t.Fatal("expected error on template without {name}")
	}
	if !strings.Contains(err.Error(), "{name}") {
		t.Errorf("error = %v, want mention {name} placeholder", err)
	}
}

func TestPushDestinyResolver_NameMismatch(t *testing.T) {
	r := newPushDestinyResolver(&stubLoader{}, stubTemplate("git@host/{name}.git"), "redis-base", "v1.0.0")
	_, err := r.Resolve(context.Background(), "different-destiny")
	if err == nil {
		t.Fatal("expected error on name mismatch (programmer error)")
	}
	if !strings.Contains(err.Error(), "programmer error") {
		t.Errorf("error = %v, want mention programmer error", err)
	}
}

func TestPushDestinyResolver_LoaderError(t *testing.T) {
	loaderErr := errors.New("git fetch failed")
	loader := &stubLoader{err: loaderErr}
	r := newPushDestinyResolver(loader, stubTemplate("git@host/{name}.git"), "redis-base", "v1.0.0")

	_, err := r.Resolve(context.Background(), "redis-base")
	if err == nil {
		t.Fatal("expected loader error to propagate")
	}
	if !errors.Is(err, loaderErr) {
		t.Errorf("error = %v, want wraps loaderErr", err)
	}
}
