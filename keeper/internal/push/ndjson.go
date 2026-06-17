package push

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ErrNoRunResult — поток stdout завершился (EOF) без финального RunResult.
// Это маркер недоставленного результата: `soul apply` всегда шлёт ровно один
// RunResult в конце (ndjsonsink.go), его отсутствие = крах процесса/обрыв
// сессии до завершения прогона. Диспетчер обязан трактовать это как fail
// (fail-closed), а не как «успех без отчёта».
var ErrNoRunResult = errors.New("push: NDJSON-поток завершился без RunResult")

// EventHandler — колбэк на каждый промежуточный TaskEvent NDJSON-потока.
// nil допустим: пилоту TaskEvent-ы нужны только end-to-end (RunResult несёт
// итог), полная per-task обработка/запись в apply_task_register — слайс
// интеграции в runner (S3).
type EventHandler func(*keeperv1.TaskEvent)

// ParseStream читает построчный NDJSON-stdout `soul apply` (ndjsonsink.go):
// последовательность protojson-строк TaskEvent, затем РОВНО одна финальная
// строка RunResult. Каждый TaskEvent передаётся в onEvent (если не nil),
// RunResult возвращается как итог.
//
// Дискриминатор TaskEvent vs RunResult: оба сериализуются как голый protojson
// БЕЗ type-тега (ndjsonsink пишет их одним writeLine), но их enum-поле `status`
// использует непересекающиеся префиксы — `TASK_STATUS_*` у TaskEvent против
// `RUN_STATUS_*` у RunResult. Различаем по этому префиксу, а не по «последняя
// строка = RunResult»: построчное чтение не знает заранее, где конец, а опора
// на overlapping-поля (protojson.Unmarshal lenient) дала бы тихий mis-parse.
//
// Возврат:
//   - (*RunResult, nil) — поток корректен, финальный RunResult получен.
//   - (nil, ErrNoRunResult) — EOF без RunResult (краш/обрыв до завершения).
//   - (nil, ошибка) — битая/неклассифицируемая строка, либо ошибка чтения.
//
// Строки после RunResult — ошибка протокола (RunResult финален): такой поток
// отвергается. Пустые строки (двойной '\n') пропускаются.
func ParseStream(r io.Reader, onEvent EventHandler) (*keeperv1.RunResult, error) {
	sc := bufio.NewScanner(r)
	// soul apply может вернуть крупный TaskEvent (register_data/output): поднимаем
	// лимит строки с дефолтных 64 KiB до 1 MiB, чтобы длинный output не рвал поток
	// ErrTooLong-ом на ровном месте.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var result *keeperv1.RunResult
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("push: строка NDJSON после финального RunResult: %q", truncate(line))
		}

		kind, err := classifyLine(line)
		if err != nil {
			return nil, err
		}
		switch kind {
		case lineTaskEvent:
			ev := &keeperv1.TaskEvent{}
			if err := protojson.Unmarshal([]byte(line), ev); err != nil {
				return nil, fmt.Errorf("push: разбор TaskEvent: %w (строка: %q)", err, truncate(line))
			}
			if onEvent != nil {
				onEvent(ev)
			}
		case lineRunResult:
			rr := &keeperv1.RunResult{}
			if err := protojson.Unmarshal([]byte(line), rr); err != nil {
				return nil, fmt.Errorf("push: разбор RunResult: %w (строка: %q)", err, truncate(line))
			}
			result = rr
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("push: чтение NDJSON-потока: %w", err)
	}
	if result == nil {
		return nil, ErrNoRunResult
	}
	return result, nil
}

type lineKind int

const (
	lineTaskEvent lineKind = iota
	lineRunResult
)

const (
	taskStatusPrefix = "TASK_STATUS_"
	runStatusPrefix  = "RUN_STATUS_"
)

// classifyLine определяет тип NDJSON-строки по enum-префиксу её поля `status`
// (см. ParseStream). Поле `status` шлётся обоими сообщениями и его enum-неймы
// непересекающиеся, поэтому это надёжный дискриминатор без type-тега.
//
// Граничные случаи (все → понятная ошибка, fail-closed):
//   - не-JSON / частичная строка → ошибка разбора;
//   - JSON без `status` либо `status` не строка → неклассифицируемо;
//   - `status` с неизвестным префиксом → неклассифицируемо.
func classifyLine(line string) (lineKind, error) {
	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return 0, fmt.Errorf("push: невалидная NDJSON-строка: %w (строка: %q)", err, truncate(line))
	}
	switch {
	case strings.HasPrefix(probe.Status, taskStatusPrefix):
		return lineTaskEvent, nil
	case strings.HasPrefix(probe.Status, runStatusPrefix):
		return lineRunResult, nil
	default:
		return 0, fmt.Errorf("push: неклассифицируемая NDJSON-строка (status=%q): %q", probe.Status, truncate(line))
	}
}

// truncate ограничивает строку для error-сообщений (битая строка может быть
// длинной — не засоряем лог целиком).
func truncate(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
