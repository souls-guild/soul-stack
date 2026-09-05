package render

import "github.com/souls-guild/soul-stack/shared/config"

// secretOutputRegisters returns the register names of tasks whose module declares
// at least one `secret: true` OUTPUT field ([ADR-0083] §8).
//
// This is the second source of the `register.<name>` addresses in [cel.SealSources.Fields], beside the
// `vault:` refs resolveRegisterSecrets actually resolved. The two cover different
// things and the difference is the reason both exist. §6 seals a register because a
// reference in it resolved — provenance observed at runtime, and the only signal
// available for a value the platform itself minted. §8 seals a register because the
// module said the field it returns is a secret — provenance declared at authoring
// time, for a value the platform never sees the inside of.
//
// Without this the §8 declaration would reach only the observable copy of the task's
// own event (redactSecretOutput). A later cell reading `${ register.<name>.<field> }`
// would be unsealed, so audit.MaskSecretsSealed would pass it through into
// apply_run_plan.params and status_details in the clear — which is precisely what
// the `no_log:` this replaces used to block, bluntly, by suppressing the whole task.
//
// Sealing by NAME, not by field: the seal is whole-cell (a cell that read a secret
// source is masked entire), so a per-field seal would be a distinction the masking
// layer cannot represent. Erring wide is the correct direction here — a masked
// non-secret costs an operator one reveal, an unmasked secret costs a rotation.
func secretOutputRegisters(tasks []config.Task, modules config.ModuleManifestResolver) map[string]bool {
	var out map[string]bool
	var walk func([]config.Task)
	walk = func(ts []config.Task) {
		for i := range ts {
			t := &ts[i]
			if t.Block != nil {
				walk(t.Block.Block)
			}
			if t.Register == "" || t.Module == nil {
				continue
			}
			if len(config.SecretOutputFields(t.Module.Module, modules)) == 0 {
				continue
			}
			if out == nil {
				out = map[string]bool{}
			}
			out[t.Register] = true
		}
	}
	walk(tasks)
	return out
}

// mergeSealedRegisters unions b into a, allocating only when there is something to
// carry. Either side may be nil.
func mergeSealedRegisters(a, b map[string]bool) map[string]bool {
	if len(b) == 0 {
		return a
	}
	if a == nil {
		return b
	}
	for k := range b {
		a[k] = true
	}
	return a
}
