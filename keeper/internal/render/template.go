package render

import (
	"errors"
	"fmt"
	"os"

	"google.golang.org/protobuf/types/known/structpb"
)

// isNotExist распознаёт «файла нет» сквозь обёртки securejoin-ридера (он
// оборачивает os.ReadFile через %w). Только это условие триггерит фоллбэк
// scenario-local→service-level; прочие I/O-ошибки не маскируются.
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// moduleFileRendered — адрес шага, для которого Keeper доставляет literal
// template-content (A1, ADR-012(d)). Только у него params.template (путь)
// заменяется на params.template_content (содержимое).
const moduleFileRendered = "core.file.rendered"

// paramTemplate / paramTemplateContent — ключи params шага core.file.rendered.
// template — путь к `.tmpl` (вход автора scenario/destiny); template_content —
// literal-содержимое, которое Keeper кладёт после чтения файла, а Soul рендерит
// сам (rendered.go читает именно template_content).
const (
	paramTemplate        = "template"
	paramTemplateContent = "template_content"
)

// paramRenderContext — ключ params, под которым Keeper доставляет собранный
// per-host корень text/template-контекста core.file.rendered: {vars, self, role,
// essence} (templating.md §3.2). Soul читает его и передаёт КОРНЕМ в
// text/template (rendered.go). Без proto-изменений (A1, ADR-012(d)) — едет
// внутри RenderedTask.params.
const paramRenderContext = "render_context"

// paramVars — авторский ключ params шага core.file.rendered: значения, которые
// автор поднимает для шаблона (templating.md §6, `params.vars`). Keeper его
// CEL-рендерит, переносит под render_context.vars (§3.2-корень) и УДАЛЯЕТ из
// params — Soul читает корень только из render_context.
const paramVars = "vars"

// TemplateReader читает literal-содержимое `.tmpl`-файла из снапшота артефакта по
// relative-path (как он записан в `params.template`, например
// `templates/redis.conf.tmpl`). Реализация обязана защищать от traversal
// (`../`/абсолютный путь/симлинк наружу) и резолвить двухуровнево
// scenario-local→service-level там, где это требует ADR-009.
//
// Симметрично [DestinyResolver]: узкий per-run интерфейс, прод-реализация —
// snapshot-backed (см. [SnapshotTemplateReader]), герметичный Trial L0 — fixture-
// backed reader поверх локального дерева кейса.
type TemplateReader interface {
	Read(relPath string) ([]byte, error)
}

// snapshotReadFunc читает файл из снапшота по relative-path с securejoin-защитой.
// Точечная инъекция artifact.readSnapshotFile (она неэкспортирована) без
// затягивания зависимости render→artifact: caller (run.go) передаёт замыкание.
type snapshotReadFunc func(relPath string) ([]byte, error)

// SnapshotTemplateReader — прод-реализация [TemplateReader] поверх материализо-
// ванного снапшота. Двухуровневый резолв (ADR-009, orchestration.md §6): сначала
// scenario-local `scenario/<name>/<relPath>`, затем service-level `<relPath>` —
// ближний полностью перекрывает дальний (shadowing, без merge).
//
// scenarioPrefix задаёт scenario-local-каталог (`scenario/<name>`); пустой —
// одноуровневый резолв (destiny-проход: `.tmpl` лежат прямо под корнем снапшота
// destiny, scenario-local-слоя у destiny нет). read — securejoin-backed чтение
// из снапшота (защита от traversal на каждом уровне).
type SnapshotTemplateReader struct {
	read           snapshotReadFunc
	scenarioPrefix string
}

// NewSnapshotTemplateReader строит ридер поверх снапшота. read обязателен
// (securejoin-backed чтение из конкретного снапшота). scenarioPrefix — каталог
// scenario-local-слоя (`scenario/<name>`); "" → одноуровневый резолв.
func NewSnapshotTemplateReader(read snapshotReadFunc, scenarioPrefix string) *SnapshotTemplateReader {
	return &SnapshotTemplateReader{read: read, scenarioPrefix: scenarioPrefix}
}

// Read резолвит relPath двухуровнево. На каждом уровне путь читается через
// securejoin-backed read (traversal заклампен снапшот-ридером). Отсутствие файла
// на scenario-local-уровне → фоллбэк на service-level; отсутствие на обоих →
// ошибка not-found. I/O-ошибку (не «нет файла») на любом уровне НЕ маскируем.
func (r *SnapshotTemplateReader) Read(relPath string) ([]byte, error) {
	if r.scenarioPrefix != "" {
		local := r.scenarioPrefix + "/" + relPath
		data, err := r.read(local)
		if err == nil {
			return data, nil
		}
		if !isNotExist(err) {
			return nil, fmt.Errorf("render: чтение шаблона %q (scenario-local %q): %w", relPath, local, err)
		}
	}
	data, err := r.read(relPath)
	if err != nil {
		return nil, fmt.Errorf("render: чтение шаблона %q (service-level): %w", relPath, err)
	}
	return data, nil
}

// injectTemplateContent для шага core.file.rendered заменяет в уже CEL-rendered
// params путь `template` на literal-содержимое `template_content` (A1,
// ADR-012(d)). text/template здесь НЕ исполняется — рендер на Soul (rendered.go).
//
// preloaded — содержимое, уже прочитанное caller-ом (renderTaskIter →
// resolveTemplateUsesInput) при детекте `.input`-обращения, чтобы файл не читался
// дважды. Непустое preloaded → используется как есть (reader не трогается).
// Пустое preloaded (inline-шаблон, либо не-rendered модуль) → fallback на чтение
// через reader по params.template, как раньше.
//
// Прочие модули проходят насквозь (rt.Params не трогаются, "" RawTemplate).
// reader=nil для core.file.rendered с params.template и без preloaded — ошибка
// handoff (Keeper не настроен доставлять содержимое; именно этот пробел был
// прод-блокером golden-path).
//
// params.template обязан быть строкой-путём (после CEL-фазы; non-string —
// ошибка). После инъекции ключ template удаляется — Soul-у путь не нужен.
func injectTemplateContent(rt *RenderedTask, reader TemplateReader, preloaded string) error {
	if rt.Module != moduleFileRendered {
		return nil
	}
	if rt.Params == nil {
		return fmt.Errorf("render: task %q (core.file.rendered): params пусты — нет ключа %q", rt.Name, paramTemplate)
	}
	fields := rt.Params.GetFields()
	tv, ok := fields[paramTemplate]
	if !ok {
		// template_content уже задан напрямую (inline-шаблон без файла) — пропускаем.
		if _, has := fields[paramTemplateContent]; has {
			rt.RawTemplate = preloaded
			return nil
		}
		return fmt.Errorf("render: task %q (core.file.rendered): нет ни %q (путь), ни %q (inline-содержимое)", rt.Name, paramTemplate, paramTemplateContent)
	}
	rel := tv.GetStringValue()
	if _, isStr := tv.GetKind().(*structpb.Value_StringValue); !isStr || rel == "" {
		return fmt.Errorf("render: task %q (core.file.rendered): %q должен быть непустой строкой-путём, получено %v", rt.Name, paramTemplate, tv.AsInterface())
	}

	content := preloaded
	if content == "" {
		if reader == nil {
			return fmt.Errorf("render: task %q (core.file.rendered): TemplateReader не сконфигурирован — Keeper не может доставить содержимое шаблона %q (RenderInput.Templates=nil)", rt.Name, rel)
		}
		data, err := reader.Read(rel)
		if err != nil {
			return fmt.Errorf("render: task %q (core.file.rendered): %w", rt.Name, err)
		}
		content = string(data)
	}

	fields[paramTemplateContent] = structpb.NewStringValue(content)
	delete(fields, paramTemplate)
	rt.RawTemplate = content
	return nil
}
