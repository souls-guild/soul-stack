package voyage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
)

// StartGate decides, at the claim, whether a scenario Voyage that has not
// started yet may start now (ADR-0093 §3). It is asked before the row leaves
// pending or scheduled, so a held-back row keeps no start time and spends no
// attempt.
type StartGate interface {
	Decide(ctx context.Context, db ExecQueryRower, v *Voyage, now time.Time) (GateVerdict, error)
}

// GateVerdict is a [StartGate]'s answer. The zero value starts the run.
type GateVerdict struct {
	// DeferTo, when set, is the first moment the run may start: the claim puts
	// the row back to scheduled for it instead of starting it.
	DeferTo time.Time
	// NoStart — the targets have no moment the run may start: their windows
	// do not meet within [maintenance.CommonStartHorizon]. The claim fails the
	// row with Reason, since waiting would never end.
	NoStart bool
	Reason  string
}

// scopeSize counts the units of a target_resolved array; 0 when it is not one.
func scopeSize(raw []byte) int {
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return 0
	}
	return len(ids)
}

// MaintenanceGate is the production [StartGate]: one start for the whole
// Voyage, the first moment every target's gate is open
// ([maintenance.CommonStart]). A target with neither windows nor freezes never
// holds the others.
type MaintenanceGate struct{}

// Decide implements [StartGate].
func (MaintenanceGate) Decide(ctx context.Context, db ExecQueryRower, v *Voyage, now time.Time) (GateVerdict, error) {
	var ids []string
	if err := json.Unmarshal(v.TargetResolved, &ids); err != nil {
		return GateVerdict{NoStart: true, Reason: "the run's targets cannot be read"}, nil
	}
	rules, err := maintenance.LoadRules(ctx, db, ids)
	if err != nil {
		return GateVerdict{}, fmt.Errorf("voyage: start gate: %w", err)
	}
	cals := make([]*maintenance.Calculator, 0, len(rules))
	for _, id := range ids {
		r, ok := rules[id]
		if !ok {
			continue
		}
		c, err := r.Gate(v.InMaintenanceWindow, v.ConfirmedFreezeIDs)
		if err != nil {
			// Only a zone the Keeper can no longer load gets here. Failing the
			// row beats returning the error: the claim reads the queue head
			// first, and an error would hold every Voyage behind this one.
			return GateVerdict{NoStart: true,
				Reason: fmt.Sprintf("the maintenance windows of incarnation %s cannot be evaluated: %v", id, err)}, nil
		}
		cals = append(cals, c)
	}
	start, ok := maintenance.CommonStart(cals, now)
	if !ok {
		return GateVerdict{NoStart: true, Reason: fmt.Sprintf(
			"the targets' maintenance windows never meet: no moment within %d days after their last freeze ends at which all of them may start",
			int(maintenance.CommonStartHorizon/(24*time.Hour)))}, nil
	}
	if start.After(now) {
		return GateVerdict{DeferTo: start}, nil
	}
	return GateVerdict{}, nil
}
