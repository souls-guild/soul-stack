//go:build e2e

package harness

// L3b harness extension для Vigil/Oracle/Decree (ADR-030). Три helper-а
// (CreateVigil / CreateDecree / WaitForOracleFires) + stub-emit EmitPortent —
// минимальный набор, через который тест прокатывает full path реестр →
// match-state, БЕЗ реального mTLS-EventStream-emit-а от soul-stub-а (тот —
// отдельный slice harness-расширения, см. tests/e2e/oracle_typed_portent_test.go).
//
// Контракт по слоям:
//   - CreateVigil / CreateDecree — REST POST /v1/vigils, /v1/decrees через
//     JWT первого Архонта (cluster-admin, permission `vigil.create` /
//     `decree.create` входят в `*`-набор по ADR-013). Real handler-stack
//     (validate.go + InsertVigil/InsertDecree), без обхода схемы.
//   - WaitForOracleFires — поллинг таблицы `oracle_fires` (cooldown-state per
//     (decree, subject), миграция 041). UPSERT на ON CONFLICT → одна строка на
//     уникальную пару (decree, subject); count(rows) == «было ли хоть одно
//     срабатывание для каждой пары», НЕ кумулятивный счётчик fire-ов одного
//     subject-а (повторные fire-ы того же subject-а обновляют fired_at той же
//     строки). Для подсчёта суммарных срабатываний используйте
//     audit_log.event_type='oracle.fired' (out of scope этого helper-а).
//   - EmitPortent — stub: прямой INSERT в `oracle_fires` (PG-only path).
//     НЕ проходит через handlePortentEvent → НЕ ставит scenario в work-queue,
//     НЕ пишет audit `oracle.fired`. Назначение — прокатить через 3 helper-а
//     end-to-end в smoke-тесте (TestL3b_VigilDecreeOracleFlow_Smoke). Реальный
//     путь (soul-stub.SendPortent → mTLS EventStream → handlePortentEvent →
//     SubjectMatches → where-CEL → EnqueueScenario → RecordFire → audit) покрыт
//     execution-e2e TestOracle_FileChanged_FiresScenario (vigil_oracle_test.go),
//     там же — WaitForOracleReaction (assert поставленного реактором apply_run-а).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- CreateVigil ------------------------------------------------------

// CreateVigilOpts — параметры [Stack.CreateVigil]. Субъект — XOR Coven/SID
// (CHECK vigils_subject_xor, миграция 041); валидируется на service-слое
// keeper-а, harness ничего не проверяет до round-trip-а (тестируем публичный
// контракт OpenAPI как чёрный ящик).
//
//   - Name — vigils.name (kebab-case 1..63).
//   - Interval — duration-конвенция Soul Stack ("30s"/"5m"; config.ParseDuration).
//   - Check — адрес core-beacon (`core.beacon.<name>`; shared/beaconaddr.All).
//   - Coven / SID — XOR-субъект.
//   - Params — opaque JSON-параметры проверки; форма зависит от Check.
//   - Enabled — по умолчанию true (как и REST с пустым enabled-полем).
type CreateVigilOpts struct {
	Name     string
	Interval string
	Check    string
	Coven    []string
	SID      *string
	Params   map[string]any
	Enabled  *bool
}

// CreateVigil создаёт Vigil через Operator-API (POST /v1/vigils) и возвращает
// vigils.name. Любой не-201 → t.Fatal с телом ответа (диагностика 4xx без
// догадок, как [Stack.CreateIncarnation]).
//
// IncarnationName опциональный аргумент сюда НЕ передаётся (Vigil не привязан
// к incarnation в ADR-030; incarnation_name — поле Decree).
func (s *Stack) CreateVigil(ctx context.Context, t *testing.T, opts CreateVigilOpts) string {
	t.Helper()

	body := map[string]any{
		"name":     opts.Name,
		"interval": opts.Interval,
		"check":    opts.Check,
	}
	if len(opts.Coven) > 0 {
		body["coven"] = opts.Coven
	}
	if opts.SID != nil {
		body["sid"] = *opts.SID
	}
	if opts.Params != nil {
		raw, err := json.Marshal(opts.Params)
		if err != nil {
			t.Fatalf("CreateVigil(%s): marshal params: %v", opts.Name, err)
		}
		body["params"] = json.RawMessage(raw)
	}
	if opts.Enabled != nil {
		body["enabled"] = *opts.Enabled
	}

	resp, status, err := s.opClient(t).post(ctx, "/v1/vigils", body)
	if err != nil {
		t.Fatalf("CreateVigil(%s): http: %v", opts.Name, err)
	}
	if status != http.StatusCreated {
		t.Fatalf("CreateVigil(%s): status %d, body=%s", opts.Name, status, string(resp))
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateVigil(%s): decode: %v (body=%s)", opts.Name, err, string(resp))
	}
	if out.Name == "" {
		t.Fatalf("CreateVigil(%s): empty name in 201 body=%s", opts.Name, string(resp))
	}
	return out.Name
}

// --- CreateDecree -----------------------------------------------------

// CreateDecreeOpts — параметры [Stack.CreateDecree].
//
//   - Name — decrees.name (kebab-case 1..63).
//   - OnBeacon — имя Vigil-а (decrees.on_beacon), на чей Portent правило
//     реагирует. БЕЗ FK на vigils в схеме (Decree managed-реестр, переживает
//     пересоздание Vigil-а), но грамматически имя должно совпадать с реальным
//     Vigil, иначе никогда не сматчит.
//   - WhereCEL — опц. предикат над event-payload-ом (typed-payload V5-1 или
//     legacy event.data); пустой → всегда match (субъект уже отфильтровал).
//     Компилируется на create через WhereCompiler (битый CEL → 422).
//   - Coven / SID — XOR-субъект Decree-а (independent от субъекта Vigil-а).
//   - IncarnationName — таргет-incarnation реакции (decrees.incarnation_name,
//     обязательно). На enqueue-е сверяется membership: incarnation_name ∈
//     covens отправителя (ADR-030(b) защита от cross-incarnation-эскалации).
//   - ActionScenario — имя named scenario (whitelist; raw-команда отвергнута,
//     ADR-030(b)). Snake_case паттерн (`^[a-z][a-z0-9_]*$`).
//   - ActionInput — opaque JSON-вход сценария (vault-ref КАК ЕСТЬ, инвариант A
//     ADR-027).
//   - Cooldown — duration-конвенция, минимальный интервал между срабатываниями
//     per-(decree, subject). Пустая строка → DEFAULT '0s' (cooldown OFF).
//   - Enabled — по умолчанию true.
type CreateDecreeOpts struct {
	Name            string
	OnBeacon        string
	WhereCEL        string
	Coven           []string
	SID             *string
	IncarnationName string
	ActionScenario  string
	ActionInput     map[string]any
	Cooldown        string
	Enabled         *bool
}

// CreateDecree создаёт Decree через Operator-API (POST /v1/decrees) и
// возвращает decrees.name. Любой не-201 → t.Fatal.
func (s *Stack) CreateDecree(ctx context.Context, t *testing.T, opts CreateDecreeOpts) string {
	t.Helper()

	body := map[string]any{
		"name":             opts.Name,
		"on_beacon":        opts.OnBeacon,
		"incarnation_name": opts.IncarnationName,
		"action_scenario":  opts.ActionScenario,
	}
	if opts.WhereCEL != "" {
		body["where"] = opts.WhereCEL
	}
	if len(opts.Coven) > 0 {
		body["coven"] = opts.Coven
	}
	if opts.SID != nil {
		body["sid"] = *opts.SID
	}
	if opts.ActionInput != nil {
		raw, err := json.Marshal(opts.ActionInput)
		if err != nil {
			t.Fatalf("CreateDecree(%s): marshal action_input: %v", opts.Name, err)
		}
		body["action_input"] = json.RawMessage(raw)
	}
	if opts.Cooldown != "" {
		body["cooldown"] = opts.Cooldown
	}
	if opts.Enabled != nil {
		body["enabled"] = *opts.Enabled
	}

	resp, status, err := s.opClient(t).post(ctx, "/v1/decrees", body)
	if err != nil {
		t.Fatalf("CreateDecree(%s): http: %v", opts.Name, err)
	}
	if status != http.StatusCreated {
		t.Fatalf("CreateDecree(%s): status %d, body=%s", opts.Name, status, string(resp))
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateDecree(%s): decode: %v (body=%s)", opts.Name, err, string(resp))
	}
	if out.Name == "" {
		t.Fatalf("CreateDecree(%s): empty name in 201 body=%s", opts.Name, string(resp))
	}
	return out.Name
}

// --- WaitForOracleFires -----------------------------------------------

// OracleFire — одна строка таблицы `oracle_fires` (миграция 041, cooldown-state
// per-(decree, subject)). Авторитетная схема — PRIMARY KEY (decree, subject):
// одна строка на уникальную пару, UPSERT на ON CONFLICT обновляет fired_at,
// а НЕ добавляет вторую строку. Соответственно, len([]OracleFire) — число
// уникальных subject-ов, по которым Decree уже стрелял, а не суммарный
// fire-counter (см. ограничения в шапке файла).
type OracleFire struct {
	Decree   string
	Subject  string
	FiredAt  time.Time
}

// WaitForOracleFires блокируется до того, как для Decree-а decreeName в таблице
// `oracle_fires` появится не меньше expectedCount строк, либо до истечения
// timeout. Возвращает фактический список строк (отсортирован по subject ASC
// для детерминированных assert-ов вызывающим).
//
// Семантика expectedCount — число УНИКАЛЬНЫХ subject-ов в (decree=decreeName,
// subject=*), а НЕ кумулятивный fire-counter (см. OracleFire shape).
//
// Поллинг 250 мс, как в [Stack.WaitApplySuccess]; жесткий ceil — timeout.
// Истечение timeout → t.Fatal с дампом текущего набора строк (без надежды,
// что «само пройдёт»).
func (s *Stack) WaitForOracleFires(ctx context.Context, t *testing.T, decreeName string, expectedCount int, timeout time.Duration) []OracleFire {
	t.Helper()
	if expectedCount < 1 {
		t.Fatalf("WaitForOracleFires(%s): expectedCount must be >= 1, got %d", decreeName, expectedCount)
	}

	deadline := time.Now().Add(timeout)
	var last []OracleFire
	for time.Now().Before(deadline) {
		fires, err := s.listOracleFires(ctx, decreeName)
		if err != nil {
			t.Fatalf("WaitForOracleFires(%s): query oracle_fires: %v", decreeName, err)
		}
		if len(fires) >= expectedCount {
			return fires
		}
		last = fires
		select {
		case <-ctx.Done():
			t.Fatalf("WaitForOracleFires(%s): ctx done before %d fires (got %d): %v",
				decreeName, expectedCount, len(fires), ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("WaitForOracleFires(%s): не достиг %d fire-ов за %s (текущий набор: %+v)",
		decreeName, expectedCount, timeout, last)
	return nil // unreachable: t.Fatalf не возвращает.
}

// WaitForOracleReaction блокируется до появления (или истечения timeout) хотя бы
// одной строки apply_runs, поставленной Oracle-реактором: scenario=scenarioName,
// incarnation_name=incarnationName, started_by_aid IS NULL (Soul-инициированная
// реакция без identity Архонта, см. oracle_enqueuer.go) и started_at >= since
// (отсекает авто-create-прогон incarnation-а). Возвращает apply_id первого такого
// прогона (для дальнейших assert-ов вызывающим). timeout → t.Fatal.
//
// scenarioName ОБЯЗАН отличаться от scenario авто-create incarnation-а, иначе
// фильтр scenario+started_by_aid+since недостаточен для различения. Поллинг 250 мс
// (как WaitForOracleFires).
func (s *Stack) WaitForOracleReaction(ctx context.Context, t *testing.T, incarnationName, scenarioName string, since time.Time, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		applyID, found, err := s.findOracleReaction(ctx, incarnationName, scenarioName, since)
		if err != nil {
			t.Fatalf("WaitForOracleReaction(%s/%s): query apply_runs: %v", incarnationName, scenarioName, err)
		}
		if found {
			return applyID
		}
		select {
		case <-ctx.Done():
			t.Fatalf("WaitForOracleReaction(%s/%s): ctx done до появления apply_run-а: %v",
				incarnationName, scenarioName, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("WaitForOracleReaction(%s/%s): apply_run от реактора не появился за %s",
		incarnationName, scenarioName, timeout)
	return "" // unreachable: t.Fatalf не возвращает.
}

func (s *Stack) findOracleReaction(ctx context.Context, incarnationName, scenarioName string, since time.Time) (string, bool, error) {
	const sql = `
SELECT apply_id
FROM apply_runs
WHERE incarnation_name = $1
  AND scenario = $2
  AND started_by_aid IS NULL
  AND started_at >= $3
ORDER BY started_at ASC
LIMIT 1`
	var applyID string
	err := s.db.QueryRow(ctx, sql, incarnationName, scenarioName, since).Scan(&applyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return applyID, true, nil
}

func (s *Stack) listOracleFires(ctx context.Context, decreeName string) ([]OracleFire, error) {
	const sql = `
SELECT decree, subject, fired_at
FROM oracle_fires
WHERE decree = $1
ORDER BY subject ASC`
	rows, err := s.db.Query(ctx, sql, decreeName)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []OracleFire
	for rows.Next() {
		var f OracleFire
		if err := rows.Scan(&f.Decree, &f.Subject, &f.FiredAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter: %w", err)
	}
	return out, nil
}

// --- EmitPortent (stub) -----------------------------------------------

// EmitPortent — STUB-эмуляция срабатывания Vigil-Decree-пары: прямой UPSERT в
// `oracle_fires` (decree, subject, fired_at=NOW). НЕ проходит через
// handlePortentEvent — НЕ ставит scenario в work-queue, НЕ пишет
// audit `oracle.fired`, НЕ инкрементирует circuit-breaker.
//
// Назначение: smoke-тестирование 3 helper-ов (CreateVigil / CreateDecree /
// WaitForOracleFires) end-to-end без зависимости от mTLS-EventStream-emit-а от
// soul-stub-а (отдельный slice harness-расширения; пока соул-стрим эмулировать
// не умеем — `tests/e2e/oracle_typed_portent_test.go` Skip-ит full-loop).
//
// Subject — авторитетный SID хоста-отправителя (в реальном пути — из mTLS peer
// cert, harness даёт его явно). DecreeName должен существовать в `decrees`
// (FK oracle_fires.decree → decrees(name)) — иначе INSERT даст FK-violation.
//
// При появлении полноценного soul-stub-emit (`SoulStub.SendPortent`) этот
// helper удаляется в пользу настоящего пути.
func (s *Stack) EmitPortent(ctx context.Context, t *testing.T, decreeName, subjectSID string) {
	t.Helper()

	const sql = `
INSERT INTO oracle_fires (decree, subject, fired_at)
VALUES ($1, $2, NOW())
ON CONFLICT (decree, subject) DO UPDATE SET fired_at = EXCLUDED.fired_at`
	if _, err := s.db.Exec(ctx, sql, decreeName, subjectSID); err != nil {
		t.Fatalf("EmitPortent(decree=%s subject=%s): insert oracle_fires: %v",
			decreeName, subjectSID, err)
	}
}
