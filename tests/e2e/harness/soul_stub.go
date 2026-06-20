//go:build e2e

package harness

import (
	"context"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

// TaskResponse — scripted ответ soul-stub-а на одну задачу ApplyRequest по
// task_name (success-only хелпер; harness-обёртка над soulstub.ScriptEntry, чтобы
// тест не импортировал internal/soulstub и keeperv1-enum напрямую).
//
// StateChanges уходит в RunResult.state_changes (per-task артефакт для register/
// drift); incarnation.state мутируется отдельно — keeper-side рендером
// scenario.state_changes.sets ПОСЛЕ барьера (run.go §8), НЕ из RunResult. Поэтому
// StateChanges здесь документирует ожидаемый эффект задачи на хосте, но на assert
// incarnation_state не влияет.
type TaskResponse struct {
	TaskName     string
	StateChanges map[string]any
}

// LoadApplyScript заряжает stub scripted-success-ответами на перечисленные задачи
// (matching по task_name). Задачи, не покрытые скриптом (например collectors.yml-
// шаги под when:, которые на L3a реально не выполняются), ловит
// SetApplyDefaultSuccess — иначе unscripted-задача дала бы FAILED. Симметрично
// stub-responses.yaml::scenarios.<name>.apply_responses, но загружается inline
// (YAML-loader fixtures не реализован — pilot-паттерн, как hello-world).
func LoadApplyScript(stub *soulstub.Stub, scenario string, tasks []TaskResponse) {
	entries := make([]soulstub.ScriptEntry, 0, len(tasks))
	for _, t := range tasks {
		entries = append(entries, soulstub.ScriptEntry{
			TaskName:     t.TaskName,
			Status:       keeperv1.RunStatus_RUN_STATUS_SUCCESS,
			StateChanges: t.StateChanges,
		})
	}
	stub.LoadScript(map[string][]soulstub.ScriptEntry{scenario: entries})
	// Покрываем when:-задачи (collectors.yml), не вошедшие в scripted-таблицу:
	// на L3a реализм per-task не проверяется, важен lifecycle apply_runs.
	stub.SetApplyDefaultSuccess(true)
}

// ConnectSoulStub открывает live EventStream-стрим soul-stub-а к Keeper-у для
// i-го pre-auth Soul-а (см. Config.Souls / SoulSID). Это превращает «строку в
// souls со status=connected» в реальный gRPC-mTLS-стрим: на session-open Keeper
// захватывает Redis SID-lease, и dispatch (Errand/Apply) маршрутизируется в
// локальный Outbound этого SID-а. Без открытого стрима dispatch вернёт
// ErrSoulNotConnected (errand → spawn_error, apply → orphaned).
//
// Stub отвечает на ApplyRequest scripted-RunResult-ом и на ErrandRequest —
// ErrandResult-ом со статусом SUCCESS (см. soulstub.SetErrandStatus для иных
// веток). Закрытие стрима регистрируется в Stack.Cleanup (LIFO).
//
// Возвращает *soulstub.Stub — caller может читать Messages() / менять
// errand-статус до диспетча.
func (s *Stack) ConnectSoulStub(t *testing.T, soulIndex int) *soulstub.Stub {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.souls) {
		t.Fatalf("ConnectSoulStub(%d): out of range (создано %d soul-ов)", soulIndex, len(s.souls))
	}
	id := s.souls[soulIndex]

	stub := soulstub.New(id.SID, s.KeeperGRPCAddr, id.Cert, id.Key, s.caBundle)

	ctx, cancel := context.WithCancel(context.Background())
	if err := stub.Open(ctx); err != nil {
		cancel()
		t.Fatalf("ConnectSoulStub(%s): open stream: %v", id.SID, err)
	}
	s.cleanups = append(s.cleanups, func() {
		_ = stub.Close()
		cancel()
	})

	// Дожидаемся HelloReply: Keeper отправляет его ПОСЛЕ захвата Redis SID-lease
	// (eventstream.go: presence online = живой lease, захваченный до HelloReply).
	// Значит, появление HelloReply в Messages() гарантирует, что dispatch уже
	// сможет смаршрутизировать Errand/Apply в локальный Outbound этого SID-а.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range stub.Messages() {
			if m.Kind == "HelloReply" {
				return stub
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ConnectSoulStub(%s): HelloReply не получен за 10s (lease/handshake не завершён)", id.SID)
	return nil
}
