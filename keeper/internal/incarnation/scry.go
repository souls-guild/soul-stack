package incarnation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ScryCandidate — минимальное представление incarnation для iterator-а
// фонового правила `scry_background` (ADR-031 Slice C, Reaper). Содержит ровно
// то, что нужно reaper-tick-у, чтобы построить scenario.CheckDriftSpec
// (Service-резолв + roster + dispatch) и проверить min-interval-throttle на
// чтении.
type ScryCandidate struct {
	Name             string
	Service          string
	ServiceVersion   string
	Covens           []string
	LastDriftCheckAt *time.Time
}

// SelectScryCandidates возвращает батч incarnation-ов, подходящих для
// фонового Scry-скана (ADR-031 Slice C, Reaper-правило `scry_background`).
// Iterator-предикат:
//
//   - статус ready или drift (drift — информационный, не блокирует
//     повторный скан; см. ADR-031);
//   - нет активного apply-прогона (NOT IN apply_runs WHERE finished_at IS
//     NULL — исключает все live-claimed/dispatched/running прогоны
//     независимо от status, не только applying);
//   - сортировка `last_drift_check_at NULLS FIRST` — естественный round-robin:
//     никогда не сканированные incarnation идут первыми, дальше — по дате
//     последнего скана.
//
// Min-interval-throttle (PM-конфиг `min_interval_per_incarnation`) применяется
// на iterator-level: если задан > 0, исключаем incarnation с
// `last_drift_check_at + min_interval > NOW()`. Нулевая (или отрицательная)
// duration → throttle выключен, ORDER BY NULLS FIRST даёт естественную
// «справедливость». batchSize<=0 → возврат пустого списка без обращения к PG
// (защитный no-op).
func SelectScryCandidates(ctx context.Context, db ExecQueryRower, minInterval time.Duration, batchSize int) ([]ScryCandidate, error) {
	if batchSize <= 0 {
		return nil, nil
	}
	const baseSQL = `
SELECT name, service, service_version, covens, last_drift_check_at
FROM incarnation
WHERE status IN ('ready', 'drift')
  AND name NOT IN (
      SELECT incarnation_name FROM apply_runs WHERE finished_at IS NULL
  )
`
	// $1 — min_interval (interval-литерал), $2 — limit. Min-interval-предикат
	// дописывается условно: при minInterval<=0 не передаём interval, чтобы
	// PG не выводил тип неиспользуемого параметра.
	var (
		sql  string
		args []any
	)
	if minInterval > 0 {
		sql = baseSQL + `
  AND (last_drift_check_at IS NULL OR last_drift_check_at + $1::interval <= NOW())
ORDER BY last_drift_check_at NULLS FIRST, name
LIMIT $2
`
		args = []any{fmt.Sprintf("%d seconds", int64(minInterval.Seconds())), batchSize}
	} else {
		sql = baseSQL + `
ORDER BY last_drift_check_at NULLS FIRST, name
LIMIT $1
`
		args = []any{batchSize}
	}
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("incarnation: scry candidates query: %w", err)
	}
	defer rows.Close()
	var out []ScryCandidate
	for rows.Next() {
		var c ScryCandidate
		if err := rows.Scan(&c.Name, &c.Service, &c.ServiceVersion, &c.Covens, &c.LastDriftCheckAt); err != nil {
			return nil, fmt.Errorf("incarnation: scry candidates scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("incarnation: scry candidates iter: %w", err)
	}
	return out, nil
}

// CountActiveDryRuns возвращает число idoщих фоновых dry_run-прогонов
// (`apply_runs` с `recipe->>'dry_run'='true'` и не-finished). Используется
// Reaper-правилом `scry_background` для throttle-cap-а `max_concurrent_in_flight`.
//
// Запрос не ходит по индексу (predicate на jsonb-поле), но партиал-индекса
// заводить не стоит: cardinality dry_run-прогонов на проде ничтожна по
// сравнению с регулярными apply, full scan по active-pool-у дешёвый.
func CountActiveDryRuns(ctx context.Context, db ExecQueryRower) (int, error) {
	const sql = `
SELECT count(*) FROM apply_runs
WHERE recipe->>'dry_run' = 'true'
  AND finished_at IS NULL
`
	var n int
	if err := db.QueryRow(ctx, sql).Scan(&n); err != nil {
		return 0, fmt.Errorf("incarnation: count active dry-runs: %w", err)
	}
	return n, nil
}

// DriftScanSummary — counts-агрегат одной Scry-проверки (ADR-031 Slice C),
// сохраняемый в колонку `incarnation.last_drift_summary`. Симметричен
// scenario.DriftSummary, плюс `TotalHosts` и `ScannedAt` для дискриминации
// устаревших скан-инфо.
type DriftScanSummary struct {
	HostsDrifted     int       `json:"hosts_drifted"`
	HostsClean       int       `json:"hosts_clean"`
	HostsUnsupported int       `json:"hosts_unsupported"`
	HostsFailed      int       `json:"hosts_failed"`
	TotalHosts       int       `json:"total_hosts"`
	ScannedAt        time.Time `json:"scanned_at"`
}

// UpdateDriftScanResult атомарно проставляет `last_drift_check_at` и
// `last_drift_summary` после завершения dry_run-прогона converge — фонового
// (Reaper-правило `scry_background`) или on-demand (REST/MCP CheckDrift,
// Slice B). Status incarnation НЕ трогает: Slice B делает это отдельным
// `MarkDriftStatus`; вызывающий должен координировать порядок.
//
// `summary.ScannedAt` записывается caller-ом (обычно `time.Now().UTC()` после
// сборки DriftReport). UPDATE без WHERE-guard статуса: incarnation за время
// scan-а могла уйти в applying/destroying — это не мешает зафиксировать факт
// проверки (информационные поля, не блокирующие).
func UpdateDriftScanResult(ctx context.Context, db ExecQueryRower, name string, summary DriftScanSummary) error {
	if !ValidName(name) {
		return fmt.Errorf("incarnation: invalid name %q", name)
	}
	if summary.ScannedAt.IsZero() {
		summary.ScannedAt = time.Now().UTC()
	}
	summaryBytes, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("incarnation: marshal drift summary: %w", err)
	}
	const sql = `
UPDATE incarnation
SET last_drift_check_at = $2,
    last_drift_summary  = $3,
    updated_at          = NOW()
WHERE name = $1
`
	if _, err := db.Exec(ctx, sql, name, summary.ScannedAt, summaryBytes); err != nil {
		return fmt.Errorf("incarnation: update drift scan result: %w", err)
	}
	return nil
}
