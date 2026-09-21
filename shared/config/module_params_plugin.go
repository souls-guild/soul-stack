package config

import (
	"fmt"
	"strconv"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// ModuleManifestResolver answers "what does module `<alias>.<name>` accept", for every
// module this binary does not carry compiled in (NIM-228).
//
// alias is address level 1. Since NIM-377 it is the registration alias an operator
// chose, not a publisher the artifact declared — the same bytes registered as `redis`
// and as `redis-community` answer under two aliases and one schema.
//
// The two implementations differ in what they can see, which is the whole point:
//
//   - keeper resolves from its Sigil grants, which hold the byte-exact schema document
//     the signature was verified over — i.e. the very contract that will gate the task
//     at run time under ADR-0076(t);
//   - soul-lint resolves from the schema documents an author binds on the command line
//     (`--modules <alias>=<path>`), so someone working offline in a checkout that
//     carries both halves gets the same four checks.
//
// ok=false means "this resolver cannot see that module" and is NOT an error: the
// caller reports it as [DiagPluginParamsUnchecked] so the gap is visible.
//
// A resolver is never asked about a reserved alias (`core`, `keeper`, …): those are
// answered by the compiled-in registry or by nobody. See [reservedModuleAddr].
type ModuleManifestResolver interface {
	ResolveModule(alias, name string) (plugin.ModuleDef, bool)
}

// DiagPluginParamsUnchecked marks a plugin module whose manifest did not
// resolve, so its `params:` were not checked at all.
//
// A HINT, never an error. The author of a definition usually cannot produce the
// manifest of somebody else's plugin, so failing them for its absence would
// punish the wrong person; and it cannot be a silent pass either, because that
// is the defect — NIM-206's drift survived precisely because "no diagnostics"
// read as "checked and clean".
const DiagPluginParamsUnchecked = "plugin_params_unchecked"

// validatePluginModuleParams walks an already-parsed document and checks the
// `params:` of every PLUGIN module task it finds against the resolved manifest.
//
// Deliberately a post-pass over the AST rather than part of task decoding, where
// the `core` check lives. The core registry is compiled in, so it is available at
// the moment a task is decoded; a plugin manifest arrives with the CALLER, and
// [ValidateOptions] reaches the parse entry points but not `UnmarshalYAML`.
//
// The walk knows nothing about task grammar: any mapping carrying a `module:`
// string is a module task, wherever it sits. That is on purpose — keying off
// `tasks:` / `block:` / `apply:` would silently stop checking the day the grammar
// grows another construct that holds tasks.
func validatePluginModuleParams(root ast.Node, r ModuleManifestResolver) []diag.Diagnostic {
	if root == nil {
		return nil
	}
	w := pluginParamWalk{resolver: r, reported: map[string]bool{}}
	w.walk(root, "$")
	return w.out
}

type pluginParamWalk struct {
	resolver ModuleManifestResolver
	// reported dedupes the unchecked hint by module ADDRESS: one unresolvable
	// module used by twenty tasks is one thing to fix, and twenty identical
	// hints teach the reader to skip them.
	reported map[string]bool
	out      []diag.Diagnostic
}

func (w *pluginParamWalk) walk(n ast.Node, path string) {
	switch node := n.(type) {
	case *ast.MappingNode:
		w.visitMapping(node, path)
		for _, kv := range node.Values {
			w.walk(kv.Value, path+"."+keyName(kv))
		}
	case *ast.MappingValueNode:
		// A single-pair mapping is parsed as MappingValueNode, not MappingNode.
		w.visitMapping(&ast.MappingNode{Values: []*ast.MappingValueNode{node}}, path)
		w.walk(node.Value, path+"."+keyName(node))
	case *ast.SequenceNode:
		for i, item := range node.Values {
			w.walk(item, path+"["+strconv.Itoa(i)+"]")
		}
	}
}

// visitMapping handles one mapping that may be a module task.
func (w *pluginParamWalk) visitMapping(mm *ast.MappingNode, path string) {
	var moduleKV, paramsKV *ast.MappingValueNode
	for _, kv := range mm.Values {
		switch keyName(kv) {
		case "module":
			moduleKV = kv
		case "params":
			paramsKV = kv
		}
	}
	if moduleKV == nil {
		return
	}
	sn, ok := moduleKV.Value.(*ast.StringNode)
	if !ok {
		return // module: is not a string — validateModuleField already said so.
	}
	ns, mod, state, ok := splitModuleAddress(sn.Value)
	if !ok {
		// Malformed addresses yield module_format_invalid elsewhere.
		return
	}
	address := ns + "." + mod

	// The built-in bypass keys on the REGISTRY, not on the namespace string.
	//
	// `ns == "core"` was a safe test only while a namespace meant a publisher and
	// `core` was unforgeable. Address level 1 is now an alias an operator picks at
	// registration, so that test would hand any plugin registered as `core` the
	// branch reserved for built-ins — checked by nobody, reported by nobody, and
	// silent about it. Asking the compiled-in registry whether it actually serves
	// this address cannot be spoofed by a name.
	if _, builtin := coremanifest.Default().Lookup(address); builtin {
		// Compiled in, therefore already checked at decode by validateModuleParams
		// against the very same manifest.
		return
	}
	if plugin.IsReserved(ns) {
		// A reserved name the built-in registry does not serve. No resolver may
		// answer for it: a `--modules core=…` binding, or a grant a cluster
		// accepted under a reserved alias, would otherwise get to define what
		// `core.*` accepts. Not an error — the definition may simply target an
		// engine newer than this one (ADR-0076) — but never a silent pass either.
		if !w.reported[address] {
			w.reported[address] = true
			w.out = append(w.out, w.reservedDiag(address, ns, sn, path))
		}
		return
	}

	man, resolved := w.resolveModule(ns, mod)
	if !resolved {
		if !w.reported[address] {
			w.reported[address] = true
			w.out = append(w.out, w.uncheckedDiag(address, sn, path))
		}
		return
	}

	def, hasState := man.States[state]
	if !hasState {
		tok := sn.GetToken()
		w.out = append(w.out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "module_state_unknown",
			Message:  fmt.Sprintf("%s has no state %q", address, state),
			Hint:     "see the module's schema states for valid states",
			YAMLPath: path + ".module",
		}))
		return
	}

	var paramsNode *ast.MappingNode
	if paramsKV != nil {
		if pm, isMap := paramsKV.Value.(*ast.MappingNode); isMap {
			paramsNode = pm
		} else if pv, isPair := paramsKV.Value.(*ast.MappingValueNode); isPair {
			paramsNode = &ast.MappingNode{Values: []*ast.MappingValueNode{pv}}
		}
	}
	// Same four checks as core, same functions — the rules are the module's, not
	// the namespace's, and one implementation is what keeps them from diverging.
	w.out = append(w.out, checkUnknownAndType(def, paramsNode, path)...)
	w.out = append(w.out, checkRequired(def, paramsNode, moduleKV, path)...)
}

func (w *pluginParamWalk) resolveModule(ns, mod string) (plugin.ModuleDef, bool) {
	if w.resolver == nil {
		return plugin.ModuleDef{}, false
	}
	return w.resolver.ResolveModule(ns, mod)
}

// reservedDiag is the unchecked hint for a reserved namespace this engine does not
// serve. Deliberately the same code as the ordinary miss — the fact being reported is
// the same one ("these params were not checked") and an operator filtering on it must
// see both — with a message that does not send the author looking for a plugin manifest
// they could never legally supply.
func (w *pluginParamWalk) reservedDiag(address, ns string, sn *ast.StringNode, path string) diag.Diagnostic {
	line, col := 0, 0
	if tok := sn.GetToken(); tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	return diagAt(line, col, diag.Diagnostic{
		Level: diag.LevelHint, Phase: diag.PhaseSemanticValidate,
		Code: DiagPluginParamsUnchecked,
		Message: fmt.Sprintf("params of %s were not checked: %q is a reserved name and this engine serves no built-in module %s",
			address, ns, address),
		Hint: "a reserved name cannot be supplied by a plugin, so --modules cannot fill this gap; " +
			"check the address, or run an engine release that ships the module",
		YAMLPath: path + ".module",
	})
}

func (w *pluginParamWalk) uncheckedDiag(address string, sn *ast.StringNode, path string) diag.Diagnostic {
	hint := "supply the plugin's manifest to check it: soul-lint --modules <dir>, " +
		"or lint through a keeper that has the plugin allow-listed"
	msg := fmt.Sprintf("params of %s were not checked: no manifest for this module is available here", address)
	if w.resolver == nil {
		msg = fmt.Sprintf("params of %s were not checked: no module manifests were supplied", address)
	}
	line, col := 0, 0
	if tok := sn.GetToken(); tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	return diagAt(line, col, diag.Diagnostic{
		Level: diag.LevelHint, Phase: diag.PhaseSemanticValidate,
		Code:     DiagPluginParamsUnchecked,
		Message:  msg,
		Hint:     hint,
		YAMLPath: path + ".module",
	})
}

// keyName returns a mapping pair's key as text ("" when it has no token).
func keyName(kv *ast.MappingValueNode) string {
	if kv == nil || kv.Key == nil {
		return ""
	}
	if tok := kv.Key.GetToken(); tok != nil {
		return tok.Value
	}
	return ""
}
