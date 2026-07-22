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
		t.Error("data.active must be true")
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
		t.Error("data.service must carry the service name")
	}
}

func TestServiceDownOpenRCStarted(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	// systemctl --version → fallback (not systemd) → detection falls through to OpenRC.
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
		t.Error("data.active must be true")
	}
}

func TestServiceDownOpenRCStopped(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("rc-service --version", util.Result{ExitCode: 0})
	// Regression: rc-service status returns exit 0 even for a stopped service —
	// the real status is in stdout. exit 0 without "started" → "down".
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
		t.Error("data.active must be false")
	}
}

func TestServiceDownNoInitSystem(t *testing.T) {
	r := internaltest.NewRunner() // all --version → fallback 127 → InitSystemUnknown
	b := &ServiceDown{Runner: r}
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"service": "redis"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// No init system — the service is unobservable → "down", not an error (see
	// service_down.go).
	if state != stateServiceDown {
		t.Fatalf("state = %q, want down", state)
	}
}

func TestServiceDownMissingParam(t *testing.T) {
	b := &ServiceDown{Runner: internaltest.NewRunner()}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("expected an error when param service is missing")
	}
}
