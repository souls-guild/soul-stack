package cel

import (
	"github.com/google/cel-go/common/ast"
)

// seal / sealed-paths ([ADR-010] §7.4, метафора «запечатанные пути»): механика
// render-time provenance/taint. Ячейка params помечается sealed, когда её
// CEL-выражение ЧИТАЕТ secret-источник:
//
//   - input.<name>, где <name> объявлен secret:true в активной input-схеме
//     прохода (scenario-input на scenario, destiny-input на destiny);
//   - vault(...) — чтение Vault KV;
//   - транзитивно — vars.<x>/compute.<x>, чьё значение само sealed (их выражения
//     уже прошли детекцию на своей фазе резолва).
//
// Детекция — whole-cell: достаточно одной ветки выражения, читающей секрет
// (тернарник `has(input.tls_cert) ? input.tls_cert : ”` — обе ветки в обходе),
// чтобы вся ячейка стала sealed (whole-value taint, безопасно: смешение
// literal+secret даёт sealed результат). Это AST-обход, не single-ident-матч.
//
// SealSources — что считать секрет-источником при обходе одного выражения.
// Пустой набор secret-input при пустых var/compute → detect ловит только vault().
type SealSources struct {
	// SecretInputs — имена input-параметров, объявленных secret:true в активной
	// схеме прохода. `input.<name>` для name из набора → ячейка sealed.
	SecretInputs map[string]bool

	// SealedVars / SealedCompute — имена vars.*/compute.*, чьё ЗНАЧЕНИЕ уже
	// признано sealed (транзитивность): их собственное выражение прочитало секрет
	// на фазе резолва vars/compute. `vars.<x>`/`compute.<x>` для x из набора →
	// ячейка sealed.
	SealedVars    map[string]bool
	SealedCompute map[string]bool
}

// vaultMacroName / vaultExpandedName — имена, под которыми vault() появляется в
// AST. До macro-expansion (parseNoMacro) — `vault` plain-call; после env.Parse —
// `__vault_read` (см. vault.go expandVaultMacro). Детектор парсит без макросов
// (parseNoMacro), поэтому ловит `vault`; `__vault_read` держим на случай уже-
// раскрытого дерева (внутренний идентификатор автору запрещён, но обход дешёвый).
const (
	vaultMacroName    = "vault"
	vaultExpandedName = vaultFuncName // "__vault_read"
)

// DetectSealed сообщает, читает ли интерполируемая строка raw (с `${ … }`-блоками)
// хоть один secret-источник по sources — то есть должна ли ячейка с этим
// значением быть помечена sealed. Каждый `${expr}`-блок парсится без макросов
// (parseNoMacro) и обходится PostOrderVisit-ом; первая найденная ссылка на
// секрет → true (whole-cell taint). Строки без `${ … }` (чистый литерал) и
// блоки без секрет-ссылок → false. Ошибка парса блока (битый CEL поймает eval-
// фаза отдельно) → блок пропускается, не sealed — детектор не дублирует
// валидацию, а лишь маркирует taint у валидных выражений.
func (e *Engine) DetectSealed(raw string, sources SealSources) bool {
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		return false
	}
	for _, s := range segs {
		if !s.expr {
			continue
		}
		if e.exprReadsSecret(s.text, sources) {
			return true
		}
	}
	return false
}

// exprReadsSecret парсит одно CEL-выражение (без обёртки `${ }`) без макросов и
// возвращает true, если где-либо в дереве оно читает secret-источник по sources.
func (e *Engine) exprReadsSecret(expr string, sources SealSources) bool {
	parsed, perr := e.parseNoMacro(expr)
	if perr != nil {
		return false
	}
	found := false
	ast.PostOrderVisit(parsed.Expr(), ast.NewExprVisitor(func(n ast.Expr) {
		if found {
			return
		}
		switch n.Kind() {
		case ast.CallKind:
			if isVaultCall(n) {
				found = true
			}
		case ast.SelectKind:
			if base, field, ok := selectBaseField(n); ok && readsSecretSelect(base, field, sources) {
				found = true
			}
		}
	}))
	return found
}

// isVaultCall — узел вида vault(...) (global-call `vault`, до macro-expansion) или
// уже раскрытый __vault_read(...). Member-call (`x.vault()`) не считается — это
// не наша функция.
func isVaultCall(n ast.Expr) bool {
	c := n.AsCall()
	if c.IsMemberFunction() {
		return false
	}
	name := c.FunctionName()
	return name == vaultMacroName || name == vaultExpandedName
}

// selectBaseField извлекает (base-ident, field) из Select-узла вида
// `<ident>.<field>` (например `input.password`). Вложенные Select-ы
// (`params.vars.tls_cert`) обход PostOrderVisit посещает поуровнево: пара
// `vars.tls_cert` придёт как отдельный Select (operand=ident `vars`), поэтому
// верхне-контекстный идентификатор (input/vars/compute) + имя следующего поля
// детектируются именно на этом уровне. Возврат ok=false, если операнд — не голый
// ident (например результат вызова): тогда секрет-источник определяется его
// собственным под-узлом в обходе.
func selectBaseField(n ast.Expr) (base, field string, ok bool) {
	s := n.AsSelect()
	if s.IsTestOnly() {
		return "", "", false
	}
	op := s.Operand()
	if op.Kind() != ast.IdentKind {
		return "", "", false
	}
	return op.AsIdent(), s.FieldName(), true
}

// readsSecretSelect — пара (base, field) обращается к секрет-источнику:
//   - input.<field>, field ∈ SecretInputs;
//   - vars.<field>, field ∈ SealedVars;
//   - compute.<field>, field ∈ SealedCompute.
func readsSecretSelect(base, field string, sources SealSources) bool {
	switch base {
	case "input":
		return sources.SecretInputs[field]
	case "vars":
		return sources.SealedVars[field]
	case "compute":
		return sources.SealedCompute[field]
	}
	return false
}
