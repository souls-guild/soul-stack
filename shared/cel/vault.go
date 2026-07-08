package cel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

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

// vaultMemoKey — приватный тип ключа context-value для per-render-pass кеша
// vault()-резолвов (см. [WithVaultMemo], [vaultMemo]). Неэкспортируемый тип —
// чтобы значение нельзя было перезаписать снаружи пакета случайной коллизией
// ключа.
type vaultMemoKey struct{}

// vaultMemo — кеш резолвов vault() в рамках ОДНОГО render-pass. Ключ — `body`
// (путь Vault БЕЗ `#field`), то есть ровно аргумент ReadKV; значение — весь map
// секрета. Дедуп привязан к backend-вызову: vault('secret/x#password') и
// vault('secret/x#tls') бьют один ReadKV('secret/x'), поэтому кешируем map
// целиком, а нужное поле выбирается per-call ПОСЛЕ кеша (корректность по
// #field сохраняется — разные поля выбираются из одного кешированного map).
//
// Scope — per-render-pass: кеш живёт в context.Context, который Keeper
// заводит на один Pipeline.Render (одна инкарнация, один прогон) и прокидывает
// в Vars.Ctx всех eval-вызовов этого pass-а. Не package-level и не на Engine
// (Engine шарится между инкарнациями) — иначе была бы межзапросная утечка
// секретов и stale-значения. Разные render-pass-ы несут разные context-ы → не
// делят кеш.
//
// Concurrency: в пределах одного render-pass eval-вызовы последовательны
// (Pipeline.Render — sequential per-task), но mu держим для безопасности на
// случай конкурентного per-host fan-out над общим ctx. Промах кеша может
// привести к параллельному двойному ReadKV одного пути (безвредно: значение
// идемпотентно), но запись в map синхронизирована.
type vaultMemo struct {
	mu sync.Mutex
	m  map[string]map[string]any
}

// WithVaultMemo привязывает к ctx per-render-pass кеш vault()-резолвов. Зовётся
// Keeper-ом ОДИН раз на старте render-pass (Pipeline.Render), полученный ctx
// прокидывается в Vars.Ctx всех eval-вызовов pass-а. Повторный vault() с тем же
// путём в этом pass-е берётся из кеша — Vault не бьётся снова. Без вызова (или с
// ctx без memo — soul-lint/Trial, прямые unit-eval) vault() работает как раньше,
// бьёт ReadKV каждый раз: кеш — оптимизация, не контракт результата.
func WithVaultMemo(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(vaultMemoKey{}).(*vaultMemo); ok {
		return ctx // идемпотентно: повторная привязка не плодит вложенные кеши.
	}
	return context.WithValue(ctx, vaultMemoKey{}, &vaultMemo{m: map[string]map[string]any{}})
}

// readKVMemoized читает секрет body через kv с дедупом в рамках render-pass.
// Кеш берётся из ctx ([vaultMemoKey], заведён [WithVaultMemo]). Если кеша нет
// (ctx без memo) — прямой ReadKV без кеширования. Ошибки ReadKV НЕ кешируются:
// retry в том же pass-е (напр. транзиентный сбой Vault) повторит чтение.
func readKVMemoized(ctx context.Context, kv KVReader, body string) (map[string]any, error) {
	memo, ok := ctx.Value(vaultMemoKey{}).(*vaultMemo)
	if !ok {
		return kv.ReadKV(ctx, body)
	}

	memo.mu.Lock()
	cached, hit := memo.m[body]
	memo.mu.Unlock()
	if hit {
		return cached, nil
	}

	data, err := kv.ReadKV(ctx, body)
	if err != nil {
		return nil, err
	}

	memo.mu.Lock()
	memo.m[body] = data
	memo.mu.Unlock()
	return data, nil
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
// ошибки НЕ подставляется. Путь отсутствующего/битого секрета даётся в ПЛОСКОЙ
// форме (NIM-73): у не найденного секрета нет значения для утечки, а путь —
// actionable-диагностика (оператор должен знать, ЧТО досеять в Vault). Плоская
// форма переживает observability-маскинг (audit.vaultRefRe ловит только
// `vault:<mount>/`), поэтому status_details/error_summary несут внятный текст, а
// не `***MASKED***`. Реально резолвнутый секрет-ЗНАЧЕНИЕ по-прежнему маскируется
// на выходе (маскинг-слой не тронут).
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

	data, rerr := readKVMemoized(res.ctx, res.kv, body)
	if rerr != nil {
		// NIM-73: НЕ пробрасываем rerr.Error() (может нести транспортные детали),
		// а формируем actionable-текст с путём в ПЛОСКОЙ форме. У ненайденного
		// секрета значения нет → утечки нет; путь+поле говорят оператору, ЧТО
		// досеять (secret/redis/<inc>/users/<name>#password). Плоская форма не
		// содержит `vault:<mount>/`, поэтому переживает маскинг status_details/
		// error_summary — оператор видит внятную причину, а не `***MASKED***`.
		if field != "" {
			return types.NewErr("vault(): секрет %s#%s не найден в Vault (KV path not found или нет доступа)", vaultPathHint(body), field)
		}
		return types.NewErr("vault(): секрет %s не найден в Vault (KV path not found или нет доступа)", vaultPathHint(body))
	}

	if field == "" {
		return types.DefaultTypeAdapter.NativeToValue(data)
	}
	val, ok := data[field]
	if !ok {
		// Путь+имя поля — actionable-диагностика (какое поле досеять), не секрет-
		// значение: значения других полей секрета в текст НЕ подставляются. Путь
		// в плоской форме — переживает маскинг (NIM-73).
		return types.NewErr("vault(): в секрете %s нет поля %q", vaultPathHint(body), field)
	}
	return types.DefaultTypeAdapter.NativeToValue(val)
}

// vaultPathHint нормализует путь Vault KV для actionable-текстов ошибок vault()
// (NIM-73): плоская форма без `vault:`-префикса, ведущий '/' срезан. Плоский путь
// НЕ содержит маркера `vault:<mount>/` (audit.vaultRefRe), поэтому переживает
// observability-маскинг status_details/error_summary — оператор видит, ЧТО
// досеять. Путь ненайденного/битого секрета — не секрет-значение (значения по
// нему нет), а диагностика; сам резолвнутый секрет маскируется отдельно (маскинг-
// слой не тронут).
func vaultPathHint(body string) string {
	return strings.TrimPrefix(body, "/")
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
		return fmt.Errorf("путь %q должен иметь форму <mount>/<path> (например secret/redis/admin)", vaultPathHint(body))
	}
	return nil
}
