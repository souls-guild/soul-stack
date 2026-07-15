package util_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

func TestSendFinal_WithOutput(t *testing.T) {
	s := &internaltest.ApplyStream{}
	if err := util.SendFinal(s, true, map[string]any{"name": "redis", "installed": true}); err != nil {
		t.Fatalf("SendFinal: %v", err)
	}
	if len(s.Events) != 1 {
		t.Fatalf("events=%d want 1", len(s.Events))
	}
	ev := s.Events[0]
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output == nil || ev.Output.Fields["name"].GetStringValue() != "redis" {
		t.Fatalf("output=%v", ev.Output)
	}
}

func TestSendFinal_NilOutput(t *testing.T) {
	s := &internaltest.ApplyStream{}
	if err := util.SendFinal(s, false, nil); err != nil {
		t.Fatalf("SendFinal: %v", err)
	}
	if s.Events[0].Output != nil {
		t.Fatalf("Output=%v want nil (опущено)", s.Events[0].Output)
	}
	if s.Events[0].Changed {
		t.Fatal("Changed=true want false")
	}
}

// A value that isn't structpb-serializable → SendFinal returns an error, no
// event is sent.
func TestSendFinal_UnserializableOutputErrors(t *testing.T) {
	s := &internaltest.ApplyStream{}
	bad := map[string]any{"fn": func() {}}
	if err := util.SendFinal(s, true, bad); err == nil {
		t.Fatal("SendFinal: want error on unserializable output")
	}
	if len(s.Events) != 0 {
		t.Fatalf("events=%d want 0 (нечего отправлять при ошибке)", len(s.Events))
	}
}

func TestSendFailed(t *testing.T) {
	s := &internaltest.ApplyStream{}
	if err := util.SendFailed(s, "boom"); err != nil {
		t.Fatalf("SendFailed: %v", err)
	}
	if !s.Events[0].Failed || s.Events[0].Message != "boom" {
		t.Fatalf("events[0]=%+v", s.Events[0])
	}
}
