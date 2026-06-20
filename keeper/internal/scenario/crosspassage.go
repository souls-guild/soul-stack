package scenario

// Cross-passage requisite-gating ([ADR-056](../../../docs/adr/0056-staged-render-passage.md),
// R3 — финальный слайс класса global-vs-local). Keeper-side резолв onchanges/onfail-
// связи, чей источник лежит в БОЛЕЕ РАННЕМ Passage, чем потребитель.
//
// Зачем keeper-side. requisites (`onchanges:`/`onfail:`) — НЕ passage-определяющие
// (в граф Stratify не входят, passage.go). Если consumer уехал в Passage>0 другой
// register-зависимостью (`where: register.X` от probe), а источник его requisite
// остался в Passage 0, они едут РАЗНЫМИ ApplyRequest-ами. Soul gating ОДНОГО Passage
// видит только свой registerByIdx — результат источника другого Passage ему недоступен
// (ToProtoTasks кодирует его sentinel-ом -1 = «не спасает»). Поэтому requisite-связь
// между Passage обязан резолвить Keeper по накопленным per-host CHANGED/FAILED-фактам
// (R1+R2 эту связь чинили только в пределах одного Passage / отвергали cross-passage).
//
// CHANGED-set-семантика (★). Источник onchanges считается «спас» ТОЛЬКО при CHANGED.
// skipped-источник (когда сам он отфильтрован where: / не сработал по своему requisite)
// = НЕ changed — и onchanges по нему НЕ срабатывает. Опираемся на множество
// auditpg.SelectChangedTaskKeys (статус CHANGED), а НЕ на «строка register есть»
// (register пишется и для ok/skipped probe). onfail — зеркально по FAILED∪TIMED_OUT.
//
// Per-host. CHANGED/FAILED — факт КОНКРЕТНОГО хоста (источник changed на host-a, ok на
// host-b). Поэтому решение «consumer выполняется / исключён» и rewrite его wire-
// requisite принимается per-(sid).

import (
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// crossPassageGate несёт всё необходимое для per-host резолва cross-passage
// requisite-ов ОДНОГО Passage перед его dispatch-ем. Чистые данные (без PG/ctx):
// stage-loop run.go загружает CHANGED/FAILED-факты предыдущих Passage один раз и
// прокидывает сюда. nil-gate (N=1-прогон / Passage 0) → applyGate no-op.
type crossPassageGate struct {
	// passageByIndex — global RenderedTask.Index → его Passage по ВСЕМУ плану
	// прогона (включая источники, которых нет в текущем Passage-срезе). Нужен,
	// чтобы отличить cross-passage источник (его Passage < Passage потребителя) от
	// same-passage (R1-remap его чинит на Soul-е, keeper НЕ трогает).
	passageByIndex map[int]int

	// changed / failed — per-(sid, planIndex) факты CHANGED / (FAILED∪TIMED_OUT),
	// накопленные за Passage < текущего (auditpg). planIndex = global Index.
	changed map[auditpg.ChangedTaskKey]struct{}
	failed  map[auditpg.ChangedTaskKey]struct{}
}

// newCrossPassageGate собирает gate для dispatch-а Passage p. passage — общий план
// прогона (Stratify-результат), tasks — ВЕСЬ резолвнутый план (все Passage; нужен
// для passageByIndex источников). changed/failed — CHANGED/FAILED-факты Passage < p.
func newCrossPassageGate(tasks []*render.RenderedTask, changed, failed map[auditpg.ChangedTaskKey]struct{}) *crossPassageGate {
	idx := make(map[int]int, len(tasks))
	for _, t := range tasks {
		idx[t.Index] = t.Passage
	}
	return &crossPassageGate{passageByIndex: idx, changed: changed, failed: failed}
}

// applyGate переписывает per-host срез Passage по cross-passage requisite-ам:
//
//   - onchanges: ЛЮБОЙ cross-passage источник CHANGED на хосте → requisite
//     УДОВЛЕТВОРЁН (OR-семантика) → cross-passage source-idx убираются с wire
//     (consumer выполняется; same-passage onchanges, если есть, остаются → Soul
//     гейтит по ним). Если НИ ОДИН cross-passage источник не changed И нет
//     same-passage onchanges-источника → consumer ИСКЛЮЧАЕТСЯ из среза хоста
//     (onchanges не сработал — не выполняется). Если cross не changed, но есть
//     same-passage onchanges — consumer остаётся, cross-idx убраны, Soul гейтит
//     по same-passage (R1-remap).
//   - onfail: зеркально по FAILED∪TIMED_OUT (rescue).
//
// nil-gate (N=1 / Passage 0) → возврат perHost как есть (zero-cost). Задачи без
// cross-passage requisite-ов НЕ клонируются (общий указатель переиспользуется).
func (g *crossPassageGate) applyGate(perHost map[string][]*render.RenderedTask, consumerPassage int) map[string][]*render.RenderedTask {
	if g == nil {
		return perHost
	}
	out := make(map[string][]*render.RenderedTask, len(perHost))
	for sid, tasks := range perHost {
		kept := make([]*render.RenderedTask, 0, len(tasks))
		for _, t := range tasks {
			task, include := g.resolveTask(t, sid, consumerPassage)
			if include {
				kept = append(kept, task)
			}
		}
		// Хост, у которого ВСЕ задачи Passage исключены cross-passage-гейтом
		// (например единственный consumer, чей onchanges не сработал), из среза
		// выпадает целиком — apply_runs-строка/ApplyRequest ему НЕ создаётся (как
		// where:, отфильтровавший все задачи на хосте). barrier его не ждёт.
		if len(kept) > 0 {
			out[sid] = kept
		}
	}
	return out
}

// resolveTask решает судьбу одной consumer-задачи на одном хосте. Возвращает
// (возможно клонированную с переписанными requisite-idx) задачу и флаг включения
// в срез хоста. Задача без cross-passage requisite-ов возвращается КАК ЕСТЬ
// (include=true, без клонирования) — keeper её не трогает.
func (g *crossPassageGate) resolveTask(t *render.RenderedTask, sid string, consumerPassage int) (*render.RenderedTask, bool) {
	onchangesCross, onchangesSame := g.splitRequisite(t.OnChangesIdx, consumerPassage)
	onfailCross, onfailSame := g.splitRequisite(t.OnFailIdx, consumerPassage)

	// Нет cross-passage requisite-ов вообще → keeper не трогает (R1-remap на Soul-е).
	if len(onchangesCross) == 0 && len(onfailCross) == 0 {
		return t, true
	}

	// Per-requisite-вид резолвим OR cross-части и решаем, какие idx останутся на wire.
	// nextOnchanges/nextOnfail — wire-idx после резолва; include — оставить ли consumer.
	nextOnchanges, includeOnchanges := g.resolveKind(g.changed, sid, onchangesCross, onchangesSame)
	nextOnfail, includeOnfail := g.resolveKind(g.failed, sid, onfailCross, onfailSame)

	// Связка requisite-видов — AND (как у Soul: несколько requisite-ов должны
	// удовлетвориться вместе). Если onchanges-вид исключил consumer ЛИБО onfail-вид
	// исключил — задача не выполняется на хосте.
	if !includeOnchanges || !includeOnfail {
		return nil, false
	}

	// Consumer выполняется. Клонируем (*RenderedTask общий между хостами — per-host
	// решение, нельзя мутировать) и кладём переписанные wire-idx.
	clone := *t
	clone.OnChangesIdx = nextOnchanges
	clone.OnFailIdx = nextOnfail
	return &clone, true
}

// resolveKind резолвит ОДИН вид requisite (onchanges по changed-set / onfail по
// failed-set) для одного хоста. cross — source-idx в более раннем Passage, same —
// в том же (Soul гейтит сам). Возвращает wire-idx после резолва и флаг включения:
//
//   - cross пуст → keeper этот вид не трогает, same остаётся как есть (include=true).
//   - ЛЮБОЙ cross-источник в set (changed/failed) → OR УДОВЛЕТВОРЁН keeper-side →
//     ВЕСЬ requisite снимается с wire (cross+same), consumer выполняется безусловно
//     по этому виду (нельзя оставлять same: Soul пере-гейтит по нему и мог бы ложно
//     skip-нуть, хотя cross уже спас).
//   - НИ ОДИН cross не в set, но есть same → cross-idx убираем, same оставляем →
//     Soul гейтит по same-passage части (R1-remap).
//   - НИ ОДИН cross не в set И нет same → requisite не удовлетворён → consumer
//     исключается (include=false).
func (g *crossPassageGate) resolveKind(set map[auditpg.ChangedTaskKey]struct{}, sid string, cross, same []int) (wire []int, include bool) {
	if len(cross) == 0 {
		return same, true
	}
	if g.anyKey(set, sid, cross) {
		return nil, true // OR удовлетворён cross-частью → безусловно (снять весь requisite)
	}
	if len(same) > 0 {
		return same, true // cross не спас, но есть same → Soul гейтит по same
	}
	return nil, false // cross не спас и нет same → не выполняется
}

// splitRequisite делит requisite source-idx на cross-passage (источник в более
// раннем Passage, чем consumerPassage) и same-passage (R1-remap чинит сам). idx
// неизвестного источника (нет в passageByIndex) трактуется как same-passage —
// его cross-ref-валидатор/Stratify уже отсеяли бы офлайн; здесь — безопасный
// no-op (keeper не выдумывает cross-passage из висячего idx).
func (g *crossPassageGate) splitRequisite(idxs []int, consumerPassage int) (cross, same []int) {
	for _, srcIdx := range idxs {
		if p, ok := g.passageByIndex[srcIdx]; ok && p < consumerPassage {
			cross = append(cross, srcIdx)
		} else {
			same = append(same, srcIdx)
		}
	}
	return cross, same
}

// anyKey — OR по cross-passage источникам: хоть один (sid, srcIdx) в множестве
// фактов (changed / failed). srcIdx — global plan_index (= auditpg ключ).
func (g *crossPassageGate) anyKey(set map[auditpg.ChangedTaskKey]struct{}, sid string, srcIdxs []int) bool {
	for _, srcIdx := range srcIdxs {
		if _, ok := set[auditpg.ChangedTaskKey{SID: sid, PlanIndex: srcIdx}]; ok {
			return true
		}
	}
	return false
}
