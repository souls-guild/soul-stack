package scenario

import (
	"encoding/json"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// maskedInputSnapshot builds the masked operator-input snapshot persisted on
// apply_runs.input (migration 101) for the run-history read endpoint. It reuses
// the write-path masker audit.MaskSecrets (sensitive-by-name keys + vault refs ->
// ***MASKED***, recursive) so secrets never land in PG (invariant A). Empty input
// or a marshal failure -> nil: the snapshot is observability, never a reason to
// fail a run, and the column stays NULL.
func maskedInputSnapshot(input map[string]any) json.RawMessage {
	if len(input) == 0 {
		return nil
	}
	b, err := json.Marshal(audit.MaskSecrets(input))
	if err != nil {
		return nil
	}
	return b
}
