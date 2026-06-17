package push

import (
	"errors"
	"strings"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func taskLine(t *testing.T, ev *keeperv1.TaskEvent) string {
	t.Helper()
	b, err := protojson.MarshalOptions{Multiline: false}.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal TaskEvent: %v", err)
	}
	return string(b)
}

func runLine(t *testing.T, rr *keeperv1.RunResult) string {
	t.Helper()
	b, err := protojson.MarshalOptions{Multiline: false}.Marshal(rr)
	if err != nil {
		t.Fatalf("marshal RunResult: %v", err)
	}
	return string(b)
}

func TestParseStream_HappyPath_EventsThenRunResult(t *testing.T) {
	lines := []string{
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a1", TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_OK}),
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a1", TaskIdx: 1, Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED}),
		runLine(t, &keeperv1.RunResult{ApplyId: "a1", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}),
	}
	stream := strings.Join(lines, "\n") + "\n"

	var got []*keeperv1.TaskEvent
	rr, err := ParseStream(strings.NewReader(stream), func(ev *keeperv1.TaskEvent) {
		got = append(got, ev)
	})
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("TaskEvent-ов = %d, want 2", len(got))
	}
	if got[0].GetTaskIdx() != 0 || got[1].GetTaskIdx() != 1 {
		t.Errorf("порядок TaskEvent-ов нарушен: %d, %d", got[0].GetTaskIdx(), got[1].GetTaskIdx())
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("RunResult.status = %v, want SUCCESS", rr.GetStatus())
	}
	if rr.GetApplyId() != "a1" {
		t.Errorf("RunResult.apply_id = %q", rr.GetApplyId())
	}
}

func TestParseStream_FailureRunResult(t *testing.T) {
	lines := []string{
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a2", TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_FAILED}),
		runLine(t, &keeperv1.RunResult{ApplyId: "a2", Status: keeperv1.RunStatus_RUN_STATUS_FAILED}),
	}
	stream := strings.Join(lines, "\n") + "\n"

	rr, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("ParseStream: %v (FAILED-прогон — валидный итог, не ошибка)", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("RunResult.status = %v, want FAILED", rr.GetStatus())
	}
}

func TestParseStream_NoRunResult(t *testing.T) {
	// Только TaskEvent-ы, поток оборвался до RunResult (краш/обрыв сессии).
	stream := taskLine(t, &keeperv1.TaskEvent{ApplyId: "a3", Status: keeperv1.TaskStatus_TASK_STATUS_OK}) + "\n"

	_, err := ParseStream(strings.NewReader(stream), nil)
	if !errors.Is(err, ErrNoRunResult) {
		t.Fatalf("err = %v, want ErrNoRunResult", err)
	}
}

func TestParseStream_EmptyStream(t *testing.T) {
	_, err := ParseStream(strings.NewReader(""), nil)
	if !errors.Is(err, ErrNoRunResult) {
		t.Fatalf("err = %v, want ErrNoRunResult для пустого потока", err)
	}
}

func TestParseStream_BlankLinesSkipped(t *testing.T) {
	// Двойные '\n' между сообщениями не должны ломать разбор.
	stream := "\n" +
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a4", Status: keeperv1.TaskStatus_TASK_STATUS_OK}) + "\n\n" +
		runLine(t, &keeperv1.RunResult{ApplyId: "a4", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}) + "\n"

	rr, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v", rr.GetStatus())
	}
}

func TestParseStream_BrokenJSON(t *testing.T) {
	stream := "{ not valid json\n"
	_, err := ParseStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("ожидалась ошибка разбора битой строки")
	}
	if errors.Is(err, ErrNoRunResult) {
		t.Errorf("битая строка не должна давать ErrNoRunResult, err=%v", err)
	}
	if !strings.Contains(err.Error(), "невалидная NDJSON-строка") {
		t.Errorf("неинформативная ошибка: %v", err)
	}
}

func TestParseStream_PartialLine_NoTrailingNewline(t *testing.T) {
	// Строка без завершающего '\n' (оборванный writer): bufio.Scanner отдаёт
	// последнюю строку, но она частичная и не парсится как JSON.
	stream := `{"applyId":"a5","stat` // обрыв посреди protojson
	_, err := ParseStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("ожидалась ошибка на частичной строке")
	}
	if errors.Is(err, ErrNoRunResult) {
		t.Errorf("частичная строка → ошибка разбора, не ErrNoRunResult: %v", err)
	}
}

func TestParseStream_UnclassifiableLine(t *testing.T) {
	// Валидный JSON, но status не из наших enum-неймов.
	stream := `{"applyId":"a6","status":"SOMETHING_ELSE"}` + "\n"
	_, err := ParseStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("ожидалась ошибка для неклассифицируемой строки")
	}
	if !strings.Contains(err.Error(), "неклассифицируемая") {
		t.Errorf("ошибка не про классификацию: %v", err)
	}
}

func TestParseStream_LineAfterRunResult(t *testing.T) {
	// RunResult финален: любая строка после него — ошибка протокола.
	stream := runLine(t, &keeperv1.RunResult{ApplyId: "a7", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}) + "\n" +
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a7", Status: keeperv1.TaskStatus_TASK_STATUS_OK}) + "\n"
	_, err := ParseStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("ожидалась ошибка для строки после RunResult")
	}
	if !strings.Contains(err.Error(), "после финального RunResult") {
		t.Errorf("ошибка не про порядок: %v", err)
	}
}
