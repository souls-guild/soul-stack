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

// The CEL vault() function ([templating.md §2.3], [ADR-017]). Resolves a Vault KV
// secret keeper-side during the CEL render phase:
//
//	${ vault('secret/redis/admin').password }   → map → .field (CEL access)
//	${ vault('secret/redis/admin#password') }   → one field directly (# suffix)
//
// Symmetric with the `vault:` ref in params (vault_resolve.go): without `#field` the
// whole secret map is returned, with `#field` a single value. The argument is a path:
// a string literal OR a CEL expression from a TRUSTED context (incarnation/vars),
// resolved by CEL BEFORE the ReadKV call — this is not string concatenation into a
// Vault request, so there's no injection. Operator input never reaches the path per the
// contract of variant (a): vault() is written explicitly by the scenario/destiny
// author, not substituted from input.
//
// Keeper-side resolution: the real secret value is substituted into Params and goes to
// the Soul as-is; masking happens on output (logs/OTel/UI), CEL processes the values
// normally ([ADR-010]).

// ErrVaultUnavailable — vault() appears in an expression, but the Engine was built
// without a KVReader (the vault function isn't registered). A separate class so the
// caller distinguishes "no vault client in this context" from an author error.
var ErrVaultUnavailable = errors.New("CEL: vault() недоступен — Engine собран без KVReader")

// KVReader — the narrow subset of the Vault KV client needed by the CEL vault()
// function. keeper/internal/vault.Client satisfies the interface as-is; narrowing
// allows a hermetic run (soul-lint/L0, Trial) with a fixture-backed reader. Symmetric
// with keeper/internal/render.KVReader.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// vaultFuncName — the function name after macro expansion (internal, 2-ary:
// path + the resolver carrying the context). The user writes vault('path').
const vaultFuncName = "__vault_read"

// vaultResolverVar — a reserved activation variable carrying the per-eval resolver
// {ctx, kv} into the binding function. The '__' prefix is reserved for internal
// mechanisms: author expressions with any `__` identifier are rejected by
// [internalIdentGuard] in guardUnsupported (functions.go) BEFORE compile — otherwise an
// author could bypass the vault() macro by calling `__vault_read(...)` directly. The
// macro injects this variable as a hidden argument after passing the guard.
const vaultResolverVar = "__vault_resolver"

// vaultResolver carries the per-eval context and reader for the vault() binding
// function. Placed in the activation under [vaultResolverVar]; Engine.kv (immutable) is
// shared by all runs, ctx is request-scoped. This makes vault() concurrency-safe with a
// shared Engine: per-call state lives in the activation, not on the Engine.
//
// Implements ref.Val (opaque): the cel adapter can't convert an arbitrary Go pointer
// into a ref.Val at ResolveName, so the resolver is itself a ref.Val and passes through
// to the callVault binding, which reads it via .Value().
type vaultResolver struct {
	ctx context.Context
	kv  KVReader
}

// vaultMemoKey — a private context-value key type for the per-render-pass cache of
// vault() resolutions (see [WithVaultMemo], [vaultMemo]). Unexported so the value can't
// be overwritten from outside the package by an accidental key collision.
type vaultMemoKey struct{}

// vaultMemo — a cache of vault() resolutions within ONE render-pass. The key is `body`
// (the Vault path WITHOUT `#field`), i.e. exactly the ReadKV argument; the value is the
// whole secret map. Dedup is tied to the backend call: vault('secret/x#password') and
// vault('secret/x#tls') hit one ReadKV('secret/x'), so we cache the whole map and select
// the needed field per-call AFTER the cache (#field correctness is preserved — different
// fields are selected from one cached map).
//
// Scope is per-render-pass: the cache lives in a context.Context that Keeper creates for
// one Pipeline.Render (one incarnation, one run) and passes into Vars.Ctx of all eval
// calls of that pass. Not package-level and not on the Engine (the Engine is shared
// across incarnations) — otherwise there'd be cross-request secret leakage and stale
// values. Different render passes carry different contexts → they don't share the cache.
//
// Concurrency: within one render-pass the eval calls are sequential (Pipeline.Render is
// sequential per-task), but mu is held for safety in case of concurrent per-host fan-out
// over a shared ctx. A cache miss may lead to a parallel double ReadKV of one path
// (harmless: the value is idempotent), but the map write is synchronized.
type vaultMemo struct {
	mu sync.Mutex
	m  map[string]map[string]any
}

// WithVaultMemo binds a per-render-pass cache of vault() resolutions to ctx. Called by
// Keeper ONCE at the start of a render-pass (Pipeline.Render); the returned ctx is
// passed into Vars.Ctx of all eval calls of the pass. A repeated vault() with the same
// path in that pass is served from the cache — Vault isn't hit again. Without a call (or
// with a ctx lacking the memo — soul-lint/Trial, direct unit-eval) vault() works as
// before, hitting ReadKV every time: the cache is an optimization, not a result contract.
func WithVaultMemo(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(vaultMemoKey{}).(*vaultMemo); ok {
		return ctx // idempotent: re-binding doesn't spawn nested caches.
	}
	return context.WithValue(ctx, vaultMemoKey{}, &vaultMemo{m: map[string]map[string]any{}})
}

// readKVMemoized reads secret body via kv with dedup within a render-pass. The cache
// is taken from ctx ([vaultMemoKey], created by [WithVaultMemo]). If there's no cache
// (ctx without memo) — a direct ReadKV without caching. ReadKV errors are NOT cached: a
// retry in the same pass (e.g. a transient Vault failure) repeats the read.
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

// vaultResolverType — the opaque CEL type of the resolver (not part of the user
// type-model; the resolver is a hidden macro argument).
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

// vaultEnvOptions returns the EnvOptions that register vault(): declaration of the
// resolver variable + macro vault(p) → __vault_read(p, __vault_resolver) + binding of
// the 2-ary function. Called from New only when kv != nil.
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

// expandVaultMacro expands vault(<path>) into __vault_read(<path>, __vault_resolver):
// the hidden second argument carries the per-eval resolver from the activation. This
// keeps the user's 1-ary form while the binding gets both the path and (ctx, kv).
// nil error on successful expansion (parser.MacroExpander contract).
func expandVaultMacro(mef parser.ExprHelper, _ ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
	resolver := mef.NewIdent(vaultResolverVar)
	return mef.NewCall(vaultFuncName, args[0], resolver), nil
}

// callVault — the binding of __vault_read(path, resolver). path is the already
// CEL-evaluated string argument (a literal or an expression from a trusted context),
// resolver carries {ctx, kv} from the activation. Returns the secret map (without
// #field) or a single field (with #field). Vault/format errors → types.NewErr (a normal
// CEL eval error, not a panic); a plaintext secret is NOT put into the error text. The
// path of a missing/broken secret is given in FLAT form (NIM-73): a not-found secret has
// no value to leak, and the path is actionable diagnostics (the operator needs to know
// WHAT to seed into Vault). The flat form survives observability masking (audit.vaultRefRe
// matches only `vault:<mount>/`), so status_details/error_summary carry clear text rather
// than `***MASKED***`. An actually-resolved secret VALUE is still masked on output (the
// masking layer is untouched).
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
		// NIM-73: do NOT forward rerr.Error() (it may carry transport details);
		// instead build actionable text with the path in FLAT form. A not-found
		// secret has no value → no leak; path+field tell the operator WHAT to seed
		// (secret/redis/<inc>/users/<name>#password). The flat form doesn't contain
		// `vault:<mount>/`, so it survives masking of status_details/error_summary —
		// the operator sees a clear cause, not `***MASKED***`.
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
		// Path+field name is actionable diagnostics (which field to seed), not a
		// secret value: values of other secret fields are NOT put into the text. The
		// path in flat form survives masking (NIM-73).
		return types.NewErr("vault(): в секрете %s нет поля %q", vaultPathHint(body), field)
	}
	return types.DefaultTypeAdapter.NativeToValue(val)
}

// vaultPathHint normalizes a Vault KV path for actionable vault() error texts (NIM-73):
// a flat form without the `vault:` prefix, with the leading '/' stripped. The flat path
// does NOT contain the `vault:<mount>/` marker (audit.vaultRefRe), so it survives
// observability masking of status_details/error_summary — the operator sees WHAT to
// seed. The path of a missing/broken secret is not a secret value (there's no value for
// it), it's diagnostics; the resolved secret itself is masked separately (the masking
// layer is untouched).
func vaultPathHint(body string) string {
	return strings.TrimPrefix(body, "/")
}

// splitVaultField splits the vault() argument into the path and an optional #field (the
// last '#'). Without '#' the field is empty. An empty path or an empty field after '#'
// is an error. The path is additionally validated for the `<mount>/<path>` form
// (validateVaultPath) symmetric with vault.ParseRef: `vault('foo')` without a slash → a
// clear format error rather than a relative path in ReadKV. Symmetric with readVaultRef
// in keeper/internal/render/vault_resolve.go.
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

// validateVaultPath checks that a vault() path has the form `<mount>/<path>` (with an
// optional leading '/', like a vault: ref). Without a `/` separator between the mount
// and the rel part, or with empty parts, it's a format error. A mirror of vault.ParseRef
// (keeper/internal/vault): a single normalization for both forms of vault secrets (CEL
// vault() and the vault: ref in params).
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
