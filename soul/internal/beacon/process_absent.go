package beacon

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// ProcessAbsentName is the core-beacon address (`core.beacon.<name>`, VigilDef.check).
const ProcessAbsentName = beaconaddr.ProcessAbsent

const (
	stateProcessPresent State = "present"
	stateProcessAbsent  State = "absent"
)

// ProcessAbsent is the core-beacon that observes process presence (ADR-030).
// Read-only: polls via `pgrep` only (no kill/signal). State: "present" if a
// process matching the pattern is found, "absent" otherwise. A
// present↔absent transition is edge-triggered → Portent (typical case: a
// crashed daemon).
//
// Uses util.Runner (like core.service / core.beacon.service_down) rather
// than scanning /proc: pgrep is OS-agnostic (Linux/BSD), and Runner is
// mockable in unit tests.
//
// Param `pattern` (string, required) — process name/ERE pattern (matched
// against the process name, like `pgrep <pattern>`).
type ProcessAbsent struct {
	Runner util.Runner
}

// NewProcessAbsent builds the beacon with a production Runner (os/exec).
func NewProcessAbsent() *ProcessAbsent { return &ProcessAbsent{Runner: util.OSRunner{}} }

func (b *ProcessAbsent) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	pattern, err := util.StringParam(params, "pattern")
	if err != nil {
		return "", nil, err
	}

	// pgrep: exit 0 — at least one match found; exit 1 — no matches;
	// exit ≥2 — pgrep itself errored (bad pattern / missing binary) → Check error.
	r := b.Runner.Run(ctx, "pgrep", pattern)
	if r.Err != nil {
		return "", nil, fmt.Errorf("pgrep %s: %v", pattern, r.Err)
	}
	switch r.ExitCode {
	case 0:
		return stateProcessPresent, processData(pattern), nil
	case 1:
		return stateProcessAbsent, processData(pattern), nil
	default:
		return "", nil, fmt.Errorf("pgrep %s: exit %d: %s", pattern, r.ExitCode, r.Stderr)
	}
}

func processData(pattern string) *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{"pattern": pattern})
	return s
}
