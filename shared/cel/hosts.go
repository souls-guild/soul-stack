package cel

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/parser"
)

// soulprint.hosts + .where(<predicate>) (slice E3a, [ADR-010]).
//
// Механизм — compile-time AST-rewrite статического литерал-предиката в нативный
// CEL filter-comprehension (НЕ runtime-исполнение строки):
//
//	soulprint.hosts.where("<predicate>")  →  soulprint.hosts.filter(__host, <predicate с полями __host.*>)
//	soulprint.where("<predicate>")        ≡  soulprint.hosts.where("<predicate>")  (синоним)
//
// Ключевой инвариант round-trip'а: rewrite-фаза парсит БЕЗ макросов
// (Engine.noMacroParser), поэтому exists/all/map/filter/exists_one/.where
// остаются plain CallKind и корректно сериализуются parser.Unparse. Если бы
// здесь использовался env.Parse (макросы включены), макросы раскрывались бы в
// ComprehensionKind, который Unparse этой версии cel-go НЕ сериализует
// (opaque-ошибка «unsupported expression»). Финальный env.Compile (макросы
// включены) раскрывает .filter/.exists обратно в comprehension нативно — это
// делает covens.exists(c, c=='db') в предикате полностью рабочим.
//
// Фазы rewriteHostsWhere:
//   - parseNoMacro всего выражения в AST (макросы НЕ раскрываются: .where и
//     exists/all/filter — plain member-call CallKind);
//   - PostOrder-обход дерева: находим call-узлы `where`/1-арг, receiver которых
//     сводится к soulprint.hosts / soulprint.where(...)-проекции;
//   - валидация receiver (только soulprint.hosts/soulprint.where) и аргумента
//     (только string-literal) — иначе понятная compile-ошибка;
//   - строим filter-member-call: receiver.filter(__host, <pred>), где предикат
//     парсится отдельно (тоже без макросов), а его «голые» поля элемента
//     (role/covens/os.family/…) квалифицируются в __host.<поле>; внешний
//     контекст (incarnation.*/input.*/…) и iter-переменные вложенных
//     exists/all/map/filter в предикате не трогаются;
//   - parser.Unparse всего переписанного дерева обратно в строку → env.Compile
//     единожды (Unparse корректно экранирует литералы — инъекция исключена
//     конструктивно, безопаснее ручного RenumberIDs).
//
// [ADR-010]: docs/adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов

// hostIterPrefix — префикс iter-переменной filter-comprehension. Зарезервирован
// в config-валидаторе (loopReservedPrefixes), чтобы loop-переменная `as:`/
// `index_as:` не коллизила с встроенной iter-переменной.
const hostIterPrefix = "__host"

// usesHostsAccessor — грубое распознавание выражений, которые НАДО прогнать
// через AST-проход rewriteHostsWhere: содержащие `soulprint` (hosts/where) или
// любой `.where(` (чтобы generic .where на не-soulprint-receiver получил
// понятную валидационную ошибку, а не невнятное «undeclared reference to where»).
// Точная классификация делается уже по AST; этот тест лишь решает «трогать ли
// выражение парсером» (горячий путь: большинство выражений идут мимо).
func usesHostsAccessor(expr string) bool {
	return strings.Contains(expr, "soulprint") || strings.Contains(expr, ".where(")
}

// rewriteHostsWhere переписывает soulprint.hosts.where(...)/soulprint.where(...)
// в нативный filter-comprehension и возвращает исходную строку CEL. Если
// выражение не использует hosts-аксессор, возвращает expr без изменений
// (горячий путь). allowHosts=false → обращение к soulprint.hosts/soulprint.where
// — ошибка валидации (изоляция destiny-прохода, orchestration.md §4.1).
func (e *Engine) rewriteHostsWhere(expr string, allowHosts bool) (string, error) {
	if !usesHostsAccessor(expr) {
		return expr, nil
	}

	parsed, err := e.parseNoMacro(expr)
	if err != nil {
		return "", &ErrCompile{Expr: expr, Err: err}
	}
	root := parsed.Expr()

	if !containsWhereCall(root) {
		// soulprint.hosts без .where (например, soulprint.hosts.size()).
		if containsHostsAccessor(root) && !allowHosts {
			return "", &ErrUnsupported{Expr: expr, Feature: "soulprint.hosts (scenario-only; недоступен в destiny-проходе)"}
		}
		return expr, nil
	}

	// Есть .where(...). Изоляция: при allowHosts=false обращение к
	// soulprint.hosts/soulprint.where — ошибка (destiny-проход). generic .where
	// на не-soulprint-receiver отвергается ниже понятной ошибкой rewriteWhereCall.
	if !allowHosts && containsHostsAccessor(root) {
		return "", &ErrUnsupported{Expr: expr, Feature: "soulprint.hosts (scenario-only; недоступен в destiny-проходе)"}
	}

	fac := ast.NewExprFactory()
	iter := freeIterVar(e, root)

	rewritten, err := e.rewriteNode(fac, root, iter)
	if err != nil {
		return "", &ErrCompile{Expr: expr, Err: err}
	}

	out, err := parser.Unparse(rewritten, parsed.SourceInfo())
	if err != nil {
		return "", &ErrCompile{Expr: expr, Err: fmt.Errorf("unparse переписанного дерева: %w", err)}
	}
	return out, nil
}

// parseNoMacro парсит выражение парсером БЕЗ макросов: exists/all/map/filter/
// exists_one/.where остаются plain CallKind (round-trip'абельны Unparse'ом).
// Используется только в rewrite-фазе; финальная компиляция идёт через env
// (макросы включены). Ошибка — синтаксическая (понятная, не opaque).
func (e *Engine) parseNoMacro(expr string) (*ast.AST, error) {
	parsed, iss := e.noMacroParser.Parse(common.NewTextSource(expr))
	if iss != nil && len(iss.GetErrors()) > 0 {
		return nil, fmt.Errorf("%s", iss.ToDisplayString())
	}
	return parsed, nil
}

// rewriteNode рекурсивно обходит дерево, переписывая where-вызовы над
// soulprint.hosts/soulprint.where в filter-comprehension. Прочие узлы копируются
// как есть (вложенные where внутри предиката отвергаются отдельно — см.
// buildFilter).
func (e *Engine) rewriteNode(fac ast.ExprFactory, node ast.Expr, iter string) (ast.Expr, error) {
	switch node.Kind() {
	case ast.CallKind:
		c := node.AsCall()
		if c.IsMemberFunction() && c.FunctionName() == "where" {
			return e.rewriteWhereCall(fac, node, iter)
		}
		args := make([]ast.Expr, len(c.Args()))
		for i, a := range c.Args() {
			rw, err := e.rewriteNode(fac, a, iter)
			if err != nil {
				return nil, err
			}
			args[i] = rw
		}
		if c.IsMemberFunction() {
			tgt, err := e.rewriteNode(fac, c.Target(), iter)
			if err != nil {
				return nil, err
			}
			return fac.NewMemberCall(node.ID(), c.FunctionName(), tgt, args...), nil
		}
		return fac.NewCall(node.ID(), c.FunctionName(), args...), nil
	case ast.SelectKind:
		s := node.AsSelect()
		op, err := e.rewriteNode(fac, s.Operand(), iter)
		if err != nil {
			return nil, err
		}
		if s.IsTestOnly() {
			return fac.NewPresenceTest(node.ID(), op, s.FieldName()), nil
		}
		return fac.NewSelect(node.ID(), op, s.FieldName()), nil
	case ast.ListKind:
		l := node.AsList()
		elems := make([]ast.Expr, len(l.Elements()))
		for i, el := range l.Elements() {
			rw, err := e.rewriteNode(fac, el, iter)
			if err != nil {
				return nil, err
			}
			elems[i] = rw
		}
		return fac.NewList(node.ID(), elems, l.OptionalIndices()), nil
	default:
		return fac.CopyExpr(node), nil
	}
}

// rewriteWhereCall переписывает один `<receiver>.where(<arg>)` в
// `<hosts-receiver>.filter(iter, <pred>)`. Валидирует receiver (только
// soulprint.hosts/soulprint.where) и аргумент (только string-literal).
func (e *Engine) rewriteWhereCall(fac ast.ExprFactory, node ast.Expr, iter string) (ast.Expr, error) {
	c := node.AsCall()
	if len(c.Args()) != 1 {
		return nil, fmt.Errorf(".where(...) принимает ровно один аргумент-предикат (получено %d)", len(c.Args()))
	}

	hostsReceiver, err := e.hostsReceiver(fac, c.Target(), iter)
	if err != nil {
		return nil, err
	}

	arg := c.Args()[0]
	if arg.Kind() != ast.LiteralKind {
		return nil, fmt.Errorf(".where(...) predicate must be a static string literal")
	}
	predStr, ok := arg.AsLiteral().Value().(string)
	if !ok {
		return nil, fmt.Errorf(".where(...) predicate must be a static string literal")
	}

	predParsed, err := e.parseNoMacro(predStr)
	if err != nil {
		return nil, fmt.Errorf("predicate %q: %w", predStr, err)
	}
	pred := predParsed.Expr()
	if nestedWhere(pred) {
		return nil, fmt.Errorf("nested .where(...) внутри предиката не поддерживается")
	}

	boundPred := e.qualifyPredicate(fac, pred, iter, nil)
	return fac.NewMemberCall(node.ID(), "filter", hostsReceiver, fac.NewIdent(0, iter), boundPred), nil
}

// hostsReceiver приводит receiver where-вызова к выражению-списку хостов:
//   - soulprint.hosts → как есть;
//   - soulprint (голый ident) → soulprint.where(...) ≡ soulprint.hosts.where(...):
//     receiver сводится к soulprint.hosts;
//   - soulprint.where("<p>") как receiver внешнего .where → раскрывается в filter.
//
// прочие receiver-ы — ошибка (generic .where на произвольном list запрещён).
func (e *Engine) hostsReceiver(fac ast.ExprFactory, recv ast.Expr, iter string) (ast.Expr, error) {
	if isSoulprintHosts(recv) {
		return fac.CopyExpr(recv), nil
	}
	if isSoulprintIdent(recv) {
		// soulprint.where(...) — синоним soulprint.hosts.where(...): receiver
		// внутреннего where = soulprint, подставляем soulprint.hosts.
		return fac.NewSelect(0, fac.NewIdent(0, "soulprint"), "hosts"), nil
	}
	if recv.Kind() == ast.CallKind {
		c := recv.AsCall()
		if c.IsMemberFunction() && c.FunctionName() == "where" {
			// Вложенный where как receiver внешнего .where — раскрываем рекурсивно
			// (он сам обязан сводиться к soulprint.hosts / soulprint).
			return e.rewriteWhereCall(fac, recv, iter)
		}
	}
	return nil, fmt.Errorf(".where(...) разрешён только на soulprint.hosts / soulprint.where(...); generic .where на произвольном списке запрещён")
}

// qualifyPredicate квалифицирует «голые» поля элемента в предикате (role,
// covens, os.family, …) в iter.<поле>; идентификаторы известного внешнего
// контекста (incarnation/input/register/soulprint/essence/vars + сама
// iter-переменная) не трогаются. Так предикат внутри filter ссылается на поля
// текущего элемента __host.*, а внешний контекст резолвится из активации.
//
// bound — iter-переменные вложенных comprehension-макросов
// (covens.exists(c, …)/role.all(…)/…), объявленные первым аргументом макроса.
// Внутри тела такого макроса они НЕ квалифицируются (это локальные переменные
// макроса, а не поля элемента). Поскольку rewrite-фаза парсит без макросов,
// макрос здесь — обычный member-call: receiver квалифицируем как поле элемента
// (covens → __host.covens), а тело — с дополнительно связанной переменной.
func (e *Engine) qualifyPredicate(fac ast.ExprFactory, node ast.Expr, iter string, bound map[string]bool) ast.Expr {
	switch node.Kind() {
	case ast.IdentKind:
		name := node.AsIdent()
		if name == iter || predicateContextRoots[name] || bound[name] {
			return fac.CopyExpr(node)
		}
		return fac.NewSelect(node.ID(), fac.NewIdent(0, iter), name)
	case ast.SelectKind:
		s := node.AsSelect()
		op := e.qualifyPredicate(fac, s.Operand(), iter, bound)
		if s.IsTestOnly() {
			return fac.NewPresenceTest(node.ID(), op, s.FieldName())
		}
		return fac.NewSelect(node.ID(), op, s.FieldName())
	case ast.CallKind:
		c := node.AsCall()
		if macroVar, body, ok := comprehensionMacro(c); ok {
			// Тело макроса видит свою iter-переменную как локальную: связываем
			// её на время обхода body-аргументов.
			inner := withBound(bound, macroVar)
			args := make([]ast.Expr, len(c.Args()))
			args[0] = fac.CopyExpr(c.Args()[0]) // iter-var ident — оставляем как есть
			for _, i := range body {
				args[i] = e.qualifyPredicate(fac, c.Args()[i], iter, inner)
			}
			return fac.NewMemberCall(node.ID(), c.FunctionName(), e.qualifyPredicate(fac, c.Target(), iter, bound), args...)
		}
		args := make([]ast.Expr, len(c.Args()))
		for i, a := range c.Args() {
			args[i] = e.qualifyPredicate(fac, a, iter, bound)
		}
		if c.IsMemberFunction() {
			return fac.NewMemberCall(node.ID(), c.FunctionName(), e.qualifyPredicate(fac, c.Target(), iter, bound), args...)
		}
		return fac.NewCall(node.ID(), c.FunctionName(), args...)
	case ast.ListKind:
		l := node.AsList()
		elems := make([]ast.Expr, len(l.Elements()))
		for i, el := range l.Elements() {
			elems[i] = e.qualifyPredicate(fac, el, iter, bound)
		}
		return fac.NewList(node.ID(), elems, l.OptionalIndices())
	default:
		return fac.CopyExpr(node)
	}
}

// comprehensionMacro распознаёт receiver-style вызов comprehension-макроса
// (exists/all/exists_one/existsOne/map/filter), первый аргумент которого —
// ident, объявляющий iter-переменную макроса. Возвращает имя этой переменной и
// индексы body-аргументов (всё кроме первого: предикат/трансформ). Эти макросы
// при rewrite-парсе (без макросов) — обычные member-call'ы; финальный
// env.Compile раскроет их нативно.
func comprehensionMacro(c ast.CallExpr) (string, []int, bool) {
	if !c.IsMemberFunction() {
		return "", nil, false
	}
	switch c.FunctionName() {
	case "exists", "all", "exists_one", "existsOne", "filter":
		if len(c.Args()) != 2 || c.Args()[0].Kind() != ast.IdentKind {
			return "", nil, false
		}
		return c.Args()[0].AsIdent(), []int{1}, true
	case "map":
		// map(v, transform) или map(v, filter, transform).
		if (len(c.Args()) == 2 || len(c.Args()) == 3) && c.Args()[0].Kind() == ast.IdentKind {
			body := make([]int, 0, len(c.Args())-1)
			for i := 1; i < len(c.Args()); i++ {
				body = append(body, i)
			}
			return c.Args()[0].AsIdent(), body, true
		}
	}
	return "", nil, false
}

// withBound возвращает копию bound с добавленным name (немутирующая, чтобы
// связывание не утекало в соседние ветви обхода).
func withBound(bound map[string]bool, name string) map[string]bool {
	out := make(map[string]bool, len(bound)+1)
	for k := range bound {
		out[k] = true
	}
	out[name] = true
	return out
}

// predicateContextRoots — корневые идентификаторы внешнего контекста, которые
// внутри предиката .where(...) НЕ квалифицируются в iter.<…> (они резолвятся из
// активации). Симметрично contextVars. iter-переменная добавляется отдельно.
var predicateContextRoots = map[string]bool{
	"input":       true,
	"register":    true,
	"incarnation": true,
	"soulprint":   true,
	"essence":     true,
	"vars":        true,
}

// containsHostsAccessor сообщает, использует ли дерево soulprint.hosts или
// soulprint.where(...) хоть где-то (для гейта изоляции destiny-прохода).
func containsHostsAccessor(root ast.Expr) bool {
	found := false
	ast.PostOrderVisit(root, ast.NewExprVisitor(func(e ast.Expr) {
		if isSoulprintHosts(e) {
			found = true
			return
		}
		if e.Kind() == ast.CallKind {
			c := e.AsCall()
			if c.IsMemberFunction() && c.FunctionName() == "where" && isSoulprintIdent(c.Target()) {
				found = true
			}
		}
	}))
	return found
}

// containsWhereCall сообщает, есть ли в дереве хоть один member-call `where`.
func containsWhereCall(root ast.Expr) bool {
	found := false
	ast.PostOrderVisit(root, ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() == ast.CallKind {
			c := e.AsCall()
			if c.IsMemberFunction() && c.FunctionName() == "where" {
				found = true
			}
		}
	}))
	return found
}

// nestedWhere — есть ли вложенный where-вызов где-либо в дереве предиката.
func nestedWhere(pred ast.Expr) bool {
	found := false
	ast.PostOrderVisit(pred, ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() == ast.CallKind {
			c := e.AsCall()
			if c.IsMemberFunction() && c.FunctionName() == "where" {
				found = true
			}
		}
	}))
	return found
}

// isSoulprintHosts — узел вида soulprint.hosts (Select hosts на ident soulprint).
func isSoulprintHosts(e ast.Expr) bool {
	if e.Kind() != ast.SelectKind {
		return false
	}
	s := e.AsSelect()
	return !s.IsTestOnly() && s.FieldName() == "hosts" && isSoulprintIdent(s.Operand())
}

// isSoulprintIdent — узел-идентификатор `soulprint`.
func isSoulprintIdent(e ast.Expr) bool {
	return e.Kind() == ast.IdentKind && e.AsIdent() == "soulprint"
}

// freeIterVar возвращает гарантированно-свободное имя iter-переменной для
// filter-comprehension: hostIterPrefix, либо hostIterPrefix+счётчик, если
// базовое имя встречается среди идентификаторов/полей выражения ИЛИ внутри
// строковых литералов where-предикатов (коллизия — например автор использовал
// `__host` как имя поля элемента или как идентификатор контекста). Литералы
// предикатов парсятся отдельно: их идентификаторы невидимы во внешнем AST.
func freeIterVar(e *Engine, root ast.Expr) string {
	used := map[string]bool{}
	collect := func(node ast.Expr) {
		ast.PostOrderVisit(node, ast.NewExprVisitor(func(n ast.Expr) {
			switch n.Kind() {
			case ast.IdentKind:
				used[n.AsIdent()] = true
			case ast.SelectKind:
				used[n.AsSelect().FieldName()] = true
			case ast.CallKind:
				// Содержимое where-предиката (string-литерал) парсим, чтобы учесть
				// его идентификаторы при выборе свободной iter-переменной.
				c := n.AsCall()
				if c.IsMemberFunction() && c.FunctionName() == "where" && len(c.Args()) == 1 && c.Args()[0].Kind() == ast.LiteralKind {
					if s, ok := c.Args()[0].AsLiteral().Value().(string); ok {
						if pp, err := e.parseNoMacro(s); err == nil {
							ast.PostOrderVisit(pp.Expr(), ast.NewExprVisitor(func(p ast.Expr) {
								switch p.Kind() {
								case ast.IdentKind:
									used[p.AsIdent()] = true
								case ast.SelectKind:
									used[p.AsSelect().FieldName()] = true
								}
							}))
						}
					}
				}
			}
		}))
	}
	collect(root)

	if !used[hostIterPrefix] {
		return hostIterPrefix
	}
	for i := 0; ; i++ {
		cand := fmt.Sprintf("%s%d", hostIterPrefix, i)
		if !used[cand] {
			return cand
		}
	}
}
