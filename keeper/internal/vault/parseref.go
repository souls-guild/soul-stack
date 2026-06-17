package vault

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidVaultRef — строка не соответствует формату `vault:<mount>/<path>`.
// Sentinel позволяет вызывающему коду различать «формат сломан» от транспортных
// ошибок Vault.
var ErrInvalidVaultRef = errors.New("vault: invalid ref format (expected vault:<mount>/<path>)")

// ParseRef разбирает строку `vault:<mount>/<rel>` в logical path
// (`<mount>/<rel>`), который ожидает [Client.ReadKV].
//
// Используется всеми потребителями `*_ref`-полей `keeper.yml`
// (`postgres.dsn_ref`, `auth.jwt.signing_key_ref`, и т.п.) для единой
// нормализации. Leading `/` после `vault:` допустим (`vault:/secret/...`).
//
// # Нормализация logical-path (security-инвариант)
//
// Возвращаемый logical-path нормализован: повторные слэши (`//`) схлопнуты в
// один. Это единственная точка нормализации для ОБОИХ каналов резолва —
// авторского (render.readVaultRef) и operator-input (scenario.input_vault),
// поэтому scope-match / deny-list / [Client.ReadKV] всегда работают по одному
// каноническому значению. Без этого ненормализованный путь обходил hard
// deny-list: `secret//keeper/x` не матчил префикс `secret/keeper/`, но ReadKV
// сводил его к запрещённому `secret/keeper/x` (эскалация оператор→Vault).
//
// Сегменты `.` и `..` ОТВЕРГАЮТСЯ как невалидный ref (не нормализуются молча):
// `..` — подъём выше mount-а, для Vault KV семантики нет, это вектор обхода
// scope. Регистр НЕ трогается — Vault paths case-sensitive.
//
// Примеры:
//
//	"vault:secret/keeper/postgres"         → "secret/keeper/postgres"
//	"vault:secret/keeper/jwt-signing-key"  → "secret/keeper/jwt-signing-key"
//	"vault:/secret/keeper/k"               → "secret/keeper/k"
//	"vault:secret//keeper/x"               → "secret/keeper/x"
//	"vault:secret/./keeper/x"              → ошибка (сегмент `.`)
//	"vault:secret/keeper/../keeper/x"      → ошибка (сегмент `..`)
//
// Любое отклонение от формата (нет `vault:`-префикса, пустое тело, нет
// `/`-разделителя между mount-ом и rel-частью, trailing slash без rel,
// сегмент `.`/`..`) возвращает обёрнутый [ErrInvalidVaultRef].
func ParseRef(ref string) (string, error) {
	const prefix = "vault:"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("%w: got %q", ErrInvalidVaultRef, ref)
	}
	body := strings.TrimPrefix(ref, prefix)
	body = strings.TrimPrefix(body, "/")
	if body == "" {
		return "", fmt.Errorf("%w: empty path in %q", ErrInvalidVaultRef, ref)
	}
	slash := strings.Index(body, "/")
	if slash <= 0 || slash == len(body)-1 {
		return "", fmt.Errorf("%w: missing <mount>/<path> in %q", ErrInvalidVaultRef, ref)
	}
	normalized, err := normalizeLogical(body)
	if err != nil {
		return "", fmt.Errorf("%w: %v in %q", ErrInvalidVaultRef, err, ref)
	}
	return normalized, nil
}

// normalizeLogical схлопывает повторные слэши в logical-path (`mount/rel`) и
// отвергает сегменты `.`/`..` (обход scope/deny). Эквивалент path.Clean по
// rel-части + явный запрет `..`, но без зависимости от path-семантики
// (path.Clean молча схлопывает `a/../b`→`b`, а нам нужно ОТВЕРГНУТЬ `..`,
// а не «починить» путь оператора). Регистр и сами сегменты не меняются.
func normalizeLogical(body string) (string, error) {
	segments := strings.Split(body, "/")
	out := segments[:0]
	for _, seg := range segments {
		switch seg {
		case "":
			// повторный слэш (`//`) — схлопываем, сегмент отбрасываем.
			continue
		case ".", "..":
			return "", fmt.Errorf("сегмент %q недопустим в vault-path", seg)
		default:
			out = append(out, seg)
		}
	}
	if len(out) < 2 {
		return "", fmt.Errorf("после нормализации не осталось <mount>/<path>")
	}
	return strings.Join(out, "/"), nil
}
