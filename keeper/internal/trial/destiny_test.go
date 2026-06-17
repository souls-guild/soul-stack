package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// writeDestinyFixture кладёт минимальную валидную destiny `destiny-<name>/`
// (destiny.yml + tasks/main.yml) под base и возвращает её каталог.
func writeDestinyFixture(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, "destiny-"+name)
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	man := "name: " + name + "\ndescription: fixture\n"
	if err := os.WriteFile(filepath.Join(dir, "destiny.yml"), []byte(man), 0o644); err != nil {
		t.Fatalf("write destiny.yml: %v", err)
	}
	tasks := "- name: noop\n  module: core.file.present\n  params:\n    path: /tmp/x\n    content: ok\n"
	if err := os.WriteFile(filepath.Join(dir, "tasks", "main.yml"), []byte(tasks), 0o644); err != nil {
		t.Fatalf("write tasks/main.yml: %v", err)
	}
	return dir
}

// TestFixtureDestinyResolver_MirrorsProd — счастливый путь: name объявлена в
// destiny[], file://-URL резолвится относительно service-root, destiny грузится.
func TestFixtureDestinyResolver_MirrorsProd(t *testing.T) {
	root := t.TempDir()
	serviceRoot := filepath.Join(root, "svc")
	if err := os.MkdirAll(serviceRoot, 0o755); err != nil {
		t.Fatalf("mkdir svc: %v", err)
	}
	// destiny в соседнем поддереве (cross-location): root/dst/destiny-pilot.
	writeDestinyFixture(t, filepath.Join(root, "dst"), "pilot")

	r := newFixtureDestinyResolver(serviceRoot, "file://../dst/destiny-{name}",
		[]config.DependencyRef{{Name: "pilot", Ref: "v1.0.0"}})

	got, err := r.Resolve(context.Background(), "pilot")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "pilot" {
		t.Fatalf("Name = %q, ожидали pilot", got.Name)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("ожидали 1 задачу, получили %d", len(got.Tasks))
	}
}

// TestFixtureDestinyResolver_RejectsUndeclared — необъявленная в destiny[]
// зависимость отвергается (зеркало прод-ошибки, ADR-007).
func TestFixtureDestinyResolver_RejectsUndeclared(t *testing.T) {
	r := newFixtureDestinyResolver(t.TempDir(), "file://destiny-{name}", nil)
	_, err := r.Resolve(context.Background(), "ghost")
	if err == nil {
		t.Fatal("ожидали ошибку на необъявленной destiny")
	}
	if !strings.Contains(err.Error(), "не объявлена") {
		t.Fatalf("ожидали ошибку про необъявленную зависимость, получили: %v", err)
	}
}

// TestFixtureDestinyResolver_RejectsNonFileScheme — не-file:// схема в L0
// отвергается (герметичность: ни git, ни сети).
func TestFixtureDestinyResolver_RejectsNonFileScheme(t *testing.T) {
	r := newFixtureDestinyResolver(t.TempDir(), "",
		[]config.DependencyRef{{Name: "x", Ref: "v1", Git: "git@github.com:acme/destiny-x.git"}})
	_, err := r.Resolve(context.Background(), "x")
	if err == nil {
		t.Fatal("ожидали ошибку на не-file:// схеме")
	}
	if !strings.Contains(err.Error(), "герметичен") {
		t.Fatalf("ожидали ошибку про герметичность L0, получили: %v", err)
	}
}

// TestFixtureDestinyResolver_NameCannotEscapeRoot — КРИТИЧНО для безопасности:
// {name} с `../`-обходом не вырывается за destiny-root (securejoin клампит).
// destiny-имя приходит из service.yml::destiny[]; даже если оно содержит `../`,
// итог остаётся внутри объявленного destiny-root, а не уходит к secret/.
func TestFixtureDestinyResolver_NameCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	serviceRoot := filepath.Join(root, "svc", "dst")
	if err := os.MkdirAll(serviceRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// "Секрет" вне destiny-root: root/svc/destiny-secret. Если бы {name} мог
	// вырваться через ../, leaf `destiny-../destiny-secret` достал бы его.
	writeDestinyFixture(t, filepath.Join(root, "svc"), "secret")

	// destiny-root = serviceRoot (template без `../`); name с обходом.
	r := newFixtureDestinyResolver(serviceRoot, "file://destiny-{name}",
		[]config.DependencyRef{{Name: "../destiny-secret", Ref: "v1"}})
	_, err := r.Resolve(context.Background(), "../destiny-secret")
	if err == nil {
		t.Fatal("ожидали ошибку: {name} с ../ не должен вырываться за destiny-root")
	}
	// Кламп даёт путь ВНУТРИ destiny-root (securejoin схлопывает ../), которого
	// там нет → not-found, а НЕ успешный выход к ../destiny-secret.
	if strings.Contains(err.Error(), filepath.Join(root, "svc", "destiny-secret")) {
		t.Fatalf("утечка за destiny-root: резолв достал внешний каталог: %v", err)
	}
}

// TestFixtureDestinyResolver_RejectsPlaceholderNotInLeaf — {name} обязан жить в
// последнем сегменте пути (иначе нет безопасной кламп-границы для имени).
func TestFixtureDestinyResolver_RejectsPlaceholderNotInLeaf(t *testing.T) {
	r := newFixtureDestinyResolver(t.TempDir(), "file://{name}/destiny",
		[]config.DependencyRef{{Name: "x", Ref: "v1"}})
	_, err := r.Resolve(context.Background(), "x")
	if err == nil {
		t.Fatal("ожидали ошибку: {name} не в последнем сегменте")
	}
	if !strings.Contains(err.Error(), "последнем сегменте") {
		t.Fatalf("ожидали ошибку про последний сегмент, получили: %v", err)
	}
}
