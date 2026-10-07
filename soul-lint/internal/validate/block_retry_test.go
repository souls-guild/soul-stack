package validate

import "testing"

// ★ GUARD (NIM-910). A block's retry: over an applier is refused on the plan AFTER
// include expansion: an applier a within-block include splices in is invisible to
// the per-file validator and to the fixture that writes the applier in the block.
//
// Mutation: hand RetryOverApply the parsed m.Tasks instead of the expanded tasks
// in stage.go → the spliced applier lints clean.
func TestLint_BlockRetryOverAnIncludedApplier(t *testing.T) {
	main := `name: deploy
tasks:
  - name: Converge the cluster
    retry: { count: 3 }
    block:
      - include: roll.yml
`
	included := `- name: Roll the redis destiny over the cluster
  apply:
    destiny: redis
    input: { action: apply }
`
	diags := runJSONAs(t, writeFencedService(t, main, map[string]string{"deploy/roll.yml": included}), "redis")
	if !hasCode(diags, "retry_on_apply_invalid") {
		t.Fatalf("no retry_on_apply_invalid in %+v", diags)
	}
}
