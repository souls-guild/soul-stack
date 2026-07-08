package render

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// vaultRefPrefix — маркер строки-ссылки на Vault KV (`vault:<mount>/<path>`).
// Совпадает с формой, которую разбирает vault.ParseRef.
const vaultRefPrefix = "vault:"

// resolveVaultRefs — первая фаза pipeline (vault-resolve, [ADR-010]). Рекурсивно
// обходит params задачи и заменяет каждую строку-`vault:`-ref значением,
// прочитанным из Vault KV. No-op, если refs нет (PM-decision 2): обход дешёвый,
// дополнительного round-trip-а в Vault при отсутствии ссылок не происходит.
//
// Возвращается новая структура (исходная не мутируется): orchestrator может
// рендерить тот же scenario повторно (retry), а Vault-значения свежие на каждом
// прогоне.
//
// Ошибки:
//   - `${ … }`-маркер внутри vault-ref → ошибка валидации: vault-ref должен быть
//     статической строкой, интерполяция в нём двусмысленна (резолвить ${} до
//     или после чтения Vault?) и запрещена ([ADR-010], граница фаз).
//   - неизвестный путь / транспортная ошибка Vault → пробрасывается как есть
//     (vault.ErrVaultKVNotFound / wrapped).
func resolveVaultRefs(ctx context.Context, vc KVReader, params map[string]any) (map[string]any, error) {
	if len(params) == 0 {
		return params, nil
	}
	out, err := walkVaultValue(ctx, vc, params)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any)
	return m, nil
}

// walkVaultValue рекурсивно резолвит vault-refs в произвольном YAML-значении
// (map / slice / scalar). Возвращает новое значение того же вида.
func walkVaultValue(ctx context.Context, vc KVReader, v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := walkVaultValue(ctx, vc, val)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			rv, err := walkVaultValue(ctx, vc, val)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case string:
		if !strings.HasPrefix(t, vaultRefPrefix) {
			return t, nil
		}
		return readVaultRef(ctx, vc, t)
	default:
		return v, nil
	}
}

// readVaultRef разбирает строку-`vault:`-ref и читает соответствующий секрет.
//
// Форма ref: `vault:<mount>/<path>` (vault.ParseRef) с опциональным
// `#<field>`-суффиксом — выбор одного ключа из KV-секрета. Без суффикса
// возвращается весь map секрета (downstream-CEL извлечёт нужное поле).
func readVaultRef(ctx context.Context, vc KVReader, ref string) (any, error) {
	if strings.Contains(ref, "${") {
		return nil, fmt.Errorf("render: vault-ref %q содержит ${…}-маркер — vault-ref должен быть статической строкой ([ADR-010], граница фаз)", ref)
	}
	if vc == nil {
		return nil, fmt.Errorf("render: vault-ref %q встречен, но Vault-client не сконфигурирован", ref)
	}

	body := ref
	var field string
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		body, field = ref[:i], ref[i+1:]
		if field == "" {
			return nil, fmt.Errorf("render: vault-ref %q: пустое имя поля после '#'", ref)
		}
	}

	logical, err := vault.ParseRef(body)
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	data, err := vc.ReadKV(ctx, logical)
	if err != nil {
		// NIM-73: путь в ПЛОСКОЙ форме (logical, без `vault:`-префикса) — actionable-
		// диагностика, переживающая observability-маскинг (audit.vaultRefRe ловит
		// только `vault:<mount>/`). У ненайденного секрета значения нет → утечки нет.
		// Симметрия с shared/cel.callVault. `%w` сохраняет цепочку ErrVaultKVNotFound.
		if field != "" {
			return nil, fmt.Errorf("render: секрет %s#%s не резолвится: %w", logical, field, err)
		}
		return nil, fmt.Errorf("render: секрет %s не резолвится: %w", logical, err)
	}

	if field == "" {
		return data, nil
	}
	val, ok := data[field]
	if !ok {
		// Путь+имя поля — actionable (какое поле досеять), не секрет-значение
		// (значения других полей в текст не идут). Плоская форма (NIM-73).
		return nil, fmt.Errorf("render: в секрете %s нет поля %q", logical, field)
	}
	return val, nil
}
