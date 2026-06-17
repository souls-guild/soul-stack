package essence

// mergeInto применяет слой over поверх base деструктивно (мутирует base) и
// возвращает результат. Семантика deep-merge (PM-decision 2):
//
//   - map + map  → рекурсивный merge (later override earlier);
//   - всё прочее → over заменяет значение целиком (скаляры и списки — replace,
//     НЕ append).
//
// base всегда — наш собственный аккумулятор (создаётся в pipeline), поэтому
// мутация безопасна; значения из over не разделяются по ссылке с входными
// слоями только на верхнем уровне map'ов — вложенные map'ы пере-используются,
// что приемлемо, так как слои после merge не читаются повторно.
func mergeInto(base, over map[string]any) map[string]any {
	if base == nil {
		base = make(map[string]any, len(over))
	}
	for k, ov := range over {
		bv, exists := base[k]
		if !exists {
			base[k] = ov
			continue
		}
		bm, bok := bv.(map[string]any)
		om, ook := ov.(map[string]any)
		if bok && ook {
			base[k] = mergeInto(bm, om)
			continue
		}
		base[k] = ov
	}
	return base
}
