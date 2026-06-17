//go:build e2e

package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// validApplyRunsStatus — закрытое множество значений apply_runs.status,
// разрешённых harness-у в YAML expectations.
//
// Источник правды — keeper/internal/applyrun/applyrun.go (Status const-ы +
// ValidStatus). Дублируется здесь литералом потому, что tests/e2e — отдельный
// go-модуль без зависимости на keeper/ (deps testcontainers в pilot не утекают
// в основные модули). В L3a-implementation slice — заменить на импорт
// `github.com/souls-guild/soul-stack/keeper/internal/applyrun` через replace в
// go.mod и `applyrun.ValidStatus(...)` вместо литерала. Drift между ними
// ловится в smoke_nginx_test.go — TestValidApplyRunsStatusInSyncWithKeeper.
//
// ADR-039(4): fail-early на старте теста, если expectation указывает невалидный
// status (опечатка вроде "succeeded" / "done" сразу видна, не на assert-фазе).
var validApplyRunsStatus = map[string]struct{}{
	"planned":    {},
	"claimed":    {},
	"running":    {},
	"dispatched": {},
	"success":    {},
	"failed":     {},
	"cancelled":  {},
	"orphaned":   {},
	"no_match":   {},
}

// CheckApplyRunsStatusValid валидирует, что строка из YAML-expectation —
// разрешённое enum-значение apply_runs.status; фейлит t, если нет. Не выполняет
// assert против БД (для этого AssertApplyRunsStatus).
func CheckApplyRunsStatusValid(t *testing.T, status string) {
	t.Helper()
	if !IsValidApplyRunsStatus(status) {
		known := ValidApplyRunsStatuses()
		t.Fatalf("неизвестное значение apply_runs.status %q в expectations; разрешены: %v", status, known)
	}
}

// IsValidApplyRunsStatus — pure-проверка без testing.TB. Используется
// drift-тестами smoke_nginx_test.go, которым нужен булев результат без
// побочного fail-а текущего теста.
func IsValidApplyRunsStatus(status string) bool {
	_, ok := validApplyRunsStatus[status]
	return ok
}

// ValidApplyRunsStatuses возвращает копию map-а валидных значений для
// drift-теста (smoke_nginx_test.go::TestValidApplyRunsStatusInSyncWithKeeper).
// Возвращаем slice, не map (упорядоченное сравнение проще).
func ValidApplyRunsStatuses() []string {
	out := make([]string, 0, len(validApplyRunsStatus))
	for k := range validApplyRunsStatus {
		out = append(out, k)
	}
	return out
}

// AssertApplyRunsStatus читает строки apply_runs по applyID из PG и фейлит,
// если хотя бы одна не равна expected.
//
// PK apply_runs = (apply_id, sid) → один прогон даёт N строк (по числу Soul-
// хостов). harness требует success ВСЕХ — half-applied result в expectations
// MVP не моделируется (multi-host фейлы оставляют сразу error_locked).
func (s *Stack) AssertApplyRunsStatus(t *testing.T, applyID string, expected string) {
	t.Helper()
	CheckApplyRunsStatusValid(t, expected)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := s.db.Query(ctx,
		"SELECT sid, status FROM apply_runs WHERE apply_id = $1", applyID)
	if err != nil {
		t.Fatalf("AssertApplyRunsStatus %s: query: %v", applyID, err)
	}
	defer rows.Close()

	statuses := map[string]string{}
	for rows.Next() {
		var sid, st string
		if err := rows.Scan(&sid, &st); err != nil {
			t.Fatalf("AssertApplyRunsStatus %s: scan: %v", applyID, err)
		}
		statuses[sid] = st
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertApplyRunsStatus %s: rows.Err: %v", applyID, err)
	}
	if len(statuses) == 0 {
		t.Fatalf("AssertApplyRunsStatus %s: ни одной строки apply_runs", applyID)
	}
	for sid, st := range statuses {
		if st != expected {
			t.Fatalf("AssertApplyRunsStatus %s: sid=%s status=%q, ожидался %q (полная матрица=%v)",
				applyID, sid, st, expected, statuses)
		}
	}
}

// AssertIncarnationState читает incarnation.state из БД и фейлит, если
// jsonb-payload не содержит expectedSubset (deep-subset сравнение).
func (s *Stack) AssertIncarnationState(t *testing.T, name string, expectedSubset map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stateJSON []byte
	err := s.db.QueryRow(ctx,
		"SELECT state FROM incarnation WHERE name = $1", name).Scan(&stateJSON)
	if err != nil {
		t.Fatalf("AssertIncarnationState %s: query: %v", name, err)
	}
	// state может быть NULL (новый incarnation без apply) — на этой ступени
	// expectations всё равно не сматчатся, выводим понятную диагностику.
	if len(stateJSON) == 0 || string(stateJSON) == "null" {
		t.Fatalf("AssertIncarnationState %s: state пуст, ожидался subset=%v", name, expectedSubset)
	}
	var actual map[string]any
	if err := json.Unmarshal(stateJSON, &actual); err != nil {
		t.Fatalf("AssertIncarnationState %s: unmarshal state: %v (raw=%s)", name, err, string(stateJSON))
	}
	if !subsetMatches(actual, expectedSubset) {
		t.Fatalf("AssertIncarnationState %s: state не содержит subset\nactual=%v\nexpected_subset=%v",
			name, actual, expectedSubset)
	}
}

// AssertAuditEvent ищет в audit_log хотя бы одну строку с event_type=eventType
// и payload, содержащим expectedPayload subset. Если ни одной — фейлит.
//
// Реализовано через jsonb-оператор `@>` (subset-match), эквивалентный deep-
// subset-сравнению на Go-стороне. Никаких ARRAY_CONTAINS — payload-subset
// объявленных полей.
func (s *Stack) AssertAuditEvent(t *testing.T, eventType string, expectedPayload map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	subsetJSON, err := json.Marshal(expectedPayload)
	if err != nil {
		t.Fatalf("AssertAuditEvent: marshal expected payload: %v", err)
	}

	var count int
	err = s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = $1 AND payload @> $2::jsonb
	`, eventType, string(subsetJSON)).Scan(&count)
	if err != nil {
		t.Fatalf("AssertAuditEvent %s: query: %v", eventType, err)
	}
	if count == 0 {
		// Дамп всех событий типа eventType для диагностики (не больше 10 строк).
		rows, derr := s.db.Query(ctx,
			"SELECT payload FROM audit_log WHERE event_type = $1 ORDER BY created_at DESC LIMIT 10",
			eventType)
		var dumps []string
		if derr == nil {
			defer rows.Close()
			for rows.Next() {
				var p []byte
				if err := rows.Scan(&p); err == nil {
					dumps = append(dumps, string(p))
				}
			}
		}
		t.Fatalf("AssertAuditEvent %s: payload subset не найден\nexpected=%s\nrecent_events=%v",
			eventType, string(subsetJSON), dumps)
	}
}

// AssertMetricGE скрейпит /metrics Keeper-а (отдельный listener,
// Stack.MetricsURL) и проверяет, что значение метрики `metric` >= минимума.
//
// metric — Prometheus-выражение из expectations.yaml вида
// `keeper_apply_runs_total{status="success"}` или bare-name `keeper_xxx_total`.
// Поддерживается только сумма по матчящим строкам (counter/gauge); histogram-
// decomposition в MVP не нужен.
func (s *Stack) AssertMetricGE(t *testing.T, metric string, minimum float64) {
	t.Helper()
	if s.MetricsURL == "" {
		t.Fatal("AssertMetricGE: Stack.MetricsURL пуст (NewStack не отработал?)")
	}
	resp, err := http.Get(s.MetricsURL + "/metrics")
	if err != nil {
		t.Fatalf("AssertMetricGE %s: scrape: %v", metric, err)
	}
	defer resp.Body.Close()

	body, err := readAllLimited(resp.Body, 8*1024*1024)
	if err != nil {
		t.Fatalf("AssertMetricGE %s: read body: %v", metric, err)
	}
	actual, found := parsePrometheusSum(body, metric)
	if !found {
		t.Fatalf("AssertMetricGE %s: метрика не найдена в /metrics", metric)
	}
	if actual < minimum {
		t.Fatalf("AssertMetricGE %s = %v, ожидалось >= %v", metric, actual, minimum)
	}
}

// readAllLimited читает body до limitBytes. Защита от случайного скрейпа
// гигантского /metrics в тестовой среде.
func readAllLimited(r interface{ Read(p []byte) (int, error) }, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 4096)
	for {
		if int64(buf.Len()) > limit {
			return nil, fmt.Errorf("body exceeds %d bytes", limit)
		}
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			// io.EOF не импортируем — сравниваем имя из net/http body-reader-а.
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// parsePrometheusSum суммирует значения всех строк exposition-format, имя
// метрики которых = bareName(query) и набор лейблов содержит все label=value
// пары из query.
//
// query грамматика (упрощённо):
//
//	<bare>                              — bare-counter/gauge.
//	<bare>{label1="v1",label2="v2"}     — c фильтром по лейблам.
//
// Возвращает (sum, true) если матчнулась хотя бы одна строка. Игнорирует
// `# HELP` / `# TYPE` строки и строки с label-set-ом, не содержащим все
// фильтры (но в Prometheus convention бывают доп. лейблы — мы не строгий
// match, а subset, как `@>`-jsonb).
func parsePrometheusSum(body []byte, query string) (float64, bool) {
	name, filters, ok := parseQuery(query)
	if !ok {
		return 0, false
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var sum float64
	matched := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		ln, lbls, val, ok := parseExpositionLine(line)
		if !ok {
			continue
		}
		if ln != name {
			continue
		}
		if !labelsContain(lbls, filters) {
			continue
		}
		sum += val
		matched = true
	}
	return sum, matched
}

// parseQuery разделяет `name{...}` на (name, label-filters).
func parseQuery(q string) (name string, filters map[string]string, ok bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", nil, false
	}
	br := strings.IndexByte(q, '{')
	if br < 0 {
		return q, nil, true
	}
	if !strings.HasSuffix(q, "}") {
		return "", nil, false
	}
	name = strings.TrimSpace(q[:br])
	inner := q[br+1 : len(q)-1]
	filters = map[string]string{}
	if strings.TrimSpace(inner) == "" {
		return name, filters, true
	}
	// Простой парсер `k="v",k2="v2"`. Не покрывает escape — для MVP достаточно.
	for _, part := range splitTopLevelCommas(inner) {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			return "", nil, false
		}
		k := strings.TrimSpace(part[:eq])
		v := strings.TrimSpace(part[eq+1:])
		v = strings.Trim(v, `"`)
		filters[k] = v
	}
	return name, filters, true
}

// splitTopLevelCommas разделяет по запятым вне кавычек.
func splitTopLevelCommas(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			inQ = !inQ
			cur.WriteByte(c)
		case ',':
			if inQ {
				cur.WriteByte(c)
			} else {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseExpositionLine разбирает строку Prometheus exposition format:
//
//	<name>[{label1="v1",label2="v2"}] <value> [timestamp]
//
// Возвращает (name, labels, value, ok). Игнорирует трейлинг-timestamp.
func parseExpositionLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	// Разбиваем по первому пробелу справа: `<metric-block> <value> [ts]`.
	idx := strings.LastIndex(line, " ")
	if idx < 0 {
		return "", nil, 0, false
	}
	left := strings.TrimSpace(line[:idx])
	right := strings.TrimSpace(line[idx+1:])

	// Возможен trailing timestamp: `name 1.0 1717000000000`. Тогда reparse:
	// если right содержит пробел или left заканчивается на число — split-им.
	if v, err := strconv.ParseFloat(right, 64); err == nil {
		// прямой случай: всё ок, value=right.
		_ = v
	} else {
		// right не число — может быть timestamp; пробуем сдвинуть split.
		idx2 := strings.LastIndex(left, " ")
		if idx2 < 0 {
			return "", nil, 0, false
		}
		newRight := strings.TrimSpace(left[idx2+1:])
		left = strings.TrimSpace(left[:idx2])
		right = newRight
	}

	value, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return "", nil, 0, false
	}

	// left = `name` или `name{labels}`.
	br := strings.IndexByte(left, '{')
	if br < 0 {
		return left, nil, value, true
	}
	if !strings.HasSuffix(left, "}") {
		return "", nil, 0, false
	}
	name = left[:br]
	inner := left[br+1 : len(left)-1]
	labels = map[string]string{}
	for _, part := range splitTopLevelCommas(inner) {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(part[:eq])
		v := strings.TrimSpace(part[eq+1:])
		v = strings.Trim(v, `"`)
		labels[k] = v
	}
	return name, labels, value, true
}

// labelsContain — все пары из want присутствуют в have с тем же значением.
func labelsContain(have, want map[string]string) bool {
	for k, v := range want {
		got, ok := have[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// subsetMatches — рекурсивный deep-subset для map[string]any. Поддерживает:
//
//   - map[string]any (рекурсивно);
//   - []any (точное упорядоченное равенство через reflect.DeepEqual);
//   - примитивы (через reflect.DeepEqual + EqualNumbers-нормализация
//     int↔float64, т.к. JSON-decoded числа всегда float64, а вызывающая
//     сторона часто пишет int-литералы).
func subsetMatches(actual, expected map[string]any) bool {
	for k, ev := range expected {
		av, ok := actual[k]
		if !ok {
			return false
		}
		if !valueMatches(av, ev) {
			return false
		}
	}
	return true
}

func valueMatches(actual, expected any) bool {
	// Если expected — map, рекурсивно.
	if em, ok := expected.(map[string]any); ok {
		am, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		return subsetMatches(am, em)
	}
	// Нормализация чисел.
	if af, ok := toFloat(actual); ok {
		if ef, ok := toFloat(expected); ok {
			return af == ef
		}
	}
	// Сравнение упорядоченных слайсов — DeepEqual после нормализации чисел.
	if es, ok := expected.([]any); ok {
		as, ok := actual.([]any)
		if !ok || len(as) != len(es) {
			return false
		}
		for i := range es {
			if !valueMatches(as[i], es[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(actual, expected)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	default:
		return 0, false
	}
}

// sortedKeys — helper для стабильных диагностических сообщений.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = sortedKeys // зарезервировано под диагностику будущих assert-ов
