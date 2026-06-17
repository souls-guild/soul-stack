package beacon

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

func paramStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func TestServiceDownActive(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("systemctl --version", util.Result{ExitCode: 0})
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})

	b := &ServiceDown{Runner: r}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"service": "redis"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateServiceUp {
		t.Fatalf("state = %q, want up", state)
	}
	if data.GetFields()["active"].GetBoolValue() != true {
		t.Error("data.active должно быть true")
	}
}

func TestServiceDownStopped(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("systemctl --version", util.Result{ExitCode: 0})
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 3}) // inactive

	b := &ServiceDown{Runner: r}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"service": "redis"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateServiceDown {
		t.Fatalf("state = %q, want down", state)
	}
	if data.GetFields()["service"].GetStringValue() != "redis" {
		t.Error("data.service должно нести имя сервиса")
	}
}

func TestServiceDownOpenRCStarted(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	// systemctl --version → fallback (не systemd) → детект уходит в OpenRC.
	r.On("rc-service --version", util.Result{ExitCode: 0})
	r.On("rc-service redis status", util.Result{ExitCode: 0, Stdout: " * status: started"})

	b := &ServiceDown{Runner: r}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"service": "redis"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateServiceUp {
		t.Fatalf("state = %q, want up", state)
	}
	if data.GetFields()["active"].GetBoolValue() != true {
		t.Error("data.active должно быть true")
	}
}

func TestServiceDownOpenRCStopped(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("rc-service --version", util.Result{ExitCode: 0})
	// Регрессия: rc-service status даёт exit 0 и для остановленного сервиса,
	// реальный статус — в stdout. exit 0 без "started" → "down".
	r.On("rc-service redis status", util.Result{ExitCode: 0, Stdout: " * status: stopped"})

	b := &ServiceDown{Runner: r}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"service": "redis"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateServiceDown {
		t.Fatalf("state = %q, want down", state)
	}
	if data.GetFields()["active"].GetBoolValue() != false {
		t.Error("data.active должно быть false")
	}
}

func TestServiceDownNoInitSystem(t *testing.T) {
	r := internaltest.NewRunner() // все --version → fallback 127 → InitSystemUnknown
	b := &ServiceDown{Runner: r}
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"service": "redis"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// Нет init-системы — сервис недоступен с точки зрения наблюдателя → "down",
	// не ошибка (см. service_down.go).
	if state != stateServiceDown {
		t.Fatalf("state = %q, want down", state)
	}
}

func TestServiceDownMissingParam(t *testing.T) {
	b := &ServiceDown{Runner: internaltest.NewRunner()}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("ожидали ошибку при отсутствии param service")
	}
}
