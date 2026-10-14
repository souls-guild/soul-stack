package scenario

import (
	"encoding/json"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// runFailureOf is the structured record of an abort (NIM-1000): the reason code,
// the step the run was at, and — for a render failure — the address render
// attached to the error ([render.Failure]). Masked by [maskFailure] before it
// leaves.
func runFailureOf(reason string, step int, cause error) *applyrun.RunFailure {
	f := &applyrun.RunFailure{Code: reason}
	if step >= 0 {
		f.Step = &step
	}
	rf := render.FailureOf(cause)
	if rf == nil {
		return f
	}
	for _, t := range rf.Tasks {
		ft := applyrun.RunFailureTask{
			Name: t.Name, Module: t.Module, Destiny: t.Destiny,
			File: t.Source.File, Line: t.Source.Line,
		}
		if t.Index >= 0 {
			idx := t.Index
			ft.Index = &idx
		}
		for _, s := range t.IncludedFrom {
			ft.IncludedFrom = append(ft.IncludedFrom, applyrun.SourceLine{File: s.File, Line: s.Line})
		}
		f.Tasks = append(f.Tasks, ft)
	}
	f.Host, f.Cell, f.Expression = rf.Host, rf.Cell, rf.Expr
	f.Cause, f.CauseWithheld = rf.Cause, rf.Withheld
	return f
}

// maskText masks a free text that may quote a vault reference. The run's sealed
// params paths are not applied: they are cell names (`cmd`, `host`, `step`) and
// would match the record's own field names, not anything in its text. A cause
// that could quote a secret VALUE was withheld by render, the only side that
// knows what the expression read.
func maskText(s string) string {
	if s == "" {
		return s
	}
	masked, _ := audit.MaskSecrets(map[string]any{"text": s})["text"].(string)
	return masked
}

// maskFailure masks every text field of the record in place.
func maskFailure(f *applyrun.RunFailure) {
	for _, p := range []*string{&f.Host, &f.Cell, &f.Expression, &f.Cause} {
		*p = maskText(*p)
	}
	for i := range f.Tasks {
		t := &f.Tasks[i]
		for _, p := range []*string{&t.Name, &t.Destiny, &t.File} {
			*p = maskText(*p)
		}
		for j := range t.IncludedFrom {
			t.IncludedFrom[j].File = maskText(t.IncludedFrom[j].File)
		}
	}
}

// failureDetail is the masked record as stored in state_history.error_detail.
func failureDetail(f *applyrun.RunFailure) []byte {
	if f == nil {
		return nil
	}
	b, err := json.Marshal(f)
	if err != nil {
		return nil
	}
	var c applyrun.RunFailure
	if err := json.Unmarshal(b, &c); err != nil {
		return nil
	}
	maskFailure(&c)
	if b, err = json.Marshal(&c); err != nil {
		return nil
	}
	return b
}

// maskedFailure is the record for the incarnation.run_completed payload, with
// the reason's text beside it as `summary` — what error_summary stores.
func maskedFailure(f *applyrun.RunFailure, cause error) map[string]any {
	b := failureDetail(f)
	if b == nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	if cause != nil {
		m["summary"] = maskText(cause.Error())
	}
	return m
}
