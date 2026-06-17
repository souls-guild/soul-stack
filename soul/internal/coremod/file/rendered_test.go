package file_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/file"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// renderCtx собирает корень §3.2 ({vars, self, role, essence}) для тестов —
// тот же контракт, что Keeper кладёт в params.render_context (setRenderContext).
func renderCtx(vars, self, essence map[string]any, role string) map[string]any {
	if vars == nil {
		vars = map[string]any{}
	}
	if self == nil {
		self = map[string]any{}
	}
	if essence == nil {
		essence = map[string]any{}
	}
	return map[string]any{
		"vars":    vars,
		"self":    self,
		"role":    role,
		"essence": essence,
	}
}

func TestApply_Rendered_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "host = {{ .vars.host }}\nport = {{ .vars.port }}\n",
			"render_context":   renderCtx(map[string]any{"host": "db1", "port": 5432}, nil, nil, ""),
			"mode":             "0640",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v msg=%q", ev.Changed, ev.Failed, ev.Message)
	}
	got, _ := os.ReadFile(path)
	want := "host = db1\nport = 5432\n"
	if string(got) != want {
		t.Fatalf("content=%q want %q", string(got), want)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v want 0640", info.Mode().Perm())
	}
	if ev.Output.Fields["sha256"].GetStringValue() != sha(want) {
		t.Fatalf("sha256 mismatch")
	}
}

// .self.* — стабильные факты хоста, доступны из корня §3.2. Регресс E2E BUG-A:
// раньше vars подавался плоским корнем → `.self.network.primary_ip` падал
// «map has no entry for key "self"».
func TestApply_Rendered_SelfFact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "redis.conf")
	m := file.New()

	self := map[string]any{
		"network": map[string]any{"primary_ip": "10.0.0.7"},
		"os":      map[string]any{"family": "debian"},
		"sid":     "redis-1.example.com",
	}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "bind {{ .self.network.primary_ip }}\nfamily {{ .self.os.family }}\n",
			"render_context":   renderCtx(nil, self, nil, ""),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true: %q", stream.Last().Message)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "bind 10.0.0.7\nfamily debian\n" {
		t.Fatalf("content=%q", string(got))
	}
}

// .role — declared-роль из spec (bootstrap-create), доступна корнем §3.2.
func TestApply_Rendered_Role(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "role = {{ .role }}\n",
			"render_context":   renderCtx(nil, nil, nil, "primary"),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true: %q", stream.Last().Message)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "role = primary\n" {
		t.Fatalf("content=%q", string(got))
	}
}

// .essence.* — собранный essence (read-only snapshot), доступен корнем §3.2.
func TestApply_Rendered_Essence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	m := file.New()

	essence := map[string]any{"redis": map[string]any{"maxmemory": "512mb"}}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "maxmemory {{ .essence.redis.maxmemory }}\n",
			"render_context":   renderCtx(nil, nil, essence, ""),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true: %q", stream.Last().Message)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "maxmemory 512mb\n" {
		t.Fatalf("content=%q", string(got))
	}
}

func TestApply_Rendered_IdempotentNoChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(path, []byte("value = 42\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "value = {{ .vars.v }}\n",
			"render_context":   renderCtx(map[string]any{"v": 42}, nil, nil, ""),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true для совпадающего рендера")
	}
}

func TestApply_Rendered_ChangeOnContentDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(path, []byte("value = 1\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "value = {{ .vars.v }}\n",
			"render_context":   renderCtx(map[string]any{"v": 2}, nil, nil, ""),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false на diff содержимого")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "value = 2\n" {
		t.Fatalf("content=%q", string(got))
	}
}

// strict-mode: обращение к отсутствующей vars-переменной → ошибка рендера.
func TestApply_Rendered_MissingVarFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "hello {{ .vars.name }}\n",
			"render_context":   renderCtx(map[string]any{}, nil, nil, ""), // .vars.name отсутствует
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false при отсутствующей переменной (ждём strict missingkey)")
	}
	// файл не должен быть создан при ошибке рендера
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("файл создан несмотря на ошибку рендера: %v", err)
	}
}

// Регресс ровно того, что упало в E2E (BUG-A): шаблон обращается к .self, но в
// корне self нет (Keeper не положил self в render_context) → strict-mode ошибка
// «map has no entry for key "self"», а не молчаливый <no value>. Корень §3.2 без
// self отсутствует у нас конструктивно (renderCtx всегда кладёт self), поэтому
// эмулируем «битый Keeper» — render_context только с vars.
func TestApply_Rendered_SelfMissingInContextFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "redis.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "bind {{ .self.network.primary_ip }}\n",
			// render_context без self — единственный ключ vars.
			"render_context": map[string]any{"vars": map[string]any{}},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false при обращении к .self без self в корне (ждём strict missingkey)")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("файл создан несмотря на ошибку рендера: %v", err)
	}
}

// render_context вовсе не доставлен (handoff не настроен) → штатный failed,
// не паника. Симметрично отсутствию template_content.
func TestApply_Rendered_MissingRenderContextFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "x = {{ .vars.x }}\n",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false при отсутствии render_context")
	}
}

func TestApply_Rendered_ParseErrorFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "{{ .unterminated ",
			"render_context":   renderCtx(nil, nil, nil, ""),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false на синтаксически битом шаблоне")
	}
}

func TestApply_Rendered_NestedVars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "addr = {{ .vars.db.host }}:{{ .vars.db.port }}\n",
			"render_context": renderCtx(map[string]any{
				"db": map[string]any{"host": "pg", "port": 6432},
			}, nil, nil, ""),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true: %q", stream.Last().Message)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "addr = pg:6432\n" {
		t.Fatalf("content=%q", string(got))
	}
}

func TestApply_Rendered_ModeOnlyChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(path, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "x = {{ .vars.x }}\n",
			"render_context":   renderCtx(map[string]any{"x": 1}, nil, nil, ""),
			"mode":             "0600",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false на diff mode при совпадающем content")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v want 0600", info.Mode().Perm())
	}
}

func TestApply_Rendered_InvalidModeFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "x\n",
			"render_context":   renderCtx(nil, nil, nil, ""),
			"mode":             "nonsense",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false на невалидном mode")
	}
}

func TestApply_Rendered_MissingTemplateContentFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "rendered",
		Params: mustStruct(t, map[string]any{"path": path}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false при отсутствии template_content")
	}
}

// Атомарная запись не должна оставлять временных .tmp-файлов после успеха.
func TestApply_Rendered_NoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")
	m := file.New()

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "rendered",
		Params: mustStruct(t, map[string]any{
			"path":             path,
			"template_content": "ok\n",
			"render_context":   renderCtx(nil, nil, nil, ""),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("остался временный файл: %s", e.Name())
		}
	}
}
