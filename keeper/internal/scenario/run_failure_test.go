package scenario

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// The stored record must read back as the runs API decodes it: masked by its
// text, never by its field names. Masking it against the run's sealed params
// paths once turned `"step": 1` into `"step": "***MASKED***"` for a run with a
// secret param named `step`, and every runs page holding that run failed to
// decode (NIM-1000).
func TestFailureDetail_ReadsBackAndMasksVaultRefs(t *testing.T) {
	step, idx := 2, 3
	f := &applyrun.RunFailure{
		Code: "render_failed",
		Step: &step,
		Tasks: []applyrun.RunFailureTask{{
			Index: &idx, Name: "connect", Module: "core.exec.run", File: "scenario/x/main.yml", Line: 7,
			IncludedFrom: []applyrun.SourceLine{{File: "scenario/x/main.yml", Line: 2}},
		}},
		Host:       "host-a.example.com",
		Cell:       "step",
		Expression: "vars.m['x']",
		Cause:      "no such key: vault:secret/app/db",
	}

	var got applyrun.RunFailure
	if err := json.Unmarshal(failureDetail(f), &got); err != nil {
		t.Fatalf("the stored record does not read back: %v", err)
	}
	want := *f
	want.Cause = audit.MaskedValue
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v\nwant         %+v", got, want)
	}
	if f.Cause != "no such key: vault:secret/app/db" {
		t.Errorf("masking changed the caller's record: %q", f.Cause)
	}

	payload := maskedFailure(f, errors.New("render: cell \"step\": vault:secret/app/db"))
	if payload["summary"] != audit.MaskedValue || payload["code"] != "render_failed" {
		t.Errorf("run_completed failure = %v", payload)
	}
}
