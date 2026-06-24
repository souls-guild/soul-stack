package cel

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/parser"
)

// CEL-функция default(x, y) ([templating.md §2.3], [ADR-010 Amendment
// 2026-06-23]). Значение-или-дефолт, parity Ansible `| default()`: если x
// присутствует/доступен — вернуть x, иначе y. Сокращает канонический has()-guard
// ([docs/input.md]):
//
//	default(essence.tls_enable, false)   ≡  has(essence.tls_enable) ? essence.tls_enable : false
//	default(input.redis_settings, {})    ≡  has(input.redis_settings) ? input.redis_settings : {}
//	int(default(essence.tls_port, 7379)) ≡  int(has(essence.tls_port) ? essence.tls_port : 7379)
//
// Механизм — custom macro (compile-time AST-rewrite), как vault() ([vault.go]).
// CEL вычисляет аргументы ЖАДНО: будь default() обычной функцией,
// default(essence.tls_enable, false) упал бы «no such key» на отсутствующем
// ключе ДО вызова. Макрос видит AST первого аргумента ДО eval и переписывает
// его в проверенную форму has(x) ? x : y — та же техника обхода жадности, что у
// vault(). Это compile-time rewrite, не runtime-исполнение строки.
//
// Ограничение (семантика Ansible-default): x обязан быть select-chain или
// идентификатором. has() в CEL применим ТОЛЬКО к доступу к полю (select),
// поэтому:
//   - Select x (essence.tls_enable, a.b.c) → has(x) ? x : y;
//   - голый идентификатор-корень (input/essence/…) всегда присутствует в
//     активации ([Vars.activation] биндит корни как пустой map, не
//     «отсутствуют») → разворачивается в сам x (fallback недостижим — корректная
//     вырожденная семантика; has(ident) в CEL вообще не компилируется);
//   - аргумент-выражение (default(size(x), 0), default(a+b, 0), индекс-доступ
//     default(a['k'], 0)) → внятная compile-ошибка (*common.Error). Это корректная
//     семантика: default над переменной/полем, не над вычислением (для последнего
//     у автора есть тернар).
//
// Pure: без I/O/секретов/крипты/eval-time состояния (синтаксический сахар над
// has()?:). Регистрируется в Keeper-full env ([New]) и Soul flow-control env
// ([NewFlowControl]); в migration-CEL ([NewMigration], [ADR-019]) НЕ
// регистрируется — hermetic-песочница с минимумом surface area (симметрия с
// merge()/glob()).
//
// Masking-инвариант: default(x, y) НЕ переименовывает ключ назначения, поэтому
// секрет, подставленный через default(essence.x, vault('…#password')) или
// присвоенный sensitive-именованному ключу (password/secret/token/tls_key/…),
// маскируется выходным слоем (shared/audit.MaskSecrets) ИДЕНТИЧНО прямой
// подстановке; границу маскинга не расширяет и не сужает (симметрия с merge()).

// defaultMacroName — имя функции в CEL-env. Пользователь пишет default(x, y).
const defaultMacroName = "default"

// defaultEnvOptions возвращает EnvOption-ы регистрации default(): глобальный
// 2-арный макрос. Реальной cel.Function НЕТ — макрос полностью разворачивается в
// has()?:-тернар на parse-фазе, binding не нужен. Симметрично vault() в части
// macro-механизма, но без сопутствующей функции (vault() оставляет
// __vault_read-binding, default() — чистый rewrite).
func defaultEnvOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Macros(parser.NewGlobalMacro(defaultMacroName, 2, expandDefaultMacro)),
	}
}

// expandDefaultMacro раскрывает default(x, y) в has(x) ? x : y (для select) либо
// в сам x (для голого идентификатора-корня — он всегда present в активации,
// has(ident) в CEL не компилируется). x обязан быть Select или Ident; иначе —
// внятная compile-ошибка (default над выражением не имеет смысла «значения-или-
// дефолта»: для вычислений автор пишет явный тернар).
func expandDefaultMacro(mef parser.ExprHelper, _ ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
	x, fallback := args[0], args[1]
	switch x.Kind() {
	case ast.SelectKind:
		s := x.AsSelect()
		// Index-доступ (a['k']) — не Select, а CallKind (_[_]), он сюда не
		// попадает. TestOnly-select (has(...) сам) тоже сюда не попадёт от
		// пользователя в позиции аргумента. Строим has(operand.field):
		// presence-test над тем же operand/field, что у исходного select.
		presence := mef.NewPresenceTest(mef.Copy(s.Operand()), s.FieldName())
		return mef.NewCall(operators.Conditional, presence, mef.Copy(x), fallback), nil
	case ast.IdentKind:
		// Корневой идентификатор всегда присутствует в активации
		// ([Vars.activation]); has(ident) в CEL не компилируется. Fallback
		// недостижим — разворачиваем в сам x.
		return mef.Copy(x), nil
	default:
		return nil, mef.NewError(x.ID(),
			"default(x, y): первый аргумент должен быть полем (essence.tls_enable, a.b.c) "+
				"или идентификатором, а не выражением — для вычислений используй тернар has(...)?...:...")
	}
}
