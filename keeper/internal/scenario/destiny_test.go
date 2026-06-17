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

// TestDestinySource_ResolveURL_GitOverride — per-entry git override берётся как
// есть, шаблон default_destiny_source игнорируется.
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

// TestDestinySource_ResolveURL_GitOverrideEmptyTemplate — override работает даже
// когда default_destiny_source не задан (name-only зависимостей нет).
func TestDestinySource_ResolveURL_GitOverrideEmptyTemplate(t *testing.T) {
	s := NewDestinySource(nil, fixedTemplateSource(""))
	got, err := s.resolveURL("pilot-flat", "https://git.example/destiny-special.git")
	if err != nil {
		t.Fatalf("resolveURL override без шаблона: %v", err)
	}
	if want := "https://git.example/destiny-special.git"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

func TestDestinySource_ResolveURL_EmptyTemplate(t *testing.T) {
	s := NewDestinySource(nil, fixedTemplateSource(""))
	if _, err := s.resolveURL("x", ""); err == nil {
		t.Fatal("ожидалась ошибка на пустой default_destiny_source без git-override")
	}
}

// TestDestinySource_ResolveURL_NilSource — nil-источник шаблона трактуется как
// пустой шаблон (без панки): name-only зависимость без git-override → ошибка.
func TestDestinySource_ResolveURL_NilSource(t *testing.T) {
	s := NewDestinySource(nil, nil)
	if _, err := s.resolveURL("x", ""); err == nil {
		t.Fatal("ожидалась ошибка на nil-источник шаблона без git-override")
	}
}

func TestDestinySource_ResolveURL_NoPlaceholder(t *testing.T) {
	s := NewDestinySource(nil, fixedTemplateSource("https://git.example/destiny.git"))
	if _, err := s.resolveURL("x", ""); err == nil {
		t.Fatal("ожидалась ошибка на шаблон без {name}")
	}
}

// mutableTemplateSource — [DestinyTemplateSource], значение которого можно
// поменять между резолвами (модель hot-reload скаляра keeper_settings).
type mutableTemplateSource struct{ v string }

func (s *mutableTemplateSource) DefaultDestinySource() string { return s.v }

// TestDestinySource_ResolveURL_Lazy — шаблон читается ЛЕНИВО на каждый резолв:
// изменение источника после конструктора видно сразу (контракт hot-reload C2,
// ADR-029). Если бы конструктор копировал строку, второй резолв вернул бы
// старый URL.
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
		t.Errorf("url #2 = %q, want %q (шаблон не читается лениво)", got, want)
	}
}

// TestDestinyResolver_RefFromManifest — resolverFor берёт ref из service.yml::
// destiny[]; destiny вне списка отвергается до загрузки.
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
	// destiny вне service.yml::destiny[] → ошибка резолва без обращения к loader.
	_, err := r.Resolve(t.Context(), "ghost")
	if err == nil || !strings.Contains(err.Error(), "не объявлена") {
		t.Fatalf("Resolve(ghost) err = %v, want 'не объявлена'", err)
	}
}

// TestDestinyResolver_GitOverrideFromManifest — per-entry git из service.yml::
// destiny[] пробрасывается в resolveURL и побеждает default_destiny_source.
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
