package config

import "regexp"

// The vault-secrets-generated passage axis (ADR-056 amendment, Variant A).
//
// Why. A secret-generating scenario (redis create / create_from_souls) generates the
// missing passwords in Vault via the keeper step `core.vault.kv-present`
// (generate-if-absent), and later deploy tasks read those same secrets via
// `${ vault(...) }` in passage-defining fields (params / apply.input / vars / ...).
// Writing a secret and reading it MUST fall into different Passages: the deploy task's
// render (vault_resolve phase, ADR-010) reads Vault Keeper-side BEFORE dispatch — if it
// lands in the same Passage as the generate step, the secret isn't in Vault yet →
// render_failed (vault_resolve on a nonexistent path).
//
// ★ BLOCKER (create_from_souls live bug). The roster axis (passage_refresh.go) held
// this order ONLY when a refresh emitter was present: the create/ provision body pushed
// read tasks into a later Passage. create_from_souls has NO provision body (deploy over
// an already-onboarded roster) → no refresh emitter → roster axis inactive → generate
// and vault()-read collapsed into Passage 0 → render_failed. So the
// `core.vault.kv-present` emitter is a separate class of PASSAGE-DEFINING signal
// "vault-secrets-generated", symmetric to the refresh emitter (only the signal is the
// vault axis, not the register axis nor the roster axis).
//
// Mechanism. The vault emitter is a `core.vault.kv-present` task (it writes secrets to
// targets). The vault consumer is a task reading `${ vault(...) }` in any
// passage-defining field (the same ADR-056 registry as collectTaskReads:
// where / vars / params / apply.input / output / loop.items / loop.when; flow-control
// when/changed_when/failed_when is NOT included, it's Soul-side per-task gating). Any
// vault consumer after a vault emitter (program-order) goes into Passage ≥ 1 + the
// emitter's passage. The edge is wired into visit() (passage.go) as a THIRD class
// alongside register/roster.
//
// Over-approximation on the safe side. A static match of targets↔vault-path is
// INEXPRESSIBLE: targets is complex CEL (concatenation of incarnation.name + per-user
// map), and the vault path in `vault('...')` is also a concatenation. So the edge is
// built coarsely: ANY kv-present emitter → ANY vault()-read, without matching paths. An
// extra Passage is safe (+1 at most); a missed one = render_failed. A cycle is
// impossible: kv-present does NOT read vault() itself (it writes to targets, doesn't
// interpolate a secret), so a vault emitter is never a vault consumer — the edge is
// strictly directed vault-emitter→read.
//
// Not a register graph (like the roster axis): the vault boundary introduces NO register
// references, so the ADR-056 reads⊆refs invariant is untouched (a third orthogonal axis).

// vaultEmitterModuleAddr — the only module carrying vault generation
// (core.vault.kv-present, ADR-017 keeper-side core: verb form write-if-absent).
// The author form of the task address is base+state.
const vaultEmitterModuleAddr = "core.vault.kv-present"

// reVaultRead — a call to the CEL builtin `vault(...)` (ADR-010: the only
// secret-reading builtin). Left boundary is start-of-string OR a non-id/dot char (so
// `myvault(` and `obj.vault(` do NOT match: `vault` must be the root identifier, as in
// the CEL-context grammar). The opening paren is required — it distinguishes a call
// from the identifier `vault` in another context.
var reVaultRead = regexp.MustCompile(`(^|[^A-Za-z0-9_.])vault\(`)

// taskIsVaultEmitter — the task emits the "vault-secrets-generated" signal: it's
// `core.vault.kv-present` (writes secrets to targets). The address is enough — the
// module is semantically write-if-absent by targets, there's no separate discriminator
// flag here (like refresh_soulprint on the roster axis): ANY kv-present step writes secrets.
func taskIsVaultEmitter(t *Task) bool {
	return t.Module != nil && t.Module.Module == vaultEmitterModuleAddr
}

// taskReadsVaultSecret — the task statically reads a secret via `${ vault(...) }` in
// any passage-defining field (ADR-056 registry: where / vars / params /
// apply.input / output / loop.items / loop.when). Recurses through block: (a block is
// the atomic Passage unit; a vault()-read in any child makes the container a vault
// consumer).
//
// Flow-control CEL (when / changed_when / failed_when / retry.until) is NOT included
// here — it's NOT passage-defining (Soul-side per-task gating, ADR-012(d)). And
// `${ vault() }` in flow-control isn't valid anyway: vault() resolves Keeper-side in the
// vault_resolve phase BEFORE dispatch, while flow-control runs Soul-side where vault()
// is unavailable. So excluding flow-control from the vault axis loses no real cases
// (symmetric to the register/roster axes).
func taskReadsVaultSecret(t *Task) bool {
	if exprReadsVault(t.Where) {
		return true
	}
	if t.Loop != nil && (exprReadsVault(t.Loop.When) || valueReadsVault(t.Loop.Items)) {
		return true
	}
	if mapReadsVault(t.Vars) || mapReadsVault(t.Output) {
		return true
	}
	if t.Module != nil && mapReadsVault(t.Module.Params) {
		return true
	}
	if t.Apply != nil && mapReadsVault(t.Apply.Input) {
		return true
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			if taskReadsVaultSecret(&t.Block.Block[i]) {
				return true
			}
		}
	}
	return false
}

// exprReadsVault — the CEL string calls `vault(...)`. CEL string literals are stripped
// with the same celStringLiteral as exprReadsSoulprint/ExtractRegisterRefs — otherwise
// `'vault('` inside string DATA (e.g. a secret-path literal in a comment or message)
// would produce a false edge. One source of truth for the "CEL string literal" grammar.
func exprReadsVault(expr string) bool {
	if expr == "" {
		return false
	}
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	return reVaultRead.MatchString(stripped)
}

// mapReadsVault — any string value of the map (vars/params/apply.input/output),
// recursively through nested map/seq, calls vault() in a `${ … }` interpolation.
func mapReadsVault(m map[string]any) bool {
	for _, v := range m {
		if valueReadsVault(v) {
			return true
		}
	}
	return false
}

// valueReadsVault recursively walks an any value (string / map / seq).
func valueReadsVault(v any) bool {
	switch t := v.(type) {
	case string:
		return exprReadsVault(t)
	case map[string]any:
		for _, sub := range t {
			if valueReadsVault(sub) {
				return true
			}
		}
	case []any:
		for _, sub := range t {
			if valueReadsVault(sub) {
				return true
			}
		}
	}
	return false
}
