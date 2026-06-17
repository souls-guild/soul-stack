// Package soulpurview — резолвер scoped-видимости списка Souls по [rbac.Purview]
// (ADR-047 S3b). Souls-аналог keeper/internal/statepredicate (тот резолвит
// инкарнации по state-CEL; этот — souls по scope-измерениям Purview).
//
// Резолвер принимает уже-резолвнутый [rbac.Purview] ПАРАМЕТРОМ и НЕ обращается к
// enforcer сам: ResolvePurview зовёт вызывающая сторона (handler), а soulpurview
// лишь переводит верхнюю границу scope в параметры souls-запроса. Это держит
// пакет свободным от RBAC-резолва (S4 target-фильтр переиспользует тот же
// перевод поверх своего Purview-пересечения) и однонаправленным:
// soulpurview→rbac, без import-cycle.
//
// Перф-стратегия (фундамент, расширяемый additive-слайсами как Purview-измерения):
//   - S3b-0: coven-измерение — чистый SQL-pushdown `souls.coven &&
//     ARRAY[purview.Covens]` (offset/total корректны без дрейфа, keyset не нужен).
//   - S3b-2a: regex-измерение — keyset-окно по `(registered_at, sid)`
//   - Go-OR-постфильтр поверх внутренних страниц. Наличие regex ОТКЛЮЧАЕТ
//     coven-SQL-pushdown (иначе AND сузил бы НИЖЕ Purview): видимость хоста =
//     covenMatch OR regexMatch вычисляется в Go ([CompiledScope.Visible]).
//     Single-read ([InScope]) использует тот же coven+regex предикат — list↔get
//     консистентны (рассинхрон coven-only InScope устранён gate-fix-ом).
//   - S3b-2b: soulprint/state измерения — page-CEL-постфильтр (ещё не вычисляются;
//     [Scope.Partial] помечает scope, который пилот не может выразить, чтобы
//     потребитель знал: результат НЕ полон до S3b-2b).
package soulpurview

import (
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// MaxRegexLen — верхняя граница длины одного scope-паттерна (ReDoS-страховка).
// RE2 (google/re2 — линейное время, без backtracking) безопасен по своей
// природе, но патологически длинный паттерн всё равно отвергаем на компиляции:
// scope-паттерны короткие по построению (`^web-`, `^db-\d+`), длинный — почти
// наверняка ошибка/злоупотребление.
const MaxRegexLen = 256

// Scope — перевод [rbac.Purview] в параметры souls-запроса (верхняя граница
// видимости оператора). Терминальные флаги взаимоисключающи с Covens.
//
// fail-closed (ADR-047): неопределённость трактуется как «скрыть», НЕ как
// «показать весь флот». Это ПРОТИВОПОЛОЖНО presence-overlay (`GET /v1/souls`
// при ошибке Redis fail-SAFE — отдаёт PG-снимок): scope при сомнении скрывает,
// presence при сомнении показывает. Эти два слоя НЕ должны заимствовать друг у
// друга стратегию (см. handler.List).
type Scope struct {
	// Covens — coven-метки, на которые распространяется видимость (дедуп,
	// отсортировано — как в [rbac.Purview]). В coven-only-режиме применяется
	// SQL-pushdown-ом `coven && ARRAY[Covens]`; в keyset-режиме (есть Regexes) —
	// одно из двух OR-измерений [CompiledScope.Visible]. Значим только при
	// !Unrestricted && !Empty.
	Covens []string

	// Regexes — RE2-паттерны по SID (ADR-047 S2a/S3b-2a). Видимость хоста =
	// covenMatch(Covens) OR regexMatch(Regexes) — союз, НЕ пересечение. Наличие
	// Regexes переводит souls-list в keyset-режим (см. [Scope.NeedsKeyset]):
	// coven-SQL-pushdown отключается, OR-фильтр считается в Go.
	Regexes []string

	// Unrestricted — нет scope-ограничений: весь доступный список без фильтра.
	Unrestricted bool

	// Empty — fail-closed: оператору не положено НИ ОДНОГО хоста для (resource,
	// action). Результат — ПУСТОЙ список (НЕ весь флот). Главный security-
	// инвариант: Purview{} (ни одного измерения, не Unrestricted) → Empty=true.
	Empty bool

	// Partial — введены измерения сверх coven/regex (soulprint/state), которые
	// пилот ещё НЕ вычисляет (page-CEL — S3b-2b). Результат при Partial НЕ полон:
	// он опускает хосты, доступные ТОЛЬКО по soulprint/state. Потребитель трактует
	// это как «пока не поддержано» (handler не выдаёт частичный набор за полный).
	// regex с S3b-2a из Partial исключён — он вычисляется keyset-фильтром.
	Partial bool
}

// NeedsKeyset — требует ли scope keyset-режима (есть regex-измерение, которое
// нельзя выразить чистым coven-SQL-pushdown-ом без сужения ниже Purview).
// coven-only / Unrestricted / Empty → offset fast-path (false).
func (s Scope) NeedsKeyset() bool { return len(s.Regexes) > 0 }

// Resolve переводит [rbac.Purview] в [Scope] для souls-запроса.
//
// Семантика (симметрична statepredicate-ветвлению по Purview):
//   - Unrestricted=true → Scope{Unrestricted:true} (весь список);
//   - есть Covens и/или Regexes → Scope{Covens, Regexes} (coven OR regex —
//     coven-pushdown при отсутствии regex, иначе keyset+Go-OR-постфильтр);
//   - введены soulprint/state (не вычисляются в S3b-2a) → Partial=true (доступ
//     есть, но пилот его не считает — S3b-2b);
//   - совсем пусто (Purview{}, не Unrestricted) → Empty=true (fail-closed).
//
// Deny из Purview (заготовка S2) трактуется как Empty (fail-closed) — defensive,
// в coven-MVP Purview.Deny не выставляется.
func Resolve(p rbac.Purview) Scope {
	if p.Unrestricted {
		return Scope{Unrestricted: true}
	}
	if p.Deny {
		return Scope{Empty: true}
	}

	// soulprint/state — измерения, которые S3b-2a НЕ вычисляет (coven+regex —
	// вычисляет). Их наличие помечает результат Partial (не полон до S3b-2b).
	hasUnsupported := len(p.SoulprintExprs) > 0 || len(p.StateExprs) > 0
	hasComputable := len(p.Covens) > 0 || len(p.Regexes) > 0

	if !hasComputable && !hasUnsupported {
		// Ни одного введённого измерения и не Unrestricted → fail-closed.
		return Scope{Empty: true}
	}

	return Scope{
		Covens:  p.Covens,
		Regexes: p.Regexes,
		Partial: hasUnsupported,
	}
}

// CompiledScope — [Scope] с предкомпилированными RE2-паттернами (компиляция
// один раз на запрос, не на хост). Полученный из [CompileScope] объект знает,
// виден ли хост в OR-границе scope.
type CompiledScope struct {
	unrestricted bool
	empty        bool
	covens       []string
	regexes      []*regexp.Regexp
}

// CompileScope компилирует regex-паттерны scope ОДИН раз. Битый паттерн или
// паттерн сверх [MaxRegexLen] → ошибка (caller трактует fail-CLOSED: скрыть/
// пусто, НЕ 500 и НЕ over-show). RE2 (Go regexp) — линейное время, поэтому
// ReDoS невозможен; лимит длины — лишь страховка от патологического ввода.
func CompileScope(s Scope) (CompiledScope, error) {
	cs := CompiledScope{
		unrestricted: s.Unrestricted,
		empty:        s.Empty,
		covens:       s.Covens,
	}
	for _, pat := range s.Regexes {
		if len(pat) > MaxRegexLen {
			return CompiledScope{}, fmt.Errorf("soulpurview: regex pattern too long (%d > %d)", len(pat), MaxRegexLen)
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return CompiledScope{}, fmt.Errorf("soulpurview: invalid regex %q: %w", pat, err)
		}
		cs.regexes = append(cs.regexes, re)
	}
	return cs, nil
}

// Visible — виден ли хост (sid + covens) в OR-границе scope:
//
//	visible ⟺ covenMatch(soulCovens, scope.Covens) OR regexMatch(sid, scope.Regexes)
//
// Union (OR), НЕ пересечение: хост, матчащий ХОТЯ БЫ ОДНО измерение, виден —
// иначе keyset-фильтр сузил бы видимость НИЖЕ Purview оператора. Терминалы:
// Unrestricted → всегда true; Empty → всегда false (fail-closed).
func (cs CompiledScope) Visible(sid string, soulCovens []string) bool {
	if cs.unrestricted {
		return true
	}
	if cs.empty {
		return false
	}
	for _, sc := range cs.covens {
		for _, hc := range soulCovens {
			if sc == hc {
				return true
			}
		}
	}
	for _, re := range cs.regexes {
		if re.MatchString(sid) {
			return true
		}
	}
	return false
}

// InScope — виден ли ОДИН хост (sid + covens=soulCovens) в OR-границе scope
// (single-object проверка для `GET /v1/souls/{sid}`, `/soulprint`, `/history`,
// ADR-047 S3b-1). Та же union-семантика, что у [CompiledScope.Visible] в keyset-
// пути List — list фильтрует выборку, single-read проверяет конкретный хост, оба
// решают видимость одним предикатом:
//
//	visible ⟺ covenMatch(soulCovens, scope.Covens) OR regexMatch(sid, scope.Regexes)
//
// fail-closed (симметрично [Scope] и [CompiledScope.Visible]):
//   - Unrestricted → true (любой хост, в т.ч. без covens);
//   - Empty → false (нет ни одного видимого хоста — оператор без прав);
//   - eval-error (битый/слишком длинный regex в Purview, [CompileScope] error) →
//     false (скрыть, НЕ показать и НЕ 500): неопределённость = «вне scope»;
//   - иначе → covenMatch OR regexMatch (через [CompiledScope.Visible]).
//
// S3b-2a→gate-fix: InScope теперь coven+regex (list↔get рассинхрон S3b-2a устранён
// — regex-видимый в List хост доступен и по GET /{sid}). soulprint/state-измерения
// (Scope.Partial) остаются отложенными до S3b-2b: они НЕ вычисляются здесь, что
// даёт строгое сужение (под-показ, никогда НЕ over-show — оператор может
// недосчитаться хоста, доступного ТОЛЬКО по soulprint, но чужого не увидит).
// Пустой scope (ни Covens, ни Regexes) при !Unrestricted → false.
func InScope(scope Scope, sid string, soulCovens []string) bool {
	if scope.Unrestricted {
		return true
	}
	if scope.Empty {
		return false
	}
	compiled, err := CompileScope(scope)
	if err != nil {
		// eval-error fail-CLOSED: битый regex в Purview скрывает хост (как в
		// listKeyset), а не палит существование и не падает в 500.
		return false
	}
	return compiled.Visible(sid, soulCovens)
}
