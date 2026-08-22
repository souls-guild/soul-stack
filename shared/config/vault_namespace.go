package config

// The fence over a service's own Vault namespace ([ADR-0083] §7).
//
// A secret belonging to a service is declared as a `state_schema` field
// (`type: secret`) and its path is DERIVED from (service, incarnation, field, key).
// The corollary is that the whole `<mount>/<service>/…` prefix now belongs to the
// platform: an author who ALSO writes a path under it by hand creates a second copy
// of the same secret, free to diverge from the one the platform mints and reveals —
// the failure the ADR exists to remove. Fencing only the CEL macro would leave that
// intact for whoever preferred one of the other spellings, and `kv-present` in
// particular is the same write this ADR replaces, wearing a module's name.
//
// So the fence is on the NAMESPACE, not on the mechanism. Every channel survives
// unchanged for paths OUTSIDE the prefix — a shared TLS CA, another service's
// credential — and only paths inside it are rejected, in each spelling the DSL
// offers:
//
//   - `${ vault('secret/<service>/…') }` in any CEL string, including one assembled
//     by concatenation (the literal still carries the prefix);
//   - a `vault:secret/<service>/…` reference in `params:` or any other value;
//   - the `path:` of `core.vault.kv-read` and the `targets:` of
//     `core.vault.kv-present`.
//
// The mount is not compared, deliberately and symmetrically with the runtime gate
// (keeper/internal/render.ownNamespaceRef): what makes a path the service's own is
// the service segment, not which mount it lives on, and load-time does not know
// keeper.yml's `vault.kv_mount` anyway. Nor is the incarnation segment compared —
// at load it is `${ incarnation.name }`, unresolved. The fence is therefore
// service-wide, which is strictly broader than the runtime gate and correct: the
// platform owns the prefix for every incarnation, not just the one being rendered.
//
// What this scan structurally cannot reach is a path assembled entirely out of
// variables (`vault(vars.some_ref)`): it names no segment static text could compare.
// That is caught on the EVALUATED path instead, by
// keeper/internal/render.ownNamespaceVaultGuard, which every CEL pass that can call
// vault() installs through render.WithVaultFence. What the guard cannot see in turn,
// a `core.vault.*` path arriving as a rendered param, is caught by
// ScanRenderedVaultParams below.
//
// One more channel reaches Vault without passing through any of those three: a
// `vault:` ref in OPERATOR input, admitted by the field's `vault_scope`. Its scope is
// a prefix-glob, so `secret/*` covers the namespace while spelling no segment this
// comparison could match, and the value arrives from the operator rather than from
// the author — it never becomes CEL and never becomes a module param. It is fenced at
// its own resolver (keeper/internal/scenario.buildInputVaultResolver), against the
// same predicate. Four layers, one PathAddressesOwnNamespace, and the four are the
// guarantee.

import (
	"sort"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// VaultOwnNamespaceCode — an author-written Vault path under the prefix the
// platform derives its own secrets into.
const VaultOwnNamespaceCode = "vault_path_in_own_namespace"

// vaultPathModules — the modules that take a Vault path as a parameter rather than
// through the `vault()` macro. Every string in their params is scanned for a path,
// because neither module spells its target the same way: kv-read takes a `path:`
// scalar, kv-present a `targets:` list built by CEL.
var vaultPathModules = map[string]bool{
	"core.vault.kv-read":   true,
	vaultEmitterModuleAddr: true, // core.vault.kv-present
}

// PathAddressesOwnNamespace reports whether p resolves under the namespace the
// platform derives service's secrets into — `<mount>/<service>/…`.
//
// p may carry the `vault:` marker and a `#<field>` selector; both are stripped. The
// segments are compared WHOLE: a prefix comparison would let `<mount>/<service>-evil/…`
// through, and that is the one mistake a namespace gate cannot afford. An empty
// service matches nothing (a caller with no manifest in scope checks nothing rather
// than everything).
//
// The comparison is made on the path as the VAULT CLIENT will see it, not as it was
// typed, because a gate that decides on a different string than the one that resolves
// is not a gate. Two rewrites happen downstream and are applied here first:
//
//   - Empty and `.` segments are dropped. `vault.normalizeLogical` collapses repeated
//     slashes and `Client.relativeKVPath` trims a leading one, so
//     `secret//<service>/…` reaches the very secret the platform mints while splitting
//     into a segs[1] of "".
//   - The mount may be absent. [vault.Client.ReadKV] documents `<service>/<inc>/…` as
//     an accepted form and substitutes the mount itself, so the service can land in
//     segs[0] as readily as in segs[1]. Both positions are checked.
//
// Checking segs[0] costs an over-refusal in one configuration — a KV mount named
// exactly after the service — which is the right direction to be wrong in, and the
// diagnostic names the prefix it refused.
//
// `..` is deliberately not handled: both `normalizeLogical` and `relativeKVPath`
// reject it outright, so no such path reaches Vault by any of the channels this fence
// covers, and inventing a meaning for it here would be this gate normalising its
// input rather than mirroring what does.
func PathAddressesOwnNamespace(p, service string) bool {
	if service == "" {
		return false
	}
	p = strings.TrimPrefix(p, vaultRefMarker)
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	raw := strings.Split(p, "/")
	segs := raw[:0]
	for _, seg := range raw {
		if seg == "" || seg == "." {
			continue
		}
		segs = append(segs, seg)
	}
	if len(segs) < 2 {
		return false
	}
	return segs[0] == service || segs[1] == service
}

// ScanOwnNamespaceVault walks a scenario for author-written paths under the
// service's own Vault namespace.
//
// tasks is passed separately from m so the caller decides whether includes are
// already expanded: soul-lint and the render pipeline scan the flat list, the
// artifact loader scans the main file as parsed. m may be nil when only a task list
// is at hand.
func ScanOwnNamespaceVault(file, service string, m *ScenarioManifest, tasks []Task) []diag.Diagnostic {
	if service == "" {
		return nil
	}
	s := &vaultScan{file: file, service: service}
	if m != nil {
		s.scanValue("$.vars", m.Vars, false)
		for i := range m.Compute {
			s.scanValue("$.compute["+strconv.Itoa(i)+"]", m.Compute[i].Value, false)
		}
		for i := range m.Validate {
			s.scanString("$.validate["+strconv.Itoa(i)+"].that", m.Validate[i].That, false)
		}
		s.scanStateChanges(m.StateChanges)
	}
	s.scanTasks(tasks, "$.tasks")
	return s.out
}

// ScanRenderedVaultParams is the post-render half of the fence for the two modules
// that take a Vault path as a parameter. ScanOwnNamespaceVault compares authored
// text, so `path: ${ vars.p }` names no segment it can see; by the time the keeper
// dispatcher holds the task the interpolation is gone and the value IS the path.
// The CEL macro's own evaluated-path guard does not cover these two: they reach
// Vault through the module, not through vault().
//
// Only those two modules are scanned. Elsewhere a rendered string that happens to
// look like a path is just a string, and flagging it would fence `/opt/<service>/`.
func ScanRenderedVaultParams(module, service string, params map[string]any) []diag.Diagnostic {
	if service == "" || len(params) == 0 || !vaultPathModules[module] {
		return nil
	}
	s := &vaultScan{file: module, service: service}
	s.scanValue("$.params", params, true)
	return s.out
}

// vaultScan accumulates diagnostics over one scenario.
type vaultScan struct {
	file    string
	service string
	out     []diag.Diagnostic
}

// add records one offending path. Line/Column stay zero: the scan runs over the
// parsed structure (after include expansion, where the AST of the file that
// actually carried the string is no longer in hand), so the address is the YAML
// path — the same trade-off cross-field invariants in this package already make.
func (s *vaultScan) add(where, path string) {
	s.out = append(s.out, diag.Diagnostic{
		Level:   diag.LevelError,
		Phase:   diag.PhaseSemanticValidate,
		File:    s.file,
		Code:    VaultOwnNamespaceCode,
		Message: "vault path " + strconv.Quote(path) + " addresses the service's own namespace, which the platform derives and owns",
		Hint: "declare the value as a `state_schema` field with `type: secret` and read it back from the register of the `core.state.present` task that writes it ([ADR-083] §1); " +
			"`vault()`, `vault:` refs and `core.vault.*` stay available for paths outside `<mount>/" + s.service + "/`",
		YAMLPath: where,
	})
}

// scanString checks one authored string.
//
// allLiterals distinguishes the two contexts. In a param of a `core.vault.*` module
// the string IS a path (or builds one), so every literal in it is a candidate and a
// quote-free string is checked whole. Anywhere else a path only counts when it
// reaches Vault — through the `vault()` macro or the `vault:` marker — and scanning
// every literal would flag `/opt/<service>/conf` on the way past.
//
// Only the first hit per string is reported: a concatenation carries the prefix in
// one literal, and a second diagnostic at the same address says nothing new.
func (s *vaultScan) scanString(where, raw string, allLiterals bool) {
	if raw == "" {
		return
	}
	if !strings.Contains(raw, "${") && !strings.ContainsAny(raw, `'"`) {
		if allLiterals || strings.HasPrefix(raw, vaultRefMarker) {
			if PathAddressesOwnNamespace(raw, s.service) {
				s.add(where, strings.TrimPrefix(raw, vaultRefMarker))
			}
			return
		}
	}
	if !allLiterals && !exprReadsVault(raw) {
		return
	}
	for _, lit := range celStringLiteral.FindAllString(raw, -1) {
		body := lit[1 : len(lit)-1]
		if PathAddressesOwnNamespace(body, s.service) {
			s.add(where, body)
			return
		}
	}
}

// scanValue walks an opaque YAML value (params / vars / apply.input / loop.items).
// Map keys are visited in sorted order so the diagnostic list is byte-identical
// across runs.
func (s *vaultScan) scanValue(where string, v any, allLiterals bool) {
	switch t := v.(type) {
	case string:
		s.scanString(where, t, allLiterals)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.scanValue(where+"."+k, t[k], allLiterals)
		}
	case []any:
		for i, sub := range t {
			s.scanValue(where+"["+strconv.Itoa(i)+"]", sub, allLiterals)
		}
	}
}

// scanStateChanges walks `state_changes:` in both forms. It is the one authoring
// surface where a fenced path would not merely be READ but written into
// incarnation.state as plaintext — the exact second copy [ADR-0083] exists to
// remove, in the place the ADR calls the source of truth.
func (s *vaultScan) scanStateChanges(sc *StateChanges) {
	if sc == nil {
		return
	}
	if sc.IsList {
		s.scanStateOps(sc.Ops, "$.state_changes")
		return
	}
	keys := make([]string, 0, len(sc.Sets))
	for k := range sc.Sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s.scanString("$.state_changes.sets."+k, sc.Sets[k], false)
	}
}

// scanStateOps walks the list form, recursing through `foreach.do`.
func (s *vaultScan) scanStateOps(ops []StateChange, prefix string) {
	for i := range ops {
		op := &ops[i]
		where := prefix + "[" + strconv.Itoa(i) + "]"
		s.scanValue(where+".value", op.Value, false)
		s.scanString(where+".key", op.Key, false)
		s.scanString(where+".match", op.Match, false)
		s.scanValue(where+".patch", op.Patch, false)
		s.scanString(where+".in", op.In, false)
		s.scanStateOps(op.Do, where+".do")
	}
}

// scanTasks walks the task list, recursing through `block:`. Every field that can
// carry CEL is visited, flow-control included: `${ vault() }` in `when:` never
// resolves (it is evaluated Soul-side, after render), but an author who wrote one
// still wrote the path, and a fence that stayed silent there would teach the path
// is acceptable somewhere.
func (s *vaultScan) scanTasks(tasks []Task, prefix string) {
	for i := range tasks {
		t := &tasks[i]
		where := prefix + "[" + strconv.Itoa(i) + "]"
		s.scanString(where+".when", t.When, false)
		s.scanString(where+".where", t.Where, false)
		s.scanString(where+".changed_when", t.ChangedWhen, false)
		s.scanString(where+".failed_when", t.FailedWhen, false)
		s.scanValue(where+".on", t.On, false)
		s.scanValue(where+".vars", t.Vars, false)
		s.scanValue(where+".output", t.Output, false)
		if t.Loop != nil {
			s.scanString(where+".loop.when", t.Loop.When, false)
			s.scanValue(where+".loop.items", t.Loop.Items, false)
		}
		if t.Retry != nil {
			s.scanString(where+".retry.until", t.Retry.Until, false)
		}
		if t.Assert != nil {
			for j, that := range t.Assert.That {
				s.scanString(where+".assert.that["+strconv.Itoa(j)+"]", that, false)
			}
		}
		if t.Module != nil {
			s.scanValue(where+".params", t.Module.Params, vaultPathModules[t.Module.Module])
		}
		if t.Apply != nil {
			s.scanValue(where+".apply.input", t.Apply.Input, false)
		}
		if t.Block != nil {
			s.scanTasks(t.Block.Block, where+".block")
		}
	}
}
