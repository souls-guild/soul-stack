package vault

import (
	"github.com/souls-guild/soul-stack/shared/secretpolicy"

	"google.golang.org/protobuf/types/known/structpb"
)

// The generation policy grammar lives in shared/secretpolicy: `core.vault.kv-present`
// and the CEL function generate_secret() must parse one definition, not two that drift
// (ADR-0083 §9). This file is the params-object adapter and nothing else.
type passwordPolicy = secretpolicy.Policy

// defaultPolicy is the base policy from defaults (length=32, ascii-printable-safe).
func defaultPolicy() passwordPolicy { return secretpolicy.Default() }

// parsePolicy parses a policy from a params object (step top-level, or an override
// within a target). A nil object leaves base unchanged, so a per-target override layers
// over the step-level default.
func parsePolicy(obj *structpb.Struct, base passwordPolicy) (passwordPolicy, error) {
	return secretpolicy.Parse(obj.AsMap(), base)
}
