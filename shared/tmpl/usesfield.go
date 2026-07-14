package tmpl

import (
	"text/template"
	"text/template/parse"
)

// UsesRootField reports whether the template references the root dot-context field
// `.<field>` (e.g. `.input`) via a real action, NOT by a mention in literal body
// text/comment ([ADR-010] §3.2, conditional injection of render_context.input,
// Variant B).
//
// Used by the Keeper-side core.file.rendered renderer: the `input` key is put into
// render_context ONLY when the template actually reads `.input.*` — otherwise
// render_context stays `{vars,self,role,essence}` as before Variant B (templates on
// `.vars` alone get no extra `input`, deep-equal fixtures stay stable).
//
// Detection is by AST, not string-search: `text/template/parse` separates action
// nodes inside `{{…}}` from literal text (TextNode). So `# ... apply.input ...` in a
// redis.conf.tmpl comment is a TextNode → NOT a reference; `{{ .input.user }}` is a
// FieldNode with Ident[0]=="input" → a reference. range/if/with/pipeline-args/nested
// define are walked recursively.
//
// The direct dot form (FieldNode `.field.sub`) is recognized. Forms via a variable
// (`{{ $.field }}` — VariableNode, `{{ $x := .field }}…{{ $x.sub }}`) and a chain via
// a builtin (`index .field "sub"` yields a bare FieldNode `.field` without a subkey)
// are not unrolled — fail-closed: input is simply not injected, the template fails
// strict-mode explicitly (not a leak). These forms for the root `.input` do not occur
// in the pilot templates.
//
// Parsing uses the same FuncMap as Render (e.funcs) — the template's custom/sprig
// functions are known to the parser, parsing does not fail on a legal call. A parse
// error (broken template) is returned as ErrParse — the caller fails the same as it
// would on Render (no behavior divergence).
func (e *Engine) UsesRootField(templateContent, field string) (bool, error) {
	t := template.New("usesfield").Funcs(e.funcs)
	t, err := t.Parse(templateContent)
	if err != nil {
		return false, &ErrParse{Err: err}
	}
	// Parse registers both the root template and all `define`-nested ones — walk
	// each (a define body may read .input via `{{ template … . }}`).
	for _, tmpl := range t.Templates() {
		if tmpl.Tree == nil || tmpl.Tree.Root == nil {
			continue
		}
		if walkUsesRootField(tmpl.Tree.Root, field) {
			return true, nil
		}
	}
	return false, nil
}

// RootFieldSubKeys returns the set of second identifiers of `.<field>.<subkey>`
// chains that the template actually accesses via an action node (not by a mention in
// literal text). E.g. for field=="vars" and the template
// `ExecStart={{ .vars.bin_path }}` → {"bin_path"}.
//
// Used by the Keeper-side core.file.rendered renderer for TARGETED injection of
// destiny locals (vars.yml) into render_context.vars: `.vars` gets ONLY those
// file-vars whose key the template actually reads as `.vars.<key>` — not the whole
// vars.yml set. So a file-var reaches the template directly (node-exporter:
// `.vars.bin_path`), but templates reading only task-var keys (redis:
// `.vars.password`/`.vars.config` — those are params.vars, not file-vars) get no
// extra file-var keys → their render_context.vars stays BIT-FOR-BIT as it was
// (deep-equal fixtures stable). Symmetric to UsesRootField (Variant B for input):
// inject exactly what the template reads, no more.
//
// Detection is by AST (the same machinery as UsesRootField): a FieldNode with
// Ident=[field, subkey, …] → subkey joins the set. A bare `.<field>` without a subkey
// (Ident=[field]) contributes no subkeys. A parse error → ErrParse (the caller fails
// as on Render).
func (e *Engine) RootFieldSubKeys(templateContent, field string) (map[string]bool, error) {
	t := template.New("subkeys").Funcs(e.funcs)
	t, err := t.Parse(templateContent)
	if err != nil {
		return nil, &ErrParse{Err: err}
	}
	keys := map[string]bool{}
	for _, tmpl := range t.Templates() {
		if tmpl.Tree == nil || tmpl.Tree.Root == nil {
			continue
		}
		collectSubKeys(tmpl.Tree.Root, field, keys)
	}
	return keys, nil
}

// collectSubKeys recursively walks the parse AST and collects the second identifiers
// of `.<field>.<subkey>` chains (same walk structure as walkUsesRootField).
func collectSubKeys(node parse.Node, field string, keys map[string]bool) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, child := range n.Nodes {
			collectSubKeys(child, field, keys)
		}
	case *parse.ActionNode:
		collectSubKeys(n.Pipe, field, keys)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, cmd := range n.Cmds {
			collectSubKeys(cmd, field, keys)
		}
	case *parse.CommandNode:
		for _, arg := range n.Args {
			collectSubKeys(arg, field, keys)
		}
	case *parse.FieldNode:
		if len(n.Ident) >= 2 && n.Ident[0] == field {
			keys[n.Ident[1]] = true
		}
	case *parse.IfNode:
		collectSubKeysBranch(&n.BranchNode, field, keys)
	case *parse.RangeNode:
		collectSubKeysBranch(&n.BranchNode, field, keys)
	case *parse.WithNode:
		collectSubKeysBranch(&n.BranchNode, field, keys)
	case *parse.TemplateNode:
		collectSubKeys(n.Pipe, field, keys)
	}
}

// collectSubKeysBranch walks the common part of if/range/with (BranchNode).
func collectSubKeysBranch(b *parse.BranchNode, field string, keys map[string]bool) {
	collectSubKeys(b.Pipe, field, keys)
	if b.List != nil {
		collectSubKeys(b.List, field, keys)
	}
	if b.ElseList != nil {
		collectSubKeys(b.ElseList, field, keys)
	}
}

// walkUsesRootField recursively walks the parse AST looking for a reference to the
// root field `.<field>`. The marker is a FieldNode whose first identifier == field
// (a FieldNode represents the chain `.a.b.c` from the dot-context: Ident=["a","b","c"]).
func walkUsesRootField(node parse.Node, field string) bool {
	if node == nil {
		return false
	}
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return false
		}
		for _, child := range n.Nodes {
			if walkUsesRootField(child, field) {
				return true
			}
		}
	case *parse.ActionNode:
		return walkUsesRootField(n.Pipe, field)
	case *parse.PipeNode:
		if n == nil {
			return false
		}
		for _, cmd := range n.Cmds {
			if walkUsesRootField(cmd, field) {
				return true
			}
		}
	case *parse.CommandNode:
		for _, arg := range n.Args {
			if walkUsesRootField(arg, field) {
				return true
			}
		}
	case *parse.FieldNode:
		return len(n.Ident) > 0 && n.Ident[0] == field
	case *parse.IfNode:
		return walkBranch(&n.BranchNode, field)
	case *parse.RangeNode:
		return walkBranch(&n.BranchNode, field)
	case *parse.WithNode:
		return walkBranch(&n.BranchNode, field)
	case *parse.TemplateNode:
		// `{{ template "name" .input.x }}` — the field is read in the template argument.
		return walkUsesRootField(n.Pipe, field)
	}
	return false
}

// walkBranch walks the common part of if/range/with (BranchNode): the control pipe +
// body + else branch.
func walkBranch(b *parse.BranchNode, field string) bool {
	if walkUsesRootField(b.Pipe, field) {
		return true
	}
	if b.List != nil && walkUsesRootField(b.List, field) {
		return true
	}
	if b.ElseList != nil && walkUsesRootField(b.ElseList, field) {
		return true
	}
	return false
}
