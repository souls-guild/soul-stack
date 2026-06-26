package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// fakeReader — in-memory TemplateReader для unit-тестов инъекции.
type fakeReader struct {
	files map[string][]byte
}

func (f fakeReader) Read(rel string) ([]byte, error) {
	if d, ok := f.files[rel]; ok {
		return d, nil
	}
	return nil, os.ErrNotExist
}

func renderedTask(module string, params map[string]any) *RenderedTask {
	st, err := structpb.NewStruct(params)
	if err != nil {
		panic(err)
	}
	return &RenderedTask{Name: "t", Module: module, Params: st}
}

// core.file.rendered: template (путь) → template_content (содержимое), путь удалён.
func TestInjectTemplateContent_PathToContent(t *testing.T) {
	rt := renderedTask(moduleFileRendered, map[string]any{
		"path":     "/etc/redis/redis.conf",
		"template": "templates/redis.conf.tmpl",
		"mode":     "0640",
	})
	reader := fakeReader{files: map[string][]byte{
		"templates/redis.conf.tmpl": []byte("port {{ .vars.port }}\n"),
	}}

	if err := injectTemplateContent(rt, reader, ""); err != nil {
		t.Fatalf("injectTemplateContent: %v", err)
	}
	fields := rt.Params.GetFields()
	if _, ok := fields[paramTemplate]; ok {
		t.Error("ключ template должен быть удалён из params (Soul-у путь не нужен)")
	}
	got := fields[paramTemplateContent].GetStringValue()
	if got != "port {{ .vars.port }}\n" {
		t.Errorf("template_content = %q, want literal содержимое .tmpl", got)
	}
	if rt.RawTemplate != got {
		t.Errorf("RawTemplate = %q, want = template_content %q", rt.RawTemplate, got)
	}
	// Прочие ключи не тронуты.
	if fields["path"].GetStringValue() != "/etc/redis/redis.conf" {
		t.Error("path не должен меняться")
	}
}

// Прочий модуль — passthrough: params не трогаются, RawTemplate пуст.
func TestInjectTemplateContent_OtherModulePassthrough(t *testing.T) {
	rt := renderedTask("core.file.present", map[string]any{
		"path":    "/etc/x",
		"content": "data",
	})
	if err := injectTemplateContent(rt, fakeReader{}, ""); err != nil {
		t.Fatalf("injectTemplateContent: %v", err)
	}
	if rt.RawTemplate != "" {
		t.Error("RawTemplate должен быть пуст для не-rendered модуля")
	}
	if _, ok := rt.Params.GetFields()["template_content"]; ok {
		t.Error("template_content не должен появляться у не-rendered модуля")
	}
}

// nil-reader при core.file.rendered с template-путём — ошибка handoff
// (именно этот пробел был прод-блокером golden-path).
func TestInjectTemplateContent_NilReaderIsError(t *testing.T) {
	rt := renderedTask(moduleFileRendered, map[string]any{
		"path":     "/etc/x",
		"template": "templates/x.tmpl",
	})
	err := injectTemplateContent(rt, nil, "")
	if err == nil {
		t.Fatal("ожидалась ошибка: TemplateReader не сконфигурирован")
	}
	if !strings.Contains(err.Error(), "TemplateReader") {
		t.Errorf("ошибка = %q, want упоминание TemplateReader", err)
	}
}

// inline template_content без template-пути — пропускаем (reader не дёргается).
func TestInjectTemplateContent_InlineContentKept(t *testing.T) {
	rt := renderedTask(moduleFileRendered, map[string]any{
		"path":             "/etc/x",
		"template_content": "inline {{ .vars.y }}",
	})
	if err := injectTemplateContent(rt, nil, ""); err != nil {
		t.Fatalf("inline template_content не должен требовать reader: %v", err)
	}
	if got := rt.Params.GetFields()["template_content"].GetStringValue(); got != "inline {{ .vars.y }}" {
		t.Errorf("template_content = %q, want сохранённый inline", got)
	}
}

// Ни template, ни template_content — ошибка.
func TestInjectTemplateContent_MissingBoth(t *testing.T) {
	rt := renderedTask(moduleFileRendered, map[string]any{"path": "/etc/x"})
	if err := injectTemplateContent(rt, fakeReader{}, ""); err == nil {
		t.Fatal("ожидалась ошибка: нет ни template, ни template_content")
	}
}

// non-string template — ошибка (после CEL-фазы путь обязан быть строкой).
func TestInjectTemplateContent_NonStringTemplate(t *testing.T) {
	rt := renderedTask(moduleFileRendered, map[string]any{
		"path":     "/etc/x",
		"template": 42,
	})
	if err := injectTemplateContent(rt, fakeReader{}, ""); err == nil {
		t.Fatal("ожидалась ошибка: template не строка")
	}
}

// Двухуровневый резолв: scenario-local перекрывает service-level (shadowing).
func TestSnapshotTemplateReader_TwoLevelShadowing(t *testing.T) {
	reader := NewSnapshotTemplateReader(
		fakeReader{files: map[string][]byte{
			"scenario/create/templates/x.tmpl": []byte("LOCAL"),
			"templates/x.tmpl":                 []byte("SERVICE"),
		}}.Read,
		"scenario/create",
	)
	data, err := reader.Read("templates/x.tmpl")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "LOCAL" {
		t.Errorf("Read = %q, want LOCAL (scenario-local перекрывает service-level)", data)
	}
}

// Двухуровневый резолв: нет scenario-local → фоллбэк на service-level.
func TestSnapshotTemplateReader_FallbackToService(t *testing.T) {
	reader := NewSnapshotTemplateReader(
		fakeReader{files: map[string][]byte{
			"templates/x.tmpl": []byte("SERVICE"),
		}}.Read,
		"scenario/create",
	)
	data, err := reader.Read("templates/x.tmpl")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "SERVICE" {
		t.Errorf("Read = %q, want SERVICE (фоллбэк на service-level)", data)
	}
}

// Одноуровневый резолв (destiny-проход: пустой prefix).
func TestSnapshotTemplateReader_SingleLevelDestiny(t *testing.T) {
	reader := NewSnapshotTemplateReader(
		fakeReader{files: map[string][]byte{
			"templates/x.tmpl": []byte("DESTINY"),
		}}.Read,
		"",
	)
	data, err := reader.Read("templates/x.tmpl")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "DESTINY" {
		t.Errorf("Read = %q, want DESTINY", data)
	}
}

// Traversal-safety: ридер поверх реального securejoin-снапшота (artifact.
// ReadSnapshotFile) отвергает выход за пределы корня снапшота через `../`.
func TestSnapshotTemplateReader_TraversalRejected(t *testing.T) {
	root := t.TempDir()
	// Секрет ВНЕ снапшота (на уровень выше root).
	parent := filepath.Dir(root)
	secret := filepath.Join(parent, "outside-secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	if err := os.MkdirAll(filepath.Join(root, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "templates", "ok.tmpl"), []byte("OK"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader := NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(root, rel) },
		"",
	)

	// Легитимный путь читается.
	if data, err := reader.Read("templates/ok.tmpl"); err != nil || string(data) != "OK" {
		t.Fatalf("легитимный путь: data=%q err=%v", data, err)
	}

	// `../` за пределы снапшота — securejoin клампит, наружу не выпускает: либо
	// not-found (клампленный путь не существует внутри root), либо ошибка. Главное —
	// содержимое внешнего секрета НЕ возвращается.
	data, err := reader.Read("../" + filepath.Base(secret))
	if err == nil && string(data) == "TOPSECRET" {
		t.Fatal("traversal через ../ вернул внешний секрет — securejoin не сработал")
	}
}
