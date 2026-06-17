package config

// Scoped-резолв `vault:`-ref в operator-input (docs/input.md → «vault_scope»).
//
// Граница доверия: значение secret-поля, переданное оператором, может быть
// `vault:`-ref. Резолвить любой такой ref значит дать оператору прочитать ЛЮБОЙ
// путь Vault-токена Keeper-а (включая `secret/keeper/jwt-signing-key`) — это
// эскалация. Поэтому резолв input-vault-ref разрешён только для поля с
// объявленным `vault_scope` (prefix-glob) и только если резолвимый путь матчит
// scope И не попадает в hard deny-list (страховка от ошибки автора в scope).
//
// Сама проверка одного ref (scope-match + deny) — чистая функция от строк,
// поэтому живёт в shared/config (Soul-safe, без vault-клиента). Чтение KV и
// аудит — keeper-side (keeper/internal/scenario), там подключается этот floor.

import "strings"

// VaultInputFloor — система-floor hard deny-list (форк C): пути под этими
// префиксами НИКОГДА не резолвятся через operator-input, даже если автор поля
// ошибочно объявил покрывающий их `vault_scope`. Проверяется ПОСЛЕ scope-матча,
// всегда, безусловно. Расширяется конфигом (keeper.yml → vault.input_deny_paths),
// но не выключается им — system-floor неотключаем.
var VaultInputFloor = []string{
	"secret/keeper/",
	"secret/internal/",
}

// validVaultScopeGlob — форма prefix-glob для `vault_scope`: один trailing `*`,
// перед ним непустой logical-prefix вида `<mount>/<path>` (есть `/`-разделитель
// mount-а и пути). Без `/` сужения по mount-у нет — отвергаем как бессмысленный.
// Без trailing `*` — это точечный путь, не prefix; допускаем и его (точное
// совпадение), но требуем хотя бы `<mount>/<leaf>`.
func validVaultScopeGlob(scope string) bool {
	if scope == "" {
		return false
	}
	prefix := strings.TrimSuffix(scope, "*")
	// после снятия `*` должен остаться `<mount>/<...>` — есть `/` не в начале.
	slash := strings.Index(prefix, "/")
	return slash > 0
}

// MatchesVaultScope — logical-path Vault KV (`<mount>/<rel>`, без `vault:`-
// префикса и без `#field`-суффикса) попадает в prefix-glob `vault_scope`.
//
// Семантика glob — единственный trailing `*` = prefix-match; без `*` — точное
// совпадение. Промежуточные `*` не поддерживаются (MVP: один prefix-glob).
func MatchesVaultScope(scope, logical string) bool {
	if scope == "" {
		return false
	}
	if strings.HasSuffix(scope, "*") {
		return strings.HasPrefix(logical, strings.TrimSuffix(scope, "*"))
	}
	return logical == scope
}

// DeniedByVaultFloor — logical-path попадает под hard deny-list (system-floor +
// опц. конфиг-расширение extra). Проверяется ПОСЛЕ scope-матча, безусловно.
// extra может быть nil (только system-floor).
func DeniedByVaultFloor(logical string, extra []string) bool {
	for _, p := range VaultInputFloor {
		if strings.HasPrefix(logical, p) {
			return true
		}
	}
	for _, p := range extra {
		if p != "" && strings.HasPrefix(logical, p) {
			return true
		}
	}
	return false
}
