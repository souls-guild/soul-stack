package cel

import (
	"fmt"

	"github.com/google/cel-go/common/ast"
)

// ErrVarIndexForm — интерполяция обращается к слою vars индекс-формой
// (`vars['k']` / `vars[expr]`) вместо select-формы (`vars.k`). Для построения
// графа зависимостей var→var (resolveVarLayer) имя ключа должно быть статически
// известно из AST; индекс-форма с произвольным выражением (в т.ч. динамическим
// `vars[input.x]`) этого не гарантирует. Детерминированно отвергаем её ЦЕЛИКОМ,
// а не пытаемся вытащить строковый литерал из части случаев: единая граница
// проще и предсказуемее для автора (canonical-форма vars.* — единственная, как и
// soulprint.self.<path> в ADR-018). Отдельный sentinel — caller отличает его от
// синтаксической ErrCompile через errors.Is.
var ErrVarIndexForm = fmt.Errorf("обращение к vars индекс-формой vars[...] не поддерживается — используй vars.<имя>")

// VarRefs извлекает имена `vars.<X>`, на которые ссылается интерполяционная
// строка raw (`${ … }`-блоки). Зеркало механизма DetectSealed (seal.go):
// scanInterpolation → per-block parseNoMacro → PostOrderVisit → собрать
// selectBaseField, где base=="vars". Это AST-обход, не regex: `vars.x` в
// литеральном тексте ВНЕ `${ … }` ссылкой не считается, а `vars` внутри строкового
// литерала CEL (`"vars.x"`) — тоже нет (это StringConstant, не Select).
//
// Возврат — имена в порядке первого появления при PostOrderVisit (дедуплицируются);
// порядок не значим для caller-а (resolveVarLayer строит граф). raw без `${ … }`
// или без vars-ссылок → пустой срез, nil-ошибка.
//
// Index-форма `vars['k']` / `vars[expr]` → [ErrVarIndexForm] (детерминированно,
// см. её док): имя ключа не извлекается из AST единообразно.
//
// Синтаксически-битый блок до per-block parseNoMacro не доходит: scanInterpolation
// (parseBlock) сам гейтит границу блока через env.Parse и на невалидном выражении
// возвращает *ErrCompile раньше. `continue` на perr ниже — защитный (зеркало
// DetectSealed, на случай ослабления parseBlock или прямого вызова): VarRefs не
// дублирует валидацию, а собирает ссылки у разбираемых выражений.
func (e *Engine) VarRefs(raw string) ([]string, error) {
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		return nil, err
	}
	var refs []string
	seen := map[string]bool{}
	for _, s := range segs {
		if !s.expr {
			continue
		}
		parsed, perr := e.parseNoMacro(s.text)
		if perr != nil {
			continue // битый CEL — не наша забота (зеркало DetectSealed)
		}
		var visitErr error
		ast.PostOrderVisit(parsed.Expr(), ast.NewExprVisitor(func(n ast.Expr) {
			if visitErr != nil {
				return
			}
			switch n.Kind() {
			case ast.SelectKind:
				if base, field, ok := selectBaseField(n); ok && base == "vars" {
					if !seen[field] {
						seen[field] = true
						refs = append(refs, field)
					}
				}
			case ast.CallKind:
				if isVarsIndex(n) {
					visitErr = fmt.Errorf("%w (выражение %q)", ErrVarIndexForm, s.text)
				}
			}
		}))
		if visitErr != nil {
			return nil, visitErr
		}
	}
	return refs, nil
}

// isVarsIndex — узел вида `vars[<expr>]` (CEL index-оператор `_[_]` над голым
// идентификатором `vars`). cel-go представляет `a[b]` глобальным вызовом с
// FunctionName == operators.Index и двумя аргументами; первый аргумент —
// IdentKind `vars`. Член-вызов (`x[y]()`) исключён формой узла.
func isVarsIndex(n ast.Expr) bool {
	c := n.AsCall()
	if c.IsMemberFunction() || c.FunctionName() != "_[_]" {
		return false
	}
	args := c.Args()
	if len(args) == 0 {
		return false
	}
	op := args[0]
	return op.Kind() == ast.IdentKind && op.AsIdent() == "vars"
}
