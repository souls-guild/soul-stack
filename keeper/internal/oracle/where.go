package oracle

import (
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// WhereEvaluator вычисляет where-CEL предикат Decree над payload Portent-события.
// Контекст активации — единственный корень `event` (форма
// `{"data": <legacy-payload>, "file_changed": {...}, "service_down": {...}, ...}`),
// так что Decree-автор пишет предикаты вида:
//   - `event.data.severity == "critical"`            (legacy через Struct);
//   - `event.file_changed.path.startsWith("/etc/")` (typed-field-access, V5-1).
//
// Backward-compat (ADR-030 amendment 2026-05-26): пока Soul шлёт ОБЕ ветки
// (data + typed), оба стиля where-CEL работают; после S5-final hard-cut `data`
// исчезнет, останется только typed-доступ. Снимок типов payload (file_changed /
// service_down / port_closed / disk_full / process_absent / http_unhealthy /
// custom) — параллель встроенным core-beacon + ветка под plugin-beacon (V5-2).
//
// Отдельный минимальный CEL-env (один объявленный `event`) держится локально в
// oracle, а НЕ в shared/cel: тамошние env-режимы (contextVars / flowControlVars /
// migrationVars) объявляют фиксированные корни scenario/destiny/migration — ни в
// одном нет `event`, а расширение их набора затронуло бы render-контракт
// (cross-package). where-CEL Decree — изолированный предикат над недоверенным
// payload, ему достаточно sandbox-env без vault()/now()/прочих контекст-корней
// (default-deny + субъект уже отфильтровали хост).
//
// Type-mismatch (CEL ожидает file_changed, прилетел service_down) — fail-safe
// no-match: при typed Activation отсутствующая ветка раскрывается в no-such-key,
// cel-go возвращает runtime-error, Eval превращает в `false` (default-deny).
//
// Компилированные программы кешируются по тексту выражения: набор Decree-ов
// невелик, а один Portent может матчить несколько Decree-ов с одинаковым where.
type WhereEvaluator struct {
	env   *cel.Env
	mu    sync.RWMutex
	cache map[string]cel.Program
}

// NewWhereEvaluator собирает sandbox-CEL для where-предикатов Decree. Объявлен
// единственный корень `event` (DynType — все ветки payload-а раскрываются
// dynamically, без proto-introspection в env: typed-message приходят как
// map[string]any в Activation, и cel-go резолвит селекторы field-by-field).
// Ошибка возможна только при несовместимой конфигурации cel-go (программная,
// не пользовательская).
func NewWhereEvaluator() (*WhereEvaluator, error) {
	env, err := cel.NewEnv(
		cel.StdLib(),
		cel.Variable("event", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("oracle: создание where-CEL окружения: %w", err)
	}
	return &WhereEvaluator{env: env, cache: make(map[string]cel.Program)}, nil
}

// Eval вычисляет предикат expr над legacy data-веткой (PortentEvent.data,
// Struct→map, без typed payload). Сохранён для backward-compat с handler-ами,
// собирающими только data-map. Возврат:
//   - (true, nil)  — предикат истинен (match);
//   - (false, nil) — предикат ложен или дал не-bool результат (no match;
//     default-deny — нечёткий результат трактуется как «не сматчило»);
//   - (false, err) — ошибка компиляции выражения (битый where_cel в Decree).
//
// Ошибка вычисления (например обращение к отсутствующему ключу в no-such-key-
// семантике cel) НЕ ошибка функции: cel-go возвращает error на runtime-проблемах,
// мы трактуем их как no-match (false) — недоверенный payload не должен вызывать
// действие из-за случайного совпадения формы. Только compile-ошибка
// (синтаксически невалидный Decree) поднимается наверх для диагностики.
func (w *WhereEvaluator) Eval(expr string, data map[string]any) (bool, error) {
	return w.evalActivation(expr, map[string]any{"event": map[string]any{"data": data}})
}

// EvalEvent вычисляет предикат expr над полным Portent-event-ом, включая typed
// payload (V5-1, ADR-030 amendment 2026-05-26). Активация формы
// `event = {"data": <legacy-map>, "<typed-branch>": {field: value, ...}}` —
// оба стиля where-CEL работают одновременно в hand-off-период. evt==nil → false
// (default-deny на отсутствие payload). Семантика возврата та же, что у Eval.
func (w *WhereEvaluator) EvalEvent(expr string, evt *keeperv1.PortentEvent) (bool, error) {
	if evt == nil {
		// Без event-а пускать предикат не на чем — default-deny.
		_, err := w.program(expr)
		if err != nil {
			return false, err
		}
		return false, nil
	}
	return w.evalActivation(expr, map[string]any{"event": buildEventActivation(evt)})
}

func (w *WhereEvaluator) evalActivation(expr string, activation map[string]any) (bool, error) {
	prg, err := w.program(expr)
	if err != nil {
		return false, err
	}
	out, _, evalErr := prg.Eval(activation)
	if evalErr != nil {
		// Runtime-ошибка (no-such-key и т.п.) → no-match (default-deny).
		return false, nil
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, nil
	}
	return b, nil
}

// buildEventActivation собирает map активации `event` из PortentEvent: legacy
// data-ветка + typed payload-ветка (по oneof-варианту). Несколько ветвей
// одновременно недопустимы (oneof гарантирует ровно одну), но обе формы
// (event.data.* и event.<typed_branch>.*) доступны в одной активации.
func buildEventActivation(evt *keeperv1.PortentEvent) map[string]any {
	act := make(map[string]any, 2)
	if d := evt.GetData(); d != nil {
		act["data"] = d.AsMap()
	} else {
		// CEL no-such-key на event.data.* → runtime-ошибка → no-match (default-deny).
		// Пустая map тут явнее, чем missing key: предикаты `has(event.data.x)` и
		// `event.data.x == "y"` оба ведут себя одинаково — нет совпадения.
		act["data"] = map[string]any{}
	}
	switch p := evt.GetPayload().(type) {
	case *keeperv1.PortentEvent_FileChanged:
		act["file_changed"] = map[string]any{
			"path":   p.FileChanged.GetPath(),
			"sha256": p.FileChanged.GetSha256(),
		}
	case *keeperv1.PortentEvent_ServiceDown:
		act["service_down"] = map[string]any{
			"service":     p.ServiceDown.GetService(),
			"active":      p.ServiceDown.GetActive(),
			"init_system": p.ServiceDown.GetInitSystem(),
		}
	case *keeperv1.PortentEvent_PortClosed:
		act["port_closed"] = map[string]any{
			"host": p.PortClosed.GetHost(),
			"port": int64(p.PortClosed.GetPort()),
		}
	case *keeperv1.PortentEvent_DiskFull:
		act["disk_full"] = map[string]any{
			"path":         p.DiskFull.GetPath(),
			"used_percent": p.DiskFull.GetUsedPercent(),
			"threshold":    p.DiskFull.GetThreshold(),
		}
	case *keeperv1.PortentEvent_ProcessAbsent:
		act["process_absent"] = map[string]any{
			"pattern": p.ProcessAbsent.GetPattern(),
		}
	case *keeperv1.PortentEvent_HttpUnhealthy:
		act["http_unhealthy"] = map[string]any{
			"url":    p.HttpUnhealthy.GetUrl(),
			"status": int64(p.HttpUnhealthy.GetStatus()),
		}
	case *keeperv1.PortentEvent_Custom:
		// Plugin-beacon (V5-2): payload — произвольный Struct, проектируется как
		// map[string]any для совпадения со стилем других ветвей.
		if p.Custom != nil {
			act["custom"] = p.Custom.AsMap()
		} else {
			act["custom"] = map[string]any{}
		}
	case *keeperv1.PortentEvent_Inotify:
		// core.beacon.inotify (V5-3): repeated InotifyEvent проектируется как
		// []map[string]any, чтобы where-CEL мог фильтровать предикатами вида
		// `event.inotify.events.exists(e, e.type == "created")`.
		ino := p.Inotify
		events := make([]any, 0, len(ino.GetEvents()))
		for _, e := range ino.GetEvents() {
			events = append(events, map[string]any{
				"type": e.GetType(),
				"file": e.GetFile(),
				"at":   e.GetAt(),
			})
		}
		act["inotify"] = map[string]any{
			"path":   ino.GetPath(),
			"events": events,
			"count":  int64(ino.GetCount()),
		}
	}
	return act
}

// CompileCheck компилирует предикат expr, не вычисляя его, и кеширует
// результат. Используется management-Service-ом ([Service.CreateDecree]) для
// fail-fast-проверки where-CEL на create: невалидный предикат отвергается 422,
// а не превращается в runtime-сюрприз при первом Portent (default-deny проглотил
// бы битый предикат как no-match без диагностики оператору). Возврат nil —
// предикат синтаксически корректен.
func (w *WhereEvaluator) CompileCheck(expr string) error {
	_, err := w.program(expr)
	return err
}

func (w *WhereEvaluator) program(expr string) (cel.Program, error) {
	w.mu.RLock()
	prg, ok := w.cache[expr]
	w.mu.RUnlock()
	if ok {
		return prg, nil
	}

	ast, iss := w.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("oracle: компиляция where-CEL %q: %w", expr, iss.Err())
	}
	prg, err := w.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("oracle: сборка программы where-CEL %q: %w", expr, err)
	}

	w.mu.Lock()
	w.cache[expr] = prg
	w.mu.Unlock()
	return prg, nil
}
