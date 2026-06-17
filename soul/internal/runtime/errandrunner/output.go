package errandrunner

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// OutputCapBytes — потолок размера stdout/stderr на одной Errand-границе
// (ADR-033 §6 «Инварианты», 64 KiB / channel). Соответствует константе
// keeper/internal/errand.OutputCapBytes — это defense-in-depth (Keeper при
// приёме результата делает тот же cap+mask).
const OutputCapBytes = 64 * 1024

// outputCollector — capture-only сервер ApplyEvent. Реализует
// grpc.ServerStreamingServer[pluginv1.ApplyEvent]: модуль шлёт сюда события,
// collector складывает их в slice. Финал извлекается через [extractFinal] —
// для shell/exec stdout/stderr/exit_code лежат в полях ApplyEvent.Output, для
// read-safe модулей весь Output — структурный.
//
// Не concurrent-safe: модуль исполняется в одной горутине, ApplyEvent-ы
// последовательны.
type outputCollector struct {
	grpc.ServerStream
	ctx    context.Context
	cap    int
	events []*pluginv1.ApplyEvent
}

func newOutputCollector(ctx context.Context, capBytes int) *outputCollector {
	return &outputCollector{ctx: ctx, cap: capBytes}
}

func (c *outputCollector) Context() context.Context     { return c.ctx }
func (c *outputCollector) SetHeader(metadata.MD) error  { return nil }
func (c *outputCollector) SendHeader(metadata.MD) error { return nil }
func (c *outputCollector) SetTrailer(metadata.MD)       {}
func (c *outputCollector) SendMsg(any) error            { return nil }
func (c *outputCollector) RecvMsg(any) error {
	return errors.New("errandrunner: RecvMsg not supported")
}

// Send принимает ApplyEvent от модуля. Capture-only — событие сохраняется
// целиком, masking + cap применяются позже в [extractFinal] (один раз на
// финале, а не на каждом промежуточном событии — core-модули MVP шлют только
// финал).
func (c *outputCollector) Send(ev *pluginv1.ApplyEvent) error {
	c.events = append(c.events, ev)
	return nil
}

// lastEvent — финальный ApplyEvent или nil. Финалом считается последнее
// событие; если модуль не прислал ничего (no-op либо ранний return) — nil.
func (c *outputCollector) lastEvent() *pluginv1.ApplyEvent {
	if len(c.events) == 0 {
		return nil
	}
	return c.events[len(c.events)-1]
}

// extractFinal раскладывает финальный ApplyEvent.Output на компоненты Errand:
//
//   - stdout / stderr — строки из output.fields для core.cmd / core.exec
//     (контракт util.SendFinal: shell/exec кладут "stdout"/"stderr"/"exit_code"
//     прямо в map[string]any output).
//   - exit_code — int32 из числового поля; отсутствие → nil-указатель в proto
//     (поле ErrandResult.exit_code = 0 ≠ "не было exit_code", но proto-default
//     0 — допустимая интерпретация для не-shell модулей).
//   - structured — оставшийся output БЕЗ stdout/stderr/exit_code (для shell —
//     это пустая структура / nil; для read-safe модулей — структурный output
//     модуля целиком, замаскированный).
//
// nil-event (модуль не прислал финал) → пустые stdout/stderr, exitCode=0,
// structured=nil. Это терминал «модуль no-op» — статус выставляется выше.
func (c *outputCollector) extractFinal() (stdout, stderr string, exitCode int32, structured *structpb.Struct) {
	last := c.lastEvent()
	if last == nil || last.GetOutput() == nil {
		return "", "", 0, nil
	}
	out := last.GetOutput()
	fields := out.GetFields()
	if fields == nil {
		return "", "", 0, nil
	}

	// Извлечение известных полей. structpb-типы: stdout/stderr — string,
	// exit_code — number (core.cmd кладёт float64(res.ExitCode)).
	if v, ok := fields["stdout"]; ok {
		stdout = v.GetStringValue()
	}
	if v, ok := fields["stderr"]; ok {
		stderr = v.GetStringValue()
	}
	if v, ok := fields["exit_code"]; ok {
		exitCode = int32(v.GetNumberValue())
	}

	// Структурированный output — то, что осталось без shell-каналов. Для
	// shell/exec результат пустой (всё было stdout/stderr/exit_code), для
	// read-safe модулей (`core.http.probe`) — целиком их output (status/body/
	// elapsed_ms/...). Маскируем через тот же словарь sensitive-keys, что
	// stdout/stderr — единая secret-policy.
	structured = maskOutputExceptShell(out)
	return stdout, stderr, exitCode, structured
}

// maskOutputExceptShell строит масированный *structpb.Struct из output БЕЗ
// shell-полей (stdout/stderr/exit_code). Если после удаления полей структура
// пуста — возвращает nil (handler решит, писать ли NULL в БД на keeper-side).
func maskOutputExceptShell(out *structpb.Struct) *structpb.Struct {
	if out == nil {
		return nil
	}
	raw := out.AsMap()
	delete(raw, "stdout")
	delete(raw, "stderr")
	delete(raw, "exit_code")
	if len(raw) == 0 {
		return nil
	}
	masked := audit.MaskSecrets(raw)
	s, err := structpb.NewStruct(masked)
	if err != nil {
		// Невозможный shape (chan/func) — audit-mask возвращает только
		// json-сериализуемые формы, поэтому defensive: вернём nil вместо
		// потенциальной паники.
		return nil
	}
	return s
}

// MaskAndCapBytes — общий helper для маскировки + cap-а stdout/stderr/error_
// message. Сначала mask (срез мог бы разрезать sensitive-подстроку), потом
// cap. Симметрично keeper/internal/errand.MaskAndCapBytes (defense-in-depth
// на обеих сторонах: тот же словарь, тот же лимит).
//
// Пустая строка → ("", false) без выделения памяти.
func MaskAndCapBytes(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	m := audit.MaskSecrets(map[string]any{"v": s})
	masked, _ := m["v"].(string)
	if masked == "" {
		masked = s
	}
	if len(masked) <= OutputCapBytes {
		return masked, false
	}
	return masked[:OutputCapBytes], true
}
