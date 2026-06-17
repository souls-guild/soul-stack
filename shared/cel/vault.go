package cel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/parser"
)

// CEL-функция vault() ([templating.md §2.3], [ADR-017]). Резолвит секрет Vault
// KV на keeper-стороне в CEL-render-фазе:
//
//	${ vault('secret/redis/admin').password }   → map → .field (CEL-доступ)
//	${ vault('secret/redis/admin#password') }   → один field напрямую (#-суффикс)
//
// Симметрия с `vault:`-ref в params (vault_resolve.go): без `#field` возвращается
// весь map секрета, с `#field` — одно значение. Аргумент — путь: строковый литерал
// ИЛИ CEL-выражение из ДОВЕРЕННОГО контекста (incarnation/vars), резолвится CEL-ом
// ДО вызова ReadKV — это не строковая склейка в Vault-запрос, инъекции нет.
// Operator-input в путь не попадает по контракту варианта (а): vault() явно
// прописывается автором scenario/destiny, а не подставляется из input.
//
// Резолв keeper-side: реальное значение секрета подставляется в Params и уходит
// на Soul реальным; маскирование — на выходе (логи/OTel/UI), CEL обрабатывает
// значения нормально ([ADR-010]).

// ErrVaultUnavailable — vault() встречен в выражении, но Engine собран без
// KVReader (vault-функция не зарегистрирована). Отдельный класс, чтобы caller
// отличал «нет vault-клиента в этом контексте» от ошибки автора.
var ErrVaultUnavailable = errors.New("CEL: vault() недоступен — Engine собран без KVReader")

// KVReader — узкое подмножество Vault KV-клиента, нужное CEL-функции vault().
// keeper/internal/vault.Client удовлетворяет интерфейсу как есть; сужение
// позволяет герметичный прогон (soul-lint/L0, Trial) с fixture-backed reader-ом.
// Симметрично keeper/internal/render.KVReader.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// vaultFuncName — имя функции после macro-expansion (внутреннее, 2-арное:
// path + resolver-носитель контекста). Пользователь пишет vault('path').
const vaultFuncName = "__vault_read"

// vaultRefPrefix — каноничный префикс vault-reference (`vault:<mount>/<path>`).
// Совпадает с keeper/internal/render.vaultRefPrefix и audit.CredentialsRefPrefix
// без `secret/`-хвоста (shared/cel не может импортировать keeper/internal —
// слоистость, поэтому константа дублируется здесь). Используется только при
// нормализации пути vault() к ref-форме в текстах ошибок (см. vaultRefForm).
const vaultRefPrefix = "vault:"

// vaultResolverVar — зарезервированная activation-переменная, несущая
// per-eval resolver {ctx, kv} в binding функции. Имя с префиксом '__'
// зарезервировано за internal-механизмами: авторские выражения с любым
// `__`-идентификатором отвергаются [internalIdentGuard] в guardUnsupported
// (functions.go) ДО compile — иначе автор обходит macro vault(), вызывая
// `__vault_read(...)` напрямую. Макрос подставляет эту переменную как
// hidden-аргумент после прохождения guard-а.
const vaultResolverVar = "__vault_resolver"

// vaultResolver несёт per-eval контекст и reader для binding функции vault().
// Кладётся в activation под [vaultResolverVar]; Engine.kv (immutable) разделяется
// всеми прогонами, ctx — request-scoped. Так vault() concurrency-safe при общем
// Engine: состояние per-call живёт в активации, не на Engine.
//
// Реализует ref.Val (opaque): cel-адаптер не умеет конвертировать произвольный
// Go-указатель в ref.Val при ResolveName, поэтому resolver сам — ref.Val и
// проходит насквозь до binding-а callVault, который читает его через .Value().
type vaultResolver struct {
	ctx context.Context
	kv  KVReader
}

// vaultResolverType — opaque CEL-тип resolver-а (не часть пользовательской
// type-model; resolver — hidden-аргумент макроса).
var vaultResolverType = types.NewOpaqueType("soulstack.vaultResolver")

func (r *vaultResolver) ConvertToNative(reflect.Type) (any, error) {
	return nil, errors.New("vaultResolver: не конвертируется в native (internal carrier)")
}
func (r *vaultResolver) ConvertToType(ref.Type) ref.Val {
	return types.NewErr("vaultResolver: не конвертируется (internal carrier)")
}
func (r *vaultResolver) Equal(ref.Val) ref.Val { return types.False }
func (r *vaultResolver) Type() ref.Type        { return vaultResolverType }
func (r *vaultResolver) Value() any            { return r }

// vaultEnvOptions возвращает EnvOption-ы регистрации vault(): декларация
// resolver-переменной + макрос vault(p) → __vault_read(p, __vault_resolver) +
// binding 2-арной функции. Вызывается из New только когда kv != nil.
func vaultEnvOptions() []cel.EnvOption {
	macro := parser.NewGlobalMacro("vault", 1, expandVaultMacro)
	return []cel.EnvOption{
		cel.Variable(vaultResolverVar, cel.DynType),
		cel.Macros(macro),
		cel.Function(vaultFuncName,
			cel.Overload(vaultFuncName+"_string_dyn",
				[]*cel.Type{cel.StringType, cel.DynType}, cel.DynType,
				cel.BinaryBinding(callVault),
			),
		),
	}
}

// expandVaultMacro раскрывает vault(<path>) в __vault_read(<path>,
// __vault_resolver): hidden-второй аргумент несёт per-eval resolver из активации.
// Так пользовательская 1-арная форма сохраняется, а binding получает и путь, и
// (ctx, kv). nil-error при ok-раскрытии (контракт parser.MacroExpander).
func expandVaultMacro(mef parser.ExprHelper, _ ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
	resolver := mef.NewIdent(vaultResolverVar)
	return mef.NewCall(vaultFuncName, args[0], resolver), nil
}

// callVault — binding функции __vault_read(path, resolver). path — уже
// вычисленный CEL-ом строковый аргумент (литерал или выражение из доверенного
// контекста), resolver — носитель {ctx, kv} из активации. Возвращает map
// секрета (без #field) либо одно поле (с #field). Ошибки Vault/формата —
// types.NewErr (штатная CEL eval-ошибка, не паника); plaintext-секрет в текст
// ошибки не подставляется, а путь Vault маскируется на выходе как vault-ref.
func callVault(pathVal, resolverVal ref.Val) ref.Val {
	path, ok := pathVal.Value().(string)
	if !ok {
		return types.NewErr("vault(): аргумент-путь должен быть строкой, получено %s", pathVal.Type().TypeName())
	}
	res, ok := resolverVal.Value().(*vaultResolver)
	if !ok || res == nil || res.kv == nil {
		return types.NewErr("vault(): %v", ErrVaultUnavailable)
	}

	body, field, err := splitVaultField(path)
	if err != nil {
		return types.NewErr("vault(): %v", err)
	}

	data, rerr := res.kv.ReadKV(res.ctx, body)
	if rerr != nil {
		// rerr может нести bare-путь (vault.ErrVaultKVNotFound: secret/<path>),
		// который НЕ ловится маркером CredentialsRefPrefix (`vault:secret/`) —
		// между `vault:` и `secret/` в тексте sentinel-а стоит ` KV path not
		// found: `. Поэтому НЕ пробрасываем rerr.Error() как есть: формируем
		// текст с путём в ref-форме `vault:<body>`, тогда audit.MaskSecrets
		// маскирует всю строку целиком (status_details / error_summary).
		// Симметрия с vault:-ref (render/vault_resolve.go ParseRef).
		return types.NewErr("vault(): чтение секрета %s: путь не найден или ошибка Vault", vaultRefForm(body))
	}

	if field == "" {
		return types.DefaultTypeAdapter.NativeToValue(data)
	}
	val, ok := data[field]
	if !ok {
		// Имя поля — не секрет (это ключ внутри секрета), но путь — секрет:
		// даём его в ref-форме, чтобы строка целиком маскировалась на выходе.
		return types.NewErr("vault(): поле %q отсутствует в секрете %s", field, vaultRefForm(body))
	}
	return types.DefaultTypeAdapter.NativeToValue(val)
}

// vaultRefForm приводит путь Vault KV к каноничной ref-форме `vault:<body>`
// (нормализуя ведущий '/'), под которую заточен маркер
// audit.CredentialsRefPrefix (`vault:secret/`). Используется только при
// формировании текста ошибок vault(): путь — секрет (наводка на секрет-локацию),
// и должен маскироваться целиком при попадании в наблюдаемые каналы
// (status_details / error_summary / логи). Симметрия с vault:-ref в params.
func vaultRefForm(body string) string {
	return vaultRefPrefix + strings.TrimPrefix(body, "/")
}

// splitVaultField разбивает аргумент vault() на путь и опциональное #field
// (последний '#'). Без '#' field пуст. Пустой путь или пустое поле после '#'
// — ошибка. Путь дополнительно валидируется на форму `<mount>/<path>`
// (validateVaultPath) симметрично vault.ParseRef: `vault('foo')` без слеша →
// понятная ошибка формата, а не relative-путь в ReadKV. Симметрично readVaultRef
// в keeper/internal/render/vault_resolve.go.
func splitVaultField(arg string) (body, field string, err error) {
	if i := strings.LastIndexByte(arg, '#'); i >= 0 {
		body, field = arg[:i], arg[i+1:]
		if field == "" {
			return "", "", errors.New("пустое имя поля после '#'")
		}
	} else {
		body = arg
	}
	if err := validateVaultPath(body); err != nil {
		return "", "", err
	}
	return body, field, nil
}

// validateVaultPath проверяет, что путь vault() имеет форму `<mount>/<path>`
// (с опциональным ведущим '/', как vault:-ref). Без `/`-разделителя между
// mount-ом и rel-частью или с пустыми частями — ошибка формата. Зеркало
// vault.ParseRef (keeper/internal/vault): единая нормализация для обеих форм
// vault-секретов (CEL vault() и vault:-ref в params).
func validateVaultPath(body string) error {
	b := strings.TrimPrefix(body, "/")
	if b == "" {
		return errors.New("пустой путь")
	}
	slash := strings.IndexByte(b, '/')
	if slash <= 0 || slash == len(b)-1 {
		return fmt.Errorf("путь %q должен иметь форму <mount>/<path> (например secret/redis/admin)", vaultRefForm(body))
	}
	return nil
}
