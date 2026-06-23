package cel

import (
	"errors"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestMerge_LastWins — правый аргумент перекрывает левый по совпавшему ключу
// верхнего уровня; несовпавшие ключи объединяются.
func TestMerge_LastWins(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.a, input.b) }`,
		Vars{Input: map[string]any{
			"a": map[string]any{"x": int64(1), "y": int64(2)},
			"b": map[string]any{"y": int64(20), "z": int64(30)},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("результат типа %T, want map[string]any", out)
	}
	want := map[string]any{"x": int64(1), "y": int64(20), "z": int64(30)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %v, want %v (last-wins)", got, want)
	}
}

// TestMerge_Shallow — вложенный map НЕ сливается глубоко: правый аргумент
// целиком замещает значение совпавшего верхнего ключа (даже если оно — map).
func TestMerge_Shallow(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.a, input.b) }`,
		Vars{Input: map[string]any{
			"a": map[string]any{"nested": map[string]any{"keep": int64(1), "drop": int64(2)}},
			"b": map[string]any{"nested": map[string]any{"keep": int64(99)}},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	nested, ok := got["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested типа %T, want map[string]any", got["nested"])
	}
	// SHALLOW: правый целиком заместил — ключа `drop` из левого больше нет.
	want := map[string]any{"keep": int64(99)}
	if !reflect.DeepEqual(nested, want) {
		t.Fatalf("nested = %v, want %v (shallow, правый целиком замещает)", nested, want)
	}
}

// TestMerge_EmptyMaps — пустые map-ы: merge пустых даёт пустой; пустой не
// затирает непустой.
func TestMerge_EmptyMaps(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.empty, input.filled, input.empty2) }`,
		Vars{Input: map[string]any{
			"empty":  map[string]any{},
			"filled": map[string]any{"k": "v"},
			"empty2": map[string]any{},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{"k": "v"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge с пустыми = %v, want %v", got, want)
	}

	// merge двух пустых → пустой map.
	out2, err := e.EvalInterpolation(`${ merge(input.empty, input.empty2) }`, Vars{Input: map[string]any{
		"empty":  map[string]any{},
		"empty2": map[string]any{},
	}})
	if err != nil {
		t.Fatalf("eval (оба пустые): %v", err)
	}
	if got2 := out2.(map[string]any); len(got2) != 0 {
		t.Fatalf("merge двух пустых = %v, want пустой map", got2)
	}
}

// TestMerge_SingleArg — один аргумент: merge возвращает его копию (валидная
// вариадик-форма с минимумом 1 аргумент).
func TestMerge_SingleArg(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`${ merge(input.a) }`, Vars{Input: map[string]any{
		"a": map[string]any{"x": int64(1), "y": int64(2)},
	}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{"x": int64(1), "y": int64(2)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(один) = %v, want %v", got, want)
	}
}

// TestMerge_ManyArgs — >2 аргументов: слияние слева направо, последний бьёт всех.
func TestMerge_ManyArgs(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.a, input.b, input.c, input.d) }`,
		Vars{Input: map[string]any{
			"a": map[string]any{"k": int64(1), "a_only": "A"},
			"b": map[string]any{"k": int64(2), "b_only": "B"},
			"c": map[string]any{"k": int64(3), "c_only": "C"},
			"d": map[string]any{"k": int64(4), "d_only": "D"},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{
		"k": int64(4), "a_only": "A", "b_only": "B", "c_only": "C", "d_only": "D",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(4) = %v, want %v", got, want)
	}
}

// TestMergeList_Flatten — форма merge(list(map)): один аргумент-список map-ов
// flatten-ится слева направо, last-wins (правый элемент списка бьёт левый).
func TestMergeList_Flatten(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.layers) }`,
		Vars{Input: map[string]any{
			"layers": []any{
				map[string]any{"k": int64(1), "a_only": "A"},
				map[string]any{"k": int64(2), "b_only": "B"},
				map[string]any{"k": int64(3), "c_only": "C"},
			},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("результат типа %T, want map[string]any", out)
	}
	want := map[string]any{"k": int64(3), "a_only": "A", "b_only": "B", "c_only": "C"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(list) = %v, want %v (flatten left-to-right last-wins)", got, want)
	}
}

// TestMergeList_FromComprehension — основной use-case: коллекция приходит из
// .map(...) над map (CEL-comprehension даёт СПИСОК map-ов), merge(list) сворачивает
// её в map «имя→объект». Это паттерн детерминированного users.acl: список из
// .map() → map, который шаблон range-ит по сортированным ключам.
func TestMergeList_FromComprehension(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.users.map(name, {name: {'perms': input.users[name].perms}})) }`,
		Vars{Input: map[string]any{
			"users": map[string]any{
				"zeta":  map[string]any{"perms": "~* +@all"},
				"alpha": map[string]any{"perms": "~app:* +@read"},
			},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("результат типа %T, want map[string]any", out)
	}
	want := map[string]any{
		"zeta":  map[string]any{"perms": "~* +@all"},
		"alpha": map[string]any{"perms": "~app:* +@read"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(comprehension) = %v, want %v", got, want)
	}
}

// TestMergeList_LastWinsWithinList — внутри списка last-wins: одинаковый ключ в
// разных элементах списка → последний бьёт.
func TestMergeList_LastWinsWithinList(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.layers) }`,
		Vars{Input: map[string]any{
			"layers": []any{
				map[string]any{"dup": "first", "x": int64(1)},
				map[string]any{"dup": "second"},
				map[string]any{"dup": "third", "y": int64(2)},
			},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{"dup": "third", "x": int64(1), "y": int64(2)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(list) last-wins = %v, want %v", got, want)
	}
}

// TestMergeList_Empty — пустой список → пустой map (валидная вырожденная форма,
// прецедент: users пуст → users.acl без юзеров).
func TestMergeList_Empty(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`${ merge(input.empty) }`, Vars{Input: map[string]any{
		"empty": []any{},
	}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("результат типа %T, want map[string]any", out)
	}
	if len(got) != 0 {
		t.Fatalf("merge(пустой список) = %v, want пустой map", got)
	}
}

// TestMergeList_NonMapElement — элемент списка не-map → внятная ошибка (не
// молчаливое проглатывание). Любой класс ошибки приемлем.
func TestMergeList_NonMapElement(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalExpression(`merge(input.layers)`, Vars{Input: map[string]any{
		"layers": []any{
			map[string]any{"x": int64(1)},
			"i-am-a-string",
		},
	}})
	if err == nil {
		t.Fatal("merge(list) с не-map элементом: ожидалась ошибка, получено nil")
	}
	var ce *ErrCompile
	var ee *ErrEval
	if !errors.As(err, &ce) && !errors.As(err, &ee) {
		t.Fatalf("merge(list) не-map элемент: ошибка = %v, want *ErrCompile или *ErrEval", err)
	}
}

// TestMergeList_AvailableInFlowControl — list-форма merge() так же доступна в
// Soul-side flow-control sandbox (та же pure-функция, тот же env).
func TestMergeList_AvailableInFlowControl(t *testing.T) {
	e := newFlowControlEngine(t)

	out, err := e.EvalExpression(
		`merge(register.layers).k == "v2"`,
		Vars{Register: map[string]any{
			"layers": []any{
				map[string]any{"k": "v1"},
				map[string]any{"k": "v2"},
			},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("результат = %v, want true (list last-wins в flow-control)", got)
	}
}

// TestMerge_VarargsBackCompat — additive-гарантия: varargs-форма merge(m, m...)
// не сломана введением list-overload-а. Прямой регресс-guard back-compat.
func TestMerge_VarargsBackCompat(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.a, input.b) }`,
		Vars{Input: map[string]any{
			"a": map[string]any{"x": int64(1)},
			"b": map[string]any{"x": int64(2), "y": int64(3)},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{"x": int64(2), "y": int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("varargs merge = %v, want %v (back-compat сломан)", got, want)
	}
}

// TestMerge_TooManyVarargs — КОНТРАКТ merge (помечен, QA-пробел 2026-06-22):
// varargs-форма объявлена для 1..mergeMaxArity (=8) map-аргументов. >8 аргументов
// → compile-ошибка no-such-overload (расширяемо без breaking change добавлением
// overload-ов). Для коллекций произвольного размера предусмотрена форма
// merge(list(map)) — она НЕ ограничена арностью. Тест фиксирует границу 8: если
// нужно слить 9+ слоёв, оборачивай их в список и зови merge(list).
func TestMerge_TooManyVarargs(t *testing.T) {
	e := newEngine(t)

	// 9 map-аргументов: mergeMaxArity=8 → нет overload-а на 9.
	expr := `merge(input.a, input.b, input.c, input.d, input.e, input.f, input.g, input.h, input.i)`
	in := map[string]any{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		in[k] = map[string]any{k: int64(1)}
	}
	_, err := e.EvalExpression(expr, Vars{Input: in})
	if err == nil {
		t.Fatal("merge(9 аргументов): ожидалась compile-ошибка no-such-overload, получено nil")
	}
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("merge(9): ошибка = %v, want *ErrCompile (no such overload)", err)
	}
}

// TestMerge_NonMapArg — не-map аргумент → ошибка (compile-no-such-overload для
// статически известного типа, либо eval-ошибка при dyn-склейке). Любой класс
// ошибки приемлем — главное, что merge не молча проглатывает не-map.
func TestMerge_NonMapArg(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalExpression(`merge(input.a, input.notmap)`, Vars{Input: map[string]any{
		"a":      map[string]any{"x": int64(1)},
		"notmap": "i-am-a-string",
	}})
	if err == nil {
		t.Fatal("merge с не-map аргументом: ожидалась ошибка, получено nil")
	}
	var ce *ErrCompile
	var ee *ErrEval
	if !errors.As(err, &ce) && !errors.As(err, &ee) {
		t.Fatalf("merge с не-map: ошибка = %v, want *ErrCompile или *ErrEval", err)
	}
}

// TestMerge_AvailableInFlowControl — Soul-side flow-control sandbox ([ADR-012(d)])
// merge() получает: pure-функция без внешнего контекста, симметрия с
// scenario-выражениями.
func TestMerge_AvailableInFlowControl(t *testing.T) {
	e := newFlowControlEngine(t)

	out, err := e.EvalExpression(
		`merge(register.a, register.b).k == "v2"`,
		Vars{Register: map[string]any{
			"a": map[string]any{"k": "v1"},
			"b": map[string]any{"k": "v2"},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("результат = %v, want true (last-wins в flow-control)", got)
	}
}

// TestMerge_SecretMaskedSameAsDirectVault — BLOCKER guard ([ADR-010 Amendment
// 2026-06-22], security/architect): секрет, попавший в merged-map через
// vault(), маскируется выходным слоем (shared/audit.MaskSecrets) ИДЕНТИЧНО
// прямому ${ vault(...) } — НЕ протекает в логи/OTel/RunResult.
//
// Механизм маскинга: vault() резолвится в реальный plaintext keeper-side (Soul
// получает настоящий секрет), а маскинг — на выходе по (а) sensitive-имени ключа
// назначения и (б) vault-ref-маркеру. merge() сохраняет ключи верхнего уровня без
// переименования, поэтому секрет под ключом `password` маскируется так же, как
// при прямой подстановке. Тест доказывает обе ветви через MaskSecrets.
func TestMerge_SecretMaskedSameAsDirectVault(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t-plaintext"},
	}}
	e := newVaultEngine(t, kv)

	// Прямой ${ vault(...) } под ключом `password` — эталон маскинга.
	direct, err := e.EvalInterpolation("${ vault('secret/redis/admin#password') }", Vars{})
	if err != nil {
		t.Fatalf("eval direct vault: %v", err)
	}
	if direct != "s3cr3t-plaintext" {
		t.Fatalf("direct vault резолвнул %v, want реальный plaintext (резолв keeper-side)", direct)
	}
	directPayload := map[string]any{"password": direct}
	maskedDirect := audit.MaskSecrets(directPayload)
	if maskedDirect["password"] == "s3cr3t-plaintext" {
		t.Fatal("эталон: прямой vault-секрет НЕ замаскирован MaskSecrets — слой маскинга сломан")
	}

	// Тот же секрет, но через merge(defaults, {password: vault(...)}).
	merged, err := e.EvalInterpolation(
		`${ merge(input.defaults, {'password': vault('secret/redis/admin#password')}) }`,
		Vars{Input: map[string]any{
			"defaults": map[string]any{"maxmemory": "256mb", "appendonly": "yes"},
		}},
	)
	if err != nil {
		t.Fatalf("eval merge+vault: %v", err)
	}
	mergedMap, ok := merged.(map[string]any)
	if !ok {
		t.Fatalf("merge результат типа %T, want map[string]any", merged)
	}
	// Контроль: секрет реально попал в merged-map в plaintext (до маскинга).
	if mergedMap["password"] != "s3cr3t-plaintext" {
		t.Fatalf("merged.password = %v, want plaintext-секрет (vault резолвится в merge)", mergedMap["password"])
	}

	maskedMerged := audit.MaskSecrets(mergedMap)
	// Главное утверждение: merged-секрет замаскирован ИДЕНТИЧНО прямому.
	if maskedMerged["password"] != maskedDirect["password"] {
		t.Fatalf("merged.password замаскирован как %v, прямой как %v — РАСХОЖДЕНИЕ (секрет течёт через merge)",
			maskedMerged["password"], maskedDirect["password"])
	}
	// И буквально: plaintext-секрета в замаскированном выводе нет.
	if maskedMerged["password"] == "s3cr3t-plaintext" {
		t.Fatal("merged.password НЕ замаскирован — секрет протекает в выходной слой через merge()")
	}
	// Несекретные ключи merged-map проходят насквозь (over-masking нет).
	if maskedMerged["maxmemory"] != "256mb" || maskedMerged["appendonly"] != "yes" {
		t.Fatalf("несекретные ключи замаскированы: %v (over-masking)", maskedMerged)
	}
}

// TestMerge_TLSKeyMaskedSameAsDirectVault — BLOCKER masking-guard (redis-
// консолидация TLS): PEM client-key, попавший в merged-map через
// merge(defaults, {tls_key: vault(...)}), маскируется выходным слоем
// (shared/audit.MaskSecrets) ИДЕНТИЧНО прямому ${ vault(...) } под ключом
// tls_key — НЕ протекает в логи/OTel/RunResult. Доказывает, что merge() границу
// маскинга для TLS PEM не расширяет/не сужает: tls_key — sensitive-имя ключа
// (sensitiveKeyRe расширен фрагментом tls[_-]?(key|cert|ca)), значит и под merge,
// и при прямой подстановке маскинг одинаков. Класс merge-masking-guard.
func TestMerge_TLSKeyMaskedSameAsDirectVault(t *testing.T) {
	const pem = "-----BEGIN PRIVATE KEY-----\nMIIE-plaintext\n-----END PRIVATE KEY-----"
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/services/redis/tls": {"key": pem, "cert": "CERTPEM", "ca": "CAPEM"},
	}}
	e := newVaultEngine(t, kv)

	// Эталон: прямой ${ vault(...) } под ключом tls_key.
	direct, err := e.EvalInterpolation("${ vault('secret/services/redis/tls#key') }", Vars{})
	if err != nil {
		t.Fatalf("eval direct vault: %v", err)
	}
	if direct != pem {
		t.Fatalf("direct vault резолвнул %v, want PEM plaintext (keeper-side)", direct)
	}
	maskedDirect := audit.MaskSecrets(map[string]any{"tls_key": direct})
	if maskedDirect["tls_key"] == pem {
		t.Fatal("эталон: прямой tls_key НЕ замаскирован — слой маскинга TLS сломан (sensitiveKeyRe не ловит tls_key)")
	}

	// Тот же PEM через merge(defaults, {tls_key/tls_cert/tls_ca: vault(...)}).
	merged, err := e.EvalInterpolation(
		`${ merge(input.defaults, {
			'tls_key':  vault('secret/services/redis/tls#key'),
			'tls_cert': vault('secret/services/redis/tls#cert'),
			'tls_ca':   vault('secret/services/redis/tls#ca')
		}) }`,
		Vars{Input: map[string]any{
			"defaults": map[string]any{"tls-port": "7379"},
		}},
	)
	if err != nil {
		t.Fatalf("eval merge+vault: %v", err)
	}
	mergedMap := merged.(map[string]any)
	// Контроль: PEM реально попал в merged-map plaintext (до маскинга).
	if mergedMap["tls_key"] != pem {
		t.Fatalf("merged.tls_key = %v, want PEM plaintext (vault резолвится в merge)", mergedMap["tls_key"])
	}

	maskedMerged := audit.MaskSecrets(mergedMap)
	if maskedMerged["tls_key"] != maskedDirect["tls_key"] {
		t.Fatalf("merged.tls_key замаскирован как %v, прямой как %v — РАСХОЖДЕНИЕ (PEM течёт через merge)",
			maskedMerged["tls_key"], maskedDirect["tls_key"])
	}
	if maskedMerged["tls_key"] == pem {
		t.Fatal("merged.tls_key НЕ замаскирован — PEM client-key протекает через merge()")
	}
	// cert/ca тоже маскируются (secret-имена); несекретный tls-port — насквозь.
	if maskedMerged["tls_cert"] == "CERTPEM" || maskedMerged["tls_ca"] == "CAPEM" {
		t.Fatalf("tls_cert/tls_ca НЕ замаскированы: cert=%v ca=%v", maskedMerged["tls_cert"], maskedMerged["tls_ca"])
	}
	if maskedMerged["tls-port"] != "7379" {
		t.Fatalf("несекретный tls-port замаскирован: %v (over-masking)", maskedMerged["tls-port"])
	}
}

// TestMerge_SecretUnderNonSensitiveKeyNotMasked — НЕГАТИВНЫЙ guard инварианта
// границы: секрет, попавший в merged-map под НЕ-sensitive ключом, выходным слоем
// НЕ маскируется. vault() резолвится в plaintext keeper-side, в map ложится
// значение секрета (без `vault:`-маркера) — vault-ref-ветвь MaskSecrets не
// срабатывает, sensitive-ключевая ветвь не матчит несекретное имя. Это
// ОТВЕТСТВЕННОСТЬ АВТОРА СЦЕНАРИЯ (класть секрет под secret-именованный ключ), а
// не merge(). Тест фиксирует, что merge() границу маскинга НЕ расширяет и НЕ
// сужает — поведение симметрично прямому ${ vault(...) } под тем же ключом.
func TestMerge_SecretUnderNonSensitiveKeyNotMasked(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t-plaintext"},
	}}
	e := newVaultEngine(t, kv)

	// Эталон: прямой vault под НЕ-sensitive ключом `maxmemory` тоже НЕ
	// маскируется — merge() обязан вести себя так же.
	direct, err := e.EvalInterpolation("${ vault('secret/redis/admin#password') }", Vars{})
	if err != nil {
		t.Fatalf("eval direct vault: %v", err)
	}
	directPayload := map[string]any{"maxmemory": direct}
	if audit.MaskSecrets(directPayload)["maxmemory"] != "s3cr3t-plaintext" {
		t.Fatal("эталон: прямой vault под НЕ-sensitive ключом замаскирован — модель маскинга изменилась")
	}

	// Тот же секрет через merge() под НЕ-sensitive ключом `maxmemory`.
	merged, err := e.EvalInterpolation(
		`${ merge(input.defaults, {'maxmemory': vault('secret/redis/admin#password')}) }`,
		Vars{Input: map[string]any{
			"defaults": map[string]any{"appendonly": "yes"},
		}},
	)
	if err != nil {
		t.Fatalf("eval merge+vault: %v", err)
	}
	mergedMap := merged.(map[string]any)
	maskedMerged := audit.MaskSecrets(mergedMap)
	// Инвариант: под НЕ-sensitive ключом секрет проходит насквозь (merge не
	// добавляет маскинг, как и прямой vault). Корректность — на авторе сценария.
	if maskedMerged["maxmemory"] != "s3cr3t-plaintext" {
		t.Fatalf("merged.maxmemory = %v, want plaintext (несекретный ключ — merge не маскирует, симметрия с прямым vault)",
			maskedMerged["maxmemory"])
	}
}

// TestMerge_ZeroArgs — нижняя граница арности: merge() без аргументов →
// compile-ошибка no-such-overload (overload-ы объявлены для 1..mergeMaxArity,
// нулевой не объявлен). Симметрично верхней границе TestMerge_TooManyVarargs.
func TestMerge_ZeroArgs(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalExpression(`merge()`, Vars{})
	if err == nil {
		t.Fatal("merge() без аргументов: ожидалась compile-ошибка no-such-overload, получено nil")
	}
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("merge(): ошибка = %v, want *ErrCompile (no such overload)", err)
	}
}

// TestMerge_UndeclaredInMigration — migration-CEL ([ADR-019]) hermetic:
// merge() НЕ зарегистрирована (см. buildEngine). Вызов → compile-ошибка
// no such overload, симметрично glob()/vault()-guard-у миграционного env
// (минимум surface area, расширение требует отдельного ADR).
func TestMerge_UndeclaredInMigration(t *testing.T) {
	e := newMigrationEngine(t)

	_, err := e.EvalExpression(`merge(state.a, state.b)`, Vars{
		State: map[string]any{
			"a": map[string]any{"x": int64(1)},
			"b": map[string]any{"y": int64(2)},
		},
	})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("merge() в migration-env: ошибка = %v, want *ErrCompile (no such overload)", err)
	}
}
