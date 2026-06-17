package oracle

import (
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// SubjectMatches проверяет субъектную привязку Decree против хоста-отправителя
// (ADR-030(b)). Субъект Decree — строго XOR (схема CHECK decrees_subject_xor):
//   - SubjectSID задан → match, если subjectSID == *d.SubjectSID;
//   - SubjectCoven задан → match, если есть пересечение SubjectCoven ∩ covens.
//
// subjectSID — авторитетный SID хоста (из mTLS peer cert, НЕ PortentEvent.sid).
// covens — covens хоста из реестра souls (авторитетные, НЕ из payload).
// Субъектная привязка — слой защиты: ограничивает, какие хосты вообще могут
// триггерить правило (недоверенный вход, ADR-030(b)).
func SubjectMatches(d *Decree, subjectSID string, covens []string) bool {
	if d.SubjectSID != nil {
		return *d.SubjectSID == subjectSID
	}
	if len(d.SubjectCoven) == 0 {
		// XOR-инвариант схемы гарантирует, что сюда не дойдём (один из субъектов
		// непуст). Fail-safe: нет субъекта → нет match (default-deny).
		return false
	}
	want := make(map[string]struct{}, len(d.SubjectCoven))
	for _, c := range d.SubjectCoven {
		want[c] = struct{}{}
	}
	for _, c := range covens {
		if _, ok := want[c]; ok {
			return true
		}
	}
	return false
}

// WithinCooldown сообщает, находится ли пара (decree, subject) в окне cooldown-а:
// прошло ли с момента lastFired меньше, чем cooldown Decree-а (ADR-030(a),
// loop-prevention). now — единое опорное время срабатывания.
//
//   - hasFired=false (пара ещё не срабатывала) → false (cooldown не активен);
//   - cooldown <= 0 (выключен, дефолт "0s") → false;
//   - now - lastFired < cooldown → true (заблокировано, skip);
//   - иначе → false (можно срабатывать).
//
// Невалидный cooldown-формат трактуется как 0 (cooldown выключен): валидация
// формата — на service-слое (S3), здесь fail-open по cooldown НЕ ослабляет
// безопасность (cooldown — loop-prevention, не authz; subject + default-deny
// уже отработали).
func WithinCooldown(cooldown string, lastFired time.Time, hasFired bool, now time.Time) bool {
	if !hasFired {
		return false
	}
	d, err := config.ParseDuration(cooldown)
	if err != nil || d <= 0 {
		return false
	}
	return now.Sub(lastFired) < d
}
