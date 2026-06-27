package tmpl

import (
	"text/template"
	"text/template/parse"
)

// UsesRootField сообщает, ссылается ли шаблон на корневое поле dot-контекста
// `.<field>` (например, `.input`) — реальным action-обращением, а НЕ упоминанием
// в литеральном тексте/комментарии тела ([ADR-010] §3.2, условная инъекция
// render_context.input, Вариант B).
//
// Используется Keeper-side рендером core.file.rendered: ключ `input` кладётся в
// render_context ТОЛЬКО когда шаблон реально читает `.input.*` — иначе
// render_context остаётся `{vars,self,role,essence}` как до Варианта B (шаблоны
// на одних `.vars` не получают лишнего `input`, deep-equal-фикстуры стабильны).
//
// Детекция — по AST, не по string-search: `text/template/parse` отделяет
// action-узлы внутри `{{…}}` от литерального текста (TextNode). Поэтому
// `# ... apply.input ...` в комментарии redis.conf.tmpl — TextNode → НЕ обращение;
// `{{ .input.user }}` — FieldNode с Ident[0]=="input" → обращение. range/if/with/
// pipeline-аргументы/вложенные define обходятся рекурсивно.
//
// Распознаётся прямая dot-форма (FieldNode `.field.sub`). Формы через переменную
// (`{{ $.field }}` — VariableNode, `{{ $x := .field }}…{{ $x.sub }}`) и chain через
// builtin (`index .field "sub"` даёт голый FieldNode `.field` без подключа) не
// разворачиваются — fail-closed: input просто не инъектится, шаблон упадёт
// strict-mode явно (не утечка). В пилот-шаблонах эти формы для корневого `.input`
// не встречаются.
//
// Парсинг идёт с тем же FuncMap, что и Render (e.funcs) — кастомные/sprig-функции
// шаблона известны парсеру, парс не падает на легальном вызове. Ошибка парсинга
// (битый шаблон) возвращается как ErrParse — caller падает так же, как упал бы на
// Render (расхождения поведения нет).
func (e *Engine) UsesRootField(templateContent, field string) (bool, error) {
	t := template.New("usesfield").Funcs(e.funcs)
	t, err := t.Parse(templateContent)
	if err != nil {
		return false, &ErrParse{Err: err}
	}
	// Parse регистрирует и корневой шаблон, и все `define`-вложенные —
	// обходим каждый (define-тело может читать .input через `{{ template … . }}`).
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

// RootFieldSubKeys возвращает множество вторых идентификаторов цепочек
// `.<field>.<subkey>`, к которым шаблон реально обращается action-узлом (а не
// упоминанием в литеральном тексте). Например, для field=="vars" и шаблона
// `ExecStart={{ .vars.bin_path }}` → {"bin_path"}.
//
// Используется Keeper-side рендером core.file.rendered для ТОЧЕЧНОЙ инъекции
// destiny-локалов (vars.yml) в render_context.vars: в `.vars` подкладываются
// ТОЛЬКО те file-vars, чей ключ шаблон реально читает как `.vars.<key>` — а не весь
// набор vars.yml. Так file-var доезжает в шаблон напрямую (node-exporter:
// `.vars.bin_path`), но шаблоны, читающие лишь task-var-ключи (redis:
// `.vars.password`/`.vars.config` — это params.vars, не file-vars), НЕ получают
// лишних file-var-ключей → их render_context.vars остаётся БИТ-В-БИТ как был
// (deep-equal-фикстуры стабильны). Симметрия с UsesRootField (Вариант B для input):
// инъектим ровно то, что шаблон читает, не больше.
//
// Детекция — по AST (та же машинерия, что UsesRootField): FieldNode с
// Ident=[field, subkey, …] → subkey попадает в множество. Голое `.<field>` без
// подключа (Ident=[field]) подключей не даёт. Ошибка парсинга → ErrParse (caller
// падает как на Render).
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

// collectSubKeys рекурсивно обходит parse-AST и собирает вторые идентификаторы
// цепочек `.<field>.<subkey>` (та же структура обхода, что walkUsesRootField).
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

// collectSubKeysBranch обходит общую часть if/range/with (BranchNode).
func collectSubKeysBranch(b *parse.BranchNode, field string, keys map[string]bool) {
	collectSubKeys(b.Pipe, field, keys)
	if b.List != nil {
		collectSubKeys(b.List, field, keys)
	}
	if b.ElseList != nil {
		collectSubKeys(b.ElseList, field, keys)
	}
}

// walkUsesRootField рекурсивно обходит parse-AST в поисках обращения к корневому
// полю `.<field>`. Признак — FieldNode, чей первый идентификатор == field
// (FieldNode представляет цепочку `.a.b.c` от dot-контекста: Ident=["a","b","c"]).
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
		// `{{ template "name" .input.x }}` — поле читается в аргументе шаблона.
		return walkUsesRootField(n.Pipe, field)
	}
	return false
}

// walkBranch обходит общую часть if/range/with (BranchNode): управляющий pipe +
// тело + else-ветка.
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
