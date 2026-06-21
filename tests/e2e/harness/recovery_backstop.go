//go:build e2e

package harness

// Helper-ы для live-crash доказательства двух recovery-backstop находок (ADR-027
// amend (m)/(n), slice S3):
//
//   - reconcile_orphan_applying — снятие осиротевшего applying-lock standalone-
//     прогона крашнувшегося keeper-владельца (Reaper-правило, presence-gated);
//   - eventstream.lease_force_released — presence-gated перехват SID-lease у
//     доказанно-мёртвого holder-а при reconnect-е того же SID к живому keeper-у.
//
// Все read-helper-ы — узкие SQL-проекции (status / epoch-колонки / audit-rows),
// поверх общего s.db. Поллинг-обёртки используют Eventually-паттерн (deadline +
// короткий poll-tick), не фиксированный sleep на полный TTL.

import (
	"context"
	"testing"
	"time"
)

// Eventually поллит cond до true либо до таймаута (poll-tick 100ms). Fatal с
// msg при таймауте. Общий Eventually-паттерн для ассертов, которым нужен лишь
// предикат «состояние достигнуто», без специализированного read-helper-а.
func (s *Stack) Eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Eventually: %s (не выполнено за %s)", msg, timeout)
}

// IncarnationApplyingEpoch — снимок epoch-колонок applying-lock инкарнации
// (миграция 082). nil-указатель = колонка NULL в PG (epoch очищен либо никогда
// не писался). Для ассерта «после снятия orphan-lock epoch обнулён в NULL».
type IncarnationApplyingEpoch struct {
	ApplyID *string
	Attempt *int
	ByKID   *string
	// SinceSet — true, если applying_since НЕ NULL (точное время не важно ассерту,
	// важен лишь факт «epoch присутствует / очищен»).
	SinceSet bool
}

// IncarnationApplyingEpochSnapshot читает epoch-колонки applying-lock инкарнации
// из PG. Fatal, если строки нет.
func (s *Stack) IncarnationApplyingEpochSnapshot(t *testing.T, name string) IncarnationApplyingEpoch {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var (
		ep    IncarnationApplyingEpoch
		since *time.Time
	)
	if err := s.db.QueryRow(ctx,
		`SELECT applying_apply_id, applying_attempt, applying_by_kid, applying_since
		   FROM incarnation WHERE name = $1`,
		name).Scan(&ep.ApplyID, &ep.Attempt, &ep.ByKID, &since); err != nil {
		t.Fatalf("IncarnationApplyingEpochSnapshot(%s): %v", name, err)
	}
	ep.SinceSet = since != nil
	return ep
}

// EpochCleared — true, если ВСЕ epoch-колонки applying-lock обнулены в NULL
// (applying_apply_id / applying_attempt / applying_by_kid / applying_since).
// Признак, что reconcile_orphan_applying ИЛИ честный финал снял lock полностью
// (ReleaseApplyingOrphan чистит epoch в той же tx, что и status→ready).
func (ep IncarnationApplyingEpoch) EpochCleared() bool {
	return ep.ApplyID == nil && ep.Attempt == nil && ep.ByKID == nil && !ep.SinceSet
}

// WaitIncarnationStatus поллит incarnation.status до одного из want-статусов либо
// до таймаута. Возвращает достигнутый статус. Fatal при таймауте с последним
// наблюдённым статусом. Eventually-обёртка для recovery-переходов applying→ready.
func (s *Stack) WaitIncarnationStatus(t *testing.T, name string, want []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := s.db.QueryRow(ctx, "SELECT status FROM incarnation WHERE name = $1", name).Scan(&last)
		cancel()
		if err == nil {
			for _, w := range want {
				if last == w {
					return last
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitIncarnationStatus(%s): статус %v не достигнут за %s (последний=%q)", name, want, timeout, last)
	return ""
}

// CountAuditEventsByPayload возвращает число audit_log-записей с заданным
// event_type, у которых payload->>field = value. Для reconcile_orphan_applying
// .executed (field="incarnation") и eventstream.lease_force_released
// (field="sid") — оба ключуются не по voyage_id, поэтому CountAuditEvents
// (voyage_id-only) им не подходит.
func (s *Stack) CountAuditEventsByPayload(t *testing.T, eventType, field, value string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = $1 AND payload->>$2 = $3`,
		eventType, field, value).Scan(&n); err != nil {
		t.Fatalf("CountAuditEventsByPayload(%s, %s=%s): %v", eventType, field, value, err)
	}
	return n
}

// WaitAuditEventByPayload поллит audit_log до появления ≥1 записи (event_type +
// payload->>field=value) либо таймаута. Возвращает true при появлении. Fatal при
// таймауте. Eventually-обёртка вокруг CountAuditEventsByPayload.
func (s *Stack) WaitAuditEventByPayload(t *testing.T, eventType, field, value string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.CountAuditEventsByPayload(t, eventType, field, value) >= 1 {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitAuditEventByPayload(%s, %s=%s): не появилось за %s", eventType, field, value, timeout)
	return false
}

// AuditPayloadField возвращает значение payload->>outField из самой свежей
// audit-записи (event_type + payload->>selField=selValue), либо пустую строку,
// если записи нет. Для ассерта конкретных полей payload (prev_kid в reconcile_
// orphan_applying.executed, new_kid в lease_force_released).
func (s *Stack) AuditPayloadField(t *testing.T, eventType, selField, selValue, outField string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out string
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(payload->>$4, '') FROM audit_log
		WHERE event_type = $1 AND payload->>$2 = $3
		ORDER BY created_at DESC LIMIT 1`,
		eventType, selField, selValue, outField).Scan(&out)
	if err != nil {
		return ""
	}
	return out
}

// LeaseForceReleasedNewKID возвращает new_kid из payload audit-события
// eventstream.lease_force_released для данного SID (KID живого keeper-а, который
// presence-gated перехватил lease у мёртвого holder-а), либо пустую строку, если
// события ещё нет. Это авторитетный признак, на какой keeper переехал SID-lease
// после force-release — payload пишется ТОЙ ЖЕ tx, что ForceAcquireSoulLease
// (eventstream.auditLeaseForceReleased), поэтому событие = состоявшийся перехват.
func (s *Stack) LeaseForceReleasedNewKID(t *testing.T, sid string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var newKID string
	err := s.db.QueryRow(ctx, `
		SELECT payload->>'new_kid' FROM audit_log
		WHERE event_type = $1 AND payload->>'sid' = $2
		ORDER BY created_at DESC LIMIT 1`,
		"eventstream.lease_force_released", sid).Scan(&newKID)
	if err != nil {
		return ""
	}
	return newKID
}

// WaitLeaseForceReleased поллит audit_log до появления события
// eventstream.lease_force_released для SID с new_kid ∈ wantNewKIDs, либо до
// таймаута. Возвращает наблюдённый new_kid. Fatal при таймауте. Eventually-
// обёртка вокруг LeaseForceReleasedNewKID (lease переехал killed→live keeper).
func (s *Stack) WaitLeaseForceReleased(t *testing.T, sid string, wantNewKIDs []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.LeaseForceReleasedNewKID(t, sid)
		for _, w := range wantNewKIDs {
			if last == w {
				return last
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitLeaseForceReleased(%s): new_kid %v не достигнут за %s (последний=%q)", sid, wantNewKIDs, timeout, last)
	return ""
}
