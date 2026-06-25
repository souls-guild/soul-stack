// Package topology резолвит «какие хосты участвуют в прогоне scenario»:
// roster хостов incarnation по Coven-меткам (ADR-008: `incarnation.name` —
// корневая Coven-метка) + last-reported soulprint-факты из
// `souls.soulprint_facts` (миграция 015, ADR-018).
//
// Слой read-only: только SELECT-ы. Запись soulprint делает
// keeper/internal/grpc (handler SoulprintReport), запись roster/coven —
// keeper/internal/soul. topology потребляет результат для scenario-резолвера
// (M2.x scenario-runner).
//
// Cross-incarnation isolation (ADR-008): резолвер читает хосты строго одной
// incarnation — souls, у которых `incarnation.name` присутствует в `coven[]`.
// Чужие incarnation в результат не попадают.
package topology

import (
	"log/slog"
	"time"
)

// stalenessThreshold — порог «устаревшего» soulprint. Если
// `received_at < now - threshold`, резолвер логирует warn (ADR-018:
// "warn в OTel при skew > 10 мин"). Stale-факты НЕ блокируют прогон —
// scenario работает на last-reported (PM-decision: last-reported + OTel warn).
const stalenessThreshold = 10 * time.Minute

// HostFacts — логическая view хоста прогона: registry-данные `souls`
// (SID, Coven, last-reported soulprint) + declared-роль (источник — Choir
// Voice, fallback — `incarnation.spec.hosts[].role`; ADR-044 п.2, ADR-008,
// scenario/orchestration.md §4.1).
//
// Soulprint — десериализованный JSONB `souls.soulprint_facts` (map, не typed:
// scenario-резолвер обращается к произвольным путям `soulprint.self.<path>`
// через CEL, типизация — на слое proto SoulprintFacts, не здесь).
//
// Role — declared, НЕ actual. Источник по precedence (ADR-044 п.2): role
// Voice-а из `incarnation_choir_voices` (Choir поглотил declared-роль) >
// `spec.hosts[].role` (fallback для хостов БЕЗ Voice и для bootstrap-create,
// wire-совместимость). Может быть пустой ("") для хостов вне declared-spec без
// Voice (ADR-008). Actual-роль — только probe + `where:` на стороне scenario,
// не здесь.
//
// Choirs — имена Choir-ов (ADR-044), в которых SID является Voice (членства из
// `incarnation_choir_voices`, 060_create_choirs.up.sql). Стабильный per-host факт для
// таргетинга `where:` по группе (`X in soulprint.self.choirs`); проецируется в
// `soulprint.self.choirs` и `soulprint.hosts[].choirs` (S-T4, симметрия с Role).
// nil/пустой — хост не состоит ни в одном Choir-е инкарнации (либо push-прогон,
// где Choir-ы неприменимы). Отсортированы лексикографически (детерминизм).
//
// CollectedAt — Soul-side timestamp сбора фактов; ReceivedAt — Keeper-side
// timestamp прихода SoulprintReport. Оба нулевые (time.Time{}), если Soul
// ещё не присылал SoulprintReport (свежеподключённый хост).
//
// Status — legacy lifecycle-снимок `souls.status` (НЕ presence: авторитет
// online — Redis SID-lease, ADR-006(a)). Используется ТОЛЬКО SQL-presence
// fallback-ом резолвера (lease==nil / Redis-сбой); в lease-aware пути presence
// решает lease, status не читается для отбора.
type HostFacts struct {
	SID   string
	Coven []string
	// Traits — operator-set key-value метки хоста (ADR-060): key → (scalar |
	// list). Registry-данные `souls.traits` (миграция 087); проецируются в
	// `soulprint.self.traits` / `soulprint.hosts[].traits` для таргетинга
	// `where:` (registry-проекция, как Coven). nil/пустой — нет меток.
	Traits      map[string]any
	Role        string
	Choirs      []string
	Status      string
	Soulprint   map[string]any
	CollectedAt time.Time
	ReceivedAt  time.Time
}

// stale сообщает, устарел ли soulprint хоста относительно now.
// Нулевой ReceivedAt (Soul ещё не присылал отчёт) — НЕ stale: хост свежий,
// фактов просто ещё нет, отдельный путь, не повод для warn-а здесь.
func (h *HostFacts) stale(now time.Time) bool {
	if h.ReceivedAt.IsZero() {
		return false
	}
	return h.ReceivedAt.Before(now.Add(-stalenessThreshold))
}

// logAttrs — атрибуты для structured-warn-а о stale soulprint.
func (h *HostFacts) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("sid", h.SID),
		slog.Time("received_at", h.ReceivedAt),
	}
}
