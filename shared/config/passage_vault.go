package config

import "regexp"

// vault-secrets-generated passage-ось (ADR-056 amendment, Вариант A).
//
// Зачем. Сценарий-генератор секретов (redis create / create_from_souls) сам генерит
// недостающие пароли в Vault keeper-шагом `core.vault.kv-present` (generate-if-absent),
// а последующие деплой-задачи читают те же секреты через `${ vault(...) }` в
// passage-определяющих полях (params / apply.input / vars / ...). Запись секрета и его
// чтение ОБЯЗАНЫ разойтись по Passage: render деплой-задачи (vault_resolve-фаза,
// ADR-010) читает Vault Keeper-side ДО dispatch — если он окажется в одном Passage с
// generate-шагом, секрета в Vault ещё нет → render_failed (vault_resolve на
// несуществующем пути).
//
// ★ БЛОКЕР (live-баг create_from_souls). roster-ось (passage_refresh.go) держала этот
// порядок ТОЛЬКО при наличии refresh-эмиттера: provision-тело create/ уводило
// read-задачи в поздний Passage. В create_from_souls provision-тела НЕТ (деплой по
// уже онбордившемуся roster-у) → refresh-эмиттера нет → roster-ось не активна →
// generate и vault()-read схлопывались в Passage 0 → render_failed. Поэтому
// `core.vault.kv-present`-эмиттер — отдельный класс PASSAGE-ОПРЕДЕЛЯЮЩЕГО сигнала
// «vault-secrets-generated», симметрично refresh-эмиттеру (только сигнал — vault-ось,
// не register-ось и не roster-ось).
//
// Механизм. vault-эмиттер — задача `core.vault.kv-present` (она пишет секреты по
// targets). vault-потребитель — задача, читающая `${ vault(...) }` в любом
// passage-определяющем поле (тот же реестр ADR-056, что collectTaskReads:
// where / vars / params / apply.input / output / loop.items / loop.when; flow-control
// when/changed_when/failed_when — НЕ входит, он Soul-side per-task gating). Любой
// vault-потребитель после vault-эмиттера (program-order) едет в Passage ≥ 1 + passage
// эмиттера. Ребро вшито в visit() (passage.go) ТРЕТЬИМ классом рядом с register/roster.
//
// Over-approximation в безопасную сторону. Статический матч путей targets↔vault-path
// НЕВЫРАЗИМ: targets — сложный CEL (конкатенация incarnation.name + per-user map), а
// vault-путь в `vault('...')` — тоже конкатенация. Поэтому ребро строится грубо: ЛЮБОЙ
// kv-present-эмиттер → ЛЮБАЯ vault()-read, без сверки путей. Лишний Passage безопасен
// (+1 максимум); пропущенный = render_failed. Цикл невозможен: kv-present сам vault()
// НЕ читает (пишет по targets, не интерполирует секрет), поэтому vault-эмиттер никогда
// не оказывается vault-потребителем — ребро строго направлено vault-эмиттер→read.
//
// Не register-граф (как и roster-ось): vault-граница НЕ вводит register-ссылок,
// поэтому инвариант reads⊆refs ADR-056 не затрагивается (третья ортогональная ось).

// vaultEmitterModuleAddr — единственный модуль-носитель vault-генерации
// (core.vault.kv-present, ADR-017 keeper-side core: verb-форма write-if-absent).
// Author-форма адреса задачи — base+state.
const vaultEmitterModuleAddr = "core.vault.kv-present"

// reVaultRead — вызов CEL-builtin `vault(...)` (ADR-010: единственный builtin чтения
// секрета). Граница слева — начало строки ИЛИ не-id/dot-символ (чтобы `myvault(` и
// `obj.vault(` НЕ матчились: `vault` обязан быть корневым идентификатором, как в
// грамматике CEL-контекста). Открывающая скобка обязательна — отличает вызов от
// идентификатора `vault` в чужом контексте.
var reVaultRead = regexp.MustCompile(`(^|[^A-Za-z0-9_.])vault\(`)

// taskIsVaultEmitter — задача эмитит сигнал «vault-secrets-generated»: это
// `core.vault.kv-present` (пишет секреты по targets). Адреса достаточно — модуль
// семантически write-if-absent по targets, отдельного флага-дискриминатора (как
// refresh_soulprint у roster-оси) тут нет: ЛЮБОЙ kv-present шаг пишет секреты.
func taskIsVaultEmitter(t *Task) bool {
	return t.Module != nil && t.Module.Module == vaultEmitterModuleAddr
}

// taskReadsVaultSecret — задача статически читает секрет через `${ vault(...) }` в
// любом passage-определяющем поле (реестр ADR-056: where / vars / params /
// apply.input / output / loop.items / loop.when). Рекурсивно через block: (block —
// атомарная единица Passage; vault()-чтение любого потомка делает контейнер
// vault-потребителем).
//
// Flow-control CEL (when / changed_when / failed_when / retry.until) СЮДА НЕ входит —
// он НЕ passage-определяющий (Soul-side per-task gating, ADR-012(d)). Но `${ vault() }`
// в flow-control и не валиден: vault() резолвится Keeper-side в vault_resolve-фазе ДО
// dispatch, а flow-control исполняется Soul-side — там vault() недоступен. Поэтому
// исключение flow-control из vault-оси не теряет реальных кейсов (симметрия
// register/roster-осей).
func taskReadsVaultSecret(t *Task) bool {
	if exprReadsVault(t.Where) {
		return true
	}
	if t.Loop != nil && (exprReadsVault(t.Loop.When) || valueReadsVault(t.Loop.Items)) {
		return true
	}
	if mapReadsVault(t.Vars) || mapReadsVault(t.Output) {
		return true
	}
	if t.Module != nil && mapReadsVault(t.Module.Params) {
		return true
	}
	if t.Apply != nil && mapReadsVault(t.Apply.Input) {
		return true
	}
	if t.Block != nil {
		for i := range t.Block.Block {
			if taskReadsVaultSecret(&t.Block.Block[i]) {
				return true
			}
		}
	}
	return false
}

// exprReadsVault — CEL-строка зовёт `vault(...)`. Строковые литералы CEL вырезаются
// тем же celStringLiteral, что и exprReadsSoulprint/ExtractRegisterRefs — иначе
// `'vault('` внутри строковых ДАННЫХ (например, секрет-путь-литерал в комментарии или
// сообщении) дал бы ложное ребро. Один источник правды грамматики «строковый литерал
// CEL».
func exprReadsVault(expr string) bool {
	if expr == "" {
		return false
	}
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	return reVaultRead.MatchString(stripped)
}

// mapReadsVault — любое строковое значение map (vars/params/apply.input/output),
// рекурсивно по вложенным map/seq, зовёт vault() в `${ … }`-интерполяции.
func mapReadsVault(m map[string]any) bool {
	for _, v := range m {
		if valueReadsVault(v) {
			return true
		}
	}
	return false
}

// valueReadsVault рекурсивно обходит any-значение (string / map / seq).
func valueReadsVault(v any) bool {
	switch t := v.(type) {
	case string:
		return exprReadsVault(t)
	case map[string]any:
		for _, sub := range t {
			if valueReadsVault(sub) {
				return true
			}
		}
	case []any:
		for _, sub := range t {
			if valueReadsVault(sub) {
				return true
			}
		}
	}
	return false
}
