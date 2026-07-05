//go:build e2e_live

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
// ValidStatus). Дублируется здесь литералом потому, что tests/e2e-live —
// отдельный go-модуль без зависимости на keeper/ (deps testcontainers в pilot
// не утекают в основные модули). Drift между ними ловится в smoke-тестах
// L3a/L3b — TestValidApplyRunsStatusInSyncWithKeeper.
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

// IsValidApplyRunsStatus — pure-проверка без testing.TB.
func IsValidApplyRunsStatus(status string) bool {
	_, ok := validApplyRunsStatus[status]
	return ok
}

// ValidApplyRunsStatuses возвращает копию map-а валидных значений для
// drift-теста.
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

// AssertApplyHostStatus проверяет статус apply_runs КОНКРЕТНОГО хоста (sid) —
// в отличие от AssertApplyRunsStatus, требующего единый статус ВСЕХ строк.
// Нужен прогонам с per-host-where: таргетом ОДНОГО хоста (split-brain guard,
// failed_when fail-stop): целевой хост = failed, прочие roster-хосты, у которых
// after on:/where: осталось 0 задач = no_match (не failed).
func (s *Stack) AssertApplyHostStatus(t *testing.T, applyID, sid, expected string) {
	t.Helper()
	CheckApplyRunsStatusValid(t, expected)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var status string
	err := s.db.QueryRow(ctx,
		"SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2", applyID, sid).Scan(&status)
	if err != nil {
		t.Fatalf("AssertApplyHostStatus(apply=%s sid=%s): нет строки apply_runs: %v", applyID, sid, err)
	}
	if status != expected {
		t.Fatalf("AssertApplyHostStatus(apply=%s sid=%s): status=%q, ожидался %q", applyID, sid, status, expected)
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
func (s *Stack) AssertAuditEvent(t *testing.T, eventType string, expectedPayload map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Пустой/nil payload (fixture `audit_events: [{type: ...}]` без payload) —
	// presence-проверка только по event_type. `payload @> 'null'::jsonb` не
	// матчит объект-payload и дал бы ложный fail; для «событие такого типа есть»
	// фильтр по payload не нужен.
	var (
		count int
		err   error
	)
	if len(expectedPayload) == 0 {
		err = s.db.QueryRow(ctx,
			"SELECT COUNT(*) FROM audit_log WHERE event_type = $1", eventType).Scan(&count)
		if err != nil {
			t.Fatalf("AssertAuditEvent %s: query: %v", eventType, err)
		}
		if count == 0 {
			t.Fatalf("AssertAuditEvent %s: ни одного события такого типа", eventType)
		}
		return
	}

	subsetJSON, err := json.Marshal(expectedPayload)
	if err != nil {
		t.Fatalf("AssertAuditEvent: marshal expected payload: %v", err)
	}

	err = s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = $1 AND payload @> $2::jsonb
	`, eventType, string(subsetJSON)).Scan(&count)
	if err != nil {
		t.Fatalf("AssertAuditEvent %s: query: %v", eventType, err)
	}
	if count == 0 {
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
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

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

func parseExpositionLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	idx := strings.LastIndex(line, " ")
	if idx < 0 {
		return "", nil, 0, false
	}
	left := strings.TrimSpace(line[:idx])
	right := strings.TrimSpace(line[idx+1:])

	if v, err := strconv.ParseFloat(right, 64); err == nil {
		_ = v
	} else {
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

func labelsContain(have, want map[string]string) bool {
	for k, v := range want {
		got, ok := have[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

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
	if em, ok := expected.(map[string]any); ok {
		am, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		return subsetMatches(am, em)
	}
	if af, ok := toFloat(actual); ok {
		if ef, ok := toFloat(expected); ok {
			return af == ef
		}
	}
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

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = sortedKeys // зарезервировано под диагностику будущих assert-ов

// Container-side asserts (L3b-4 implementation).
//
// Все четыре assert-а выполняют команды внутри privileged Debian-12 systemd-soul
// контейнера через SoulContainer.Exec. SoulContainer.Exec возвращает combined
// stdout+stderr (testcontainers-go multiplexed reader), поэтому harness не
// разделяет потоки: для assert-а достаточно exit-code, тело используется только
// в diag-сообщениях.
//
// hostExecTimeout — верхний потолок на один Exec. Команды дёшевые (dpkg-query,
// systemctl is-active, stat, cat | grep) → 30s с запасом, на медленных CI
// systemd может тянуть до 1-2s даже на is-active.
const hostExecTimeout = 30 * time.Second

// soulContainerByIdx возвращает SoulContainer по индексу или фейлит t с
// диагностическим out-of-range. Внутренний хелпер для AssertHost*.
func (s *Stack) soulContainerByIdx(t *testing.T, soulIdx int) *SoulContainer {
	t.Helper()
	if soulIdx < 0 || soulIdx >= len(s.SoulContainers) {
		t.Fatalf("soulIdx %d out of range (have %d soul-контейнеров)",
			soulIdx, len(s.SoulContainers))
	}
	sc := s.SoulContainers[soulIdx]
	if sc == nil {
		t.Fatalf("SoulContainers[%d] = nil", soulIdx)
	}
	return sc
}

// AssertHostPkgInstalled проверяет, что Debian-package реально установлен в
// soul-контейнере. Через `dpkg-query -W -f=${Status} <pkg>`: status строки
// `install ok installed` — единственное валидное значение для «полностью
// установлен» (есть промежуточные: `deinstall ok config-files`, `purge ok
// not-installed` и т.п.). Контейнер всегда debian-12 (см. container.go), rpm-
// ветка не нужна.
func (s *Stack) AssertHostPkgInstalled(t *testing.T, soulIdx int, pkg string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{"dpkg-query", "-W", "-f=${Status}", pkg})
	if err != nil {
		t.Fatalf("AssertHostPkgInstalled(soulIdx=%d pkg=%s): exec dpkg-query: %v\noutput=%s",
			soulIdx, pkg, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertHostPkgInstalled(soulIdx=%d pkg=%s): dpkg-query exit=%d\noutput=%s",
			soulIdx, pkg, code, out)
	}
	status := strings.TrimSpace(out)
	if !strings.Contains(status, "install ok installed") {
		t.Fatalf("AssertHostPkgInstalled(soulIdx=%d pkg=%s): пакет не установлен корректно, status=%q",
			soulIdx, pkg, status)
	}
}

// AssertHostServiceActive проверяет, что systemd-unit active через
// `systemctl is-active <svc>`. is-active возвращает exit=0 если active,
// иначе ненулевой (3 для inactive/failed/unknown). На assert-фазе нам важно
// именно текстовое значение stdout (`active`), exit-code дублирует.
func (s *Stack) AssertHostServiceActive(t *testing.T, soulIdx int, svc string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{"systemctl", "is-active", svc})
	if err != nil {
		t.Fatalf("AssertHostServiceActive(soulIdx=%d svc=%s): exec systemctl: %v\noutput=%s",
			soulIdx, svc, err, out)
	}
	status := strings.TrimSpace(out)
	if status != "active" {
		t.Fatalf("AssertHostServiceActive(soulIdx=%d svc=%s): status=%q (exit=%d), ожидалось 'active'",
			soulIdx, svc, status, code)
	}
}

// AssertHostFileExists проверяет, что файл/каталог по path существует внутри
// soul-контейнера. Используем `stat -c %F <path>` — exit=0 и непустой stdout
// (тип объекта: `regular file`/`directory`/…). Чисто наличие, без проверки
// типа: для проверки type/perm caller вызовет дополнительные assert-ы (пока
// L3b-4 их не вводит).
func (s *Stack) AssertHostFileExists(t *testing.T, soulIdx int, path string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{"stat", "-c", "%F", path})
	if err != nil {
		t.Fatalf("AssertHostFileExists(soulIdx=%d path=%s): exec stat: %v\noutput=%s",
			soulIdx, path, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertHostFileExists(soulIdx=%d path=%s): stat exit=%d\noutput=%s",
			soulIdx, path, code, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("AssertHostFileExists(soulIdx=%d path=%s): stat вернул пустой результат",
			soulIdx, path)
	}
}

// AssertHostFileContent проверяет, что файл по path содержит подстроку substr.
// Команда: `cat <path> | grep -F -- <substr>`; grep exit=0 — substring найден,
// 1 — нет, >=2 — ошибка. Аргументы шеллу передаём через single-quote-escape
// (shellQuote), произвольный user-input в test-fixtures не предполагается.
func (s *Stack) AssertHostFileContent(t *testing.T, soulIdx int, path, substr string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	script := fmt.Sprintf("cat %s | grep -F -- %s", shellQuote(path), shellQuote(substr))
	out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("AssertHostFileContent(soulIdx=%d path=%s substr=%q): exec: %v\noutput=%s",
			soulIdx, path, substr, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertHostFileContent(soulIdx=%d path=%s substr=%q): подстрока не найдена (grep exit=%d)\noutput=%s",
			soulIdx, path, substr, code, out)
	}
}

// AssertHostHTTPContains делает HTTP GET по url ВНУТРИ soul-контейнера (curl,
// присутствует в L3b-Dockerfile) и проверяет, что тело ответа содержит substr.
// Поллит до retrySec секунд: сетевой сервис (node_exporter :9100/metrics)
// поднимается асинхронно после systemctl start — exporter-у нужно секунду на
// bind listen-сокета.
//
// Это и есть piggyback-проверка node-exporter: url=http://127.0.0.1:9100/metrics,
// substr="node_" (любая node_exporter-метрика) подтверждает, что бинарь
// разложен, systemd-unit активен И порт реально слушает + отдаёт /metrics —
// чего services/files-проверки по отдельности не доказывают.
//
// curl -fsS: -f → ненулевой exit на HTTP >= 400, -s → без прогресс-бара, -S →
// показать ошибку. exit 0 + substr в теле = успех.
func (s *Stack) AssertHostHTTPContains(t *testing.T, soulIdx int, url, substr string, retrySec int) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	var lastOut string
	var lastCode int
	deadline := time.Now().Add(time.Duration(retrySec) * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
		script := fmt.Sprintf("curl -fsS %s | grep -F -- %s", shellQuote(url), shellQuote(substr))
		out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
		cancel()
		if err != nil {
			t.Fatalf("AssertHostHTTPContains(soulIdx=%d url=%s): exec: %v\noutput=%s",
				soulIdx, url, err, out)
		}
		lastOut, lastCode = out, code
		if code == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("AssertHostHTTPContains(soulIdx=%d url=%s substr=%q): не получено за %ds (curl|grep exit=%d)\noutput=%s",
		soulIdx, url, substr, retrySec, lastCode, lastOut)
}

// AssertRedisACLUser проверяет, что юзер виден в ЖИВОМ redis через redis-cli ACL
// LIST (AUTH admin) — живой эффект community.redis.acl, а не только state.
func (s *Stack) AssertRedisACLUser(t *testing.T, soulIdx int, host string, port int, adminUser, adminPass, wantUser string) {
	t.Helper()
	sc := s.soulContainerByIdx(t, soulIdx)

	ctx, cancel := context.WithTimeout(context.Background(), hostExecTimeout)
	defer cancel()

	// --no-auth-warning глушит stderr про пароль в argv (Exec склеивает stdout+stderr).
	// `^user <name> ` — чёткая граница (не цепляет однопрефиксного соседа).
	script := fmt.Sprintf("redis-cli -h %s -p %d --user %s --pass %s --no-auth-warning ACL LIST | grep -E %s",
		shellQuote(host), port, shellQuote(adminUser), shellQuote(adminPass), shellQuote("^user "+wantUser+" "))
	out, code, err := sc.Exec(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("AssertRedisACLUser(soulIdx=%d user=%s): exec: %v\noutput=%s", soulIdx, wantUser, err, out)
	}
	if code != 0 {
		t.Fatalf("AssertRedisACLUser(soulIdx=%d user=%s): нет в живом ACL LIST (redis-cli|grep exit=%d) — community.redis.acl не применил юзера?\noutput=%s",
			soulIdx, wantUser, code, out)
	}
}

// shellQuote оборачивает строку в одинарные кавычки, экранируя внутренние
// одинарные кавычки по шаблону POSIX `'\”`. Используется только для путей и
// substring-ов из тест-fixtures (контролируемый input, не user-data).
func shellQuote(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// ── Per-task flow-control asserts (FC-0) ────────────────────────────────────
//
// ★ РАЗВЕДКА FC-0. Per-task TaskStatus (SKIPPED/OK/CHANGED/FAILED/TIMED_OUT/
// CANCELLED) и error.code (flowcontrol.failed_when / flowcontrol.until_exhausted
// / …) keeper НЕ персистит в отдельную task-таблицу и НЕ кладёт колонкой в
// apply_runs. Единственная per-task persistence-поверхность — audit_log:
//
//   keeper/internal/grpc/events_taskevent.go → handleTaskEvent → AuditWriter.Write(
//       EventType = "task.executed", Source = "soul_grpc",
//       CorrelationID = apply_id,
//       Payload = shared/audit.BuildTaskExecutedPayload{...})
//
// Каждая задача (включая SKIPPED — soul эмитит skippedTaskEvent,
// applyrunner.go) даёт одну строку audit_log. Форма payload зафиксирована
// shared/audit.BuildTaskExecutedPayload:
//
//   {sid, apply_id, task_idx, plan_index, status, passage,
//    error?:{code, module, message?}, register_data?, suppressed?}
//
// где status = keeperv1.TaskStatus.String() (литерал "TASK_STATUS_SKIPPED" и
// т.п.), error.code = TaskError.code (для no_log error опускает message, но code
// и module кладёт). Поэтому FC-ассерты per-task читают audit_log, НЕ apply_runs.
//
// КЛЮЧ КОРРЕЛЯЦИИ — plan_index (ГЛОБАЛЬНЫЙ сквозной индекс задачи по всему
// плану, миграции 079/081, ADR-056 §S1). Локальный task_idx неуникален между
// Passage и между хостами одного Passage (per-host where:), поэтому per-task
// ассерты ключуются по plan_index. N=1-прогон → plan_index == task_idx.

// taskStatusLiteralByEnum — закрытое множество строковых литералов
// keeperv1.TaskStatus.String(), которые audit-payload несёт в payload->>'status'.
// Источник правды — proto/keeper/v1/apply.proto (enum TaskStatus). Дублируется
// здесь литералом: tests/e2e-live — отдельный go-модуль без зависимости на
// proto/gen. Fail-early на опечатку в expectation (ADR-039(4), как у
// validApplyRunsStatus).
var taskStatusLiteralByEnum = map[string]struct{}{
	"TASK_STATUS_UNSPECIFIED": {},
	"TASK_STATUS_OK":          {},
	"TASK_STATUS_CHANGED":     {},
	"TASK_STATUS_FAILED":      {},
	"TASK_STATUS_TIMED_OUT":   {},
	"TASK_STATUS_SKIPPED":     {},
	"TASK_STATUS_CANCELLED":   {},
}

// IsValidTaskStatus — pure-проверка строкового литерала per-task статуса.
func IsValidTaskStatus(status string) bool {
	_, ok := taskStatusLiteralByEnum[status]
	return ok
}

// AssertTaskStatus проверяет, что per-task статус задачи (apply_id, sid,
// plan_index, passage) равен wantStatus. Читает audit_log (event_type=
// task.executed, correlation_id=apply_id), а не apply_runs — keeper персистит
// per-task TaskStatus ТОЛЬКО в audit (см. doc-comment блока).
//
// planIdx — ГЛОБАЛЬНЫЙ сквозной plan_index задачи (не локальный task_idx); для
// N=1-прогона совпадает с позицией задачи в плане. passage — индекс Passage
// staged-render (0 = единственный).
//
// wantStatus — строковый литерал keeperv1.TaskStatus.String(), например
// "TASK_STATUS_SKIPPED" / "TASK_STATUS_FAILED" / "TASK_STATUS_CHANGED". Невалидный
// литерал в expectation → fail-early (опечатка видна сразу).
//
// Берём ПОСЛЕДНЮЮ по created_at строку: retry той же задачи эмитит TaskEvent
// последней попытки (один TaskEvent на task — см. runTaskWithRetry), но дубль
// под cross-Keeper-роутинг возможен; «последняя побеждает» совпадает с
// register-семантикой.
func (s *Stack) AssertTaskStatus(t *testing.T, applyID, sid string, planIdx, passage int, wantStatus string) {
	t.Helper()
	if !IsValidTaskStatus(wantStatus) {
		t.Fatalf("AssertTaskStatus: неизвестный TaskStatus-литерал %q (разрешены: TASK_STATUS_OK/CHANGED/FAILED/TIMED_OUT/SKIPPED/CANCELLED/UNSPECIFIED)", wantStatus)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var status string
	err := s.db.QueryRow(ctx, `
		SELECT payload->>'status'
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1
		  AND payload->>'sid' = $2
		  AND (payload->>'plan_index')::int = $3
		  AND (payload->>'passage')::int = $4
		ORDER BY created_at DESC
		LIMIT 1
	`, applyID, sid, planIdx, passage).Scan(&status)
	if err != nil {
		s.dumpTaskEvents(ctx, t, applyID, sid)
		t.Fatalf("AssertTaskStatus(apply=%s sid=%s plan_index=%d passage=%d): нет task.executed-строки в audit_log (задача не исполнялась / TaskEvent не дошёл?): %v",
			applyID, sid, planIdx, passage, err)
	}
	if status != wantStatus {
		s.dumpTaskEvents(ctx, t, applyID, sid)
		t.Fatalf("AssertTaskStatus(apply=%s sid=%s plan_index=%d passage=%d): status=%q, ожидался %q",
			applyID, sid, planIdx, passage, status, wantStatus)
	}
}

// AssertTaskErrorCode проверяет error.code per-task задачи (apply_id, sid,
// plan_index, passage). Доказывает класс падения: flowcontrol.failed_when (бизнес-
// провал по CEL-предикату) vs flowcontrol.until_exhausted vs модульная ошибка
// (например "pkg.not_found"). error.code персистится в audit_log payload->'error'
// ->>'code' (см. shared/audit.BuildTaskExecutedPayload); apply_runs хранит лишь
// composed error_summary-ТЕКСТ, не структурный code.
//
// Для no_log-задачи error.message подавлен, но code и module кладутся — этот
// assert работает и на no_log-задачах.
//
// wantCode — точный литерал TaskError.code, например "flowcontrol.failed_when".
// Если у задачи нет error (OK/CHANGED/SKIPPED) → error.code отсутствует → fail.
func (s *Stack) AssertTaskErrorCode(t *testing.T, applyID, sid string, planIdx, passage int, wantCode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var code string
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(payload->'error'->>'code', '<no-error>')
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1
		  AND payload->>'sid' = $2
		  AND (payload->>'plan_index')::int = $3
		  AND (payload->>'passage')::int = $4
		ORDER BY created_at DESC
		LIMIT 1
	`, applyID, sid, planIdx, passage).Scan(&code)
	if err != nil {
		s.dumpTaskEvents(ctx, t, applyID, sid)
		t.Fatalf("AssertTaskErrorCode(apply=%s sid=%s plan_index=%d passage=%d): нет task.executed-строки в audit_log: %v",
			applyID, sid, planIdx, passage, err)
	}
	if code != wantCode {
		s.dumpTaskEvents(ctx, t, applyID, sid)
		t.Fatalf("AssertTaskErrorCode(apply=%s sid=%s plan_index=%d passage=%d): error.code=%q, ожидался %q",
			applyID, sid, planIdx, passage, code, wantCode)
	}
}

// AssertTaskRegisterField читает одно поле register_data задачи из
// apply_task_register (PK apply_id, sid, plan_index — миграция 079). Возвращает
// JSON-скаляр поля как строку (register_data->>'<field>'); для stdout / exit_code
// / ignored_error / changed / failed нужен FC-1/FC-4 (доказать, что register
// flow-control-задачи несёт ожидаемое значение).
//
// register_data — то, что РЕАЛЬНЫЙ soul эмитил в TaskEvent.register_data и keeper
// собрал (accumulateRegister). НЕ персистируется для задачи без register: (nil
// register_data → строки нет, UpsertTaskRegister no-op) → assert зафейлит «нет
// строки».
//
// planIdx — ГЛОБАЛЬНЫЙ сквозной plan_index (ключ корреляции, не локальный
// task_idx). Без разреза по passage: PK уже уникален по plan_index сквозь все
// Passage (та же находка, что закрыла миграция 079).
func (s *Stack) AssertTaskRegisterField(t *testing.T, applyID, sid string, planIdx int, field, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var got string
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(register_data->>$4, '<null>')
		FROM apply_task_register
		WHERE apply_id = $1 AND sid = $2 AND plan_index = $3
	`, applyID, sid, planIdx, field).Scan(&got)
	if err != nil {
		t.Fatalf("AssertTaskRegisterField(apply=%s sid=%s plan_index=%d field=%s): нет register-строки (задача без register:/реальный soul не вернул register?): %v",
			applyID, sid, planIdx, field, err)
	}
	if got != want {
		t.Fatalf("AssertTaskRegisterField(apply=%s sid=%s plan_index=%d field=%s): %q, ожидалось %q",
			applyID, sid, planIdx, field, got, want)
	}
}

// dumpTaskEvents печатает диагностику: все task.executed-строки прогона по хосту
// (plan_index/task_idx/passage/status/error.code). Зовётся на фейл per-task
// assert-ов — без неё «нет строки» немой, видно ли вообще TaskEvent-ы дошли.
func (s *Stack) dumpTaskEvents(ctx context.Context, t *testing.T, applyID, sid string) {
	t.Helper()
	rows, err := s.db.Query(ctx, `
		SELECT
			COALESCE(payload->>'plan_index','?'),
			COALESCE(payload->>'task_idx','?'),
			COALESCE(payload->>'passage','?'),
			COALESCE(payload->>'status','?'),
			COALESCE(payload->'error'->>'code','-')
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1 AND payload->>'sid' = $2
		ORDER BY created_at ASC
	`, applyID, sid)
	if err != nil {
		t.Logf("dumpTaskEvents(apply=%s sid=%s): query: %v", applyID, sid, err)
		return
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var planIdx, taskIdx, passage, status, code string
		if err := rows.Scan(&planIdx, &taskIdx, &passage, &status, &code); err != nil {
			t.Logf("dumpTaskEvents: scan: %v", err)
			return
		}
		lines = append(lines, fmt.Sprintf("plan_index=%s task_idx=%s passage=%s status=%s error_code=%s",
			planIdx, taskIdx, passage, status, code))
	}
	if len(lines) == 0 {
		t.Logf("dumpTaskEvents(apply=%s sid=%s): НИ ОДНОЙ task.executed-строки", applyID, sid)
		return
	}
	t.Logf("dumpTaskEvents(apply=%s sid=%s) task.executed:\n  %s", applyID, sid, strings.Join(lines, "\n  "))
}
