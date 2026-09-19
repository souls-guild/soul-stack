package applysink

import (
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// redactSecretOutput returns rd with every top-level field named in fields
// replaced by [audit.MaskedValue] ([ADR-0083] §8). rd is NOT mutated: the same
// *structpb.Struct is the run's live register payload, which
// [Sink.accumulateRegister] stores unredacted so the next task can read what
// this one produced. Redaction applies to the OBSERVABLE copy only.
//
// So a module-declared output secret DOES reach apply_task_register in the clear,
// where the removed `no_log:` skipped that write entirely. That is deliberate and
// bounded by purge_apply_task_register (migration 023/080, grace 1h): masking the
// stored copy would take the value away from the very consumer it was produced for.
// A DECLARED state secret ([ADR-0083] §1) is the other case - it rides as a `vault:`
// ref and has no plaintext form here at all.
//
// Top-level only, because that is the granularity the manifest declares: a state's
// `output:` block names fields, not paths inside them. A module whose secret is
// nested one level down declares the whole containing field secret — coarser than
// necessary, but never wrong, whereas inventing a path syntax the manifest cannot
// express would be.
//
// An empty list is the overwhelmingly common case (a module returns nothing
// secret) and returns rd itself — no copy, no allocation.
func redactSecretOutput(rd *structpb.Struct, fields []string) *structpb.Struct {
	if rd == nil || len(fields) == 0 {
		return rd
	}
	masked := make(map[string]*structpb.Value, len(rd.GetFields()))
	for k, v := range rd.GetFields() {
		masked[k] = v
	}
	hit := false
	for _, f := range fields {
		if _, present := masked[f]; !present {
			// A declared field the module did not return this time (a verb whose
			// output varies, an older module binary). Nothing to mask, and adding
			// the key would invent output that never existed.
			continue
		}
		masked[f] = structpb.NewStringValue(audit.MaskedValue)
		hit = true
	}
	if !hit {
		return rd
	}
	return &structpb.Struct{Fields: masked}
}
