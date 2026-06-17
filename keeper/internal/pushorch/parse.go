// Package pushorch — multi-host push orchestrator (Variant C, architect-вердикт
// a58e1fcd141f8a441). Реализует `POST /v1/push/apply` / `GET /v1/push/{apply_id}`
// и MCP-tool `keeper.push.apply`:
//
//   - render — синтетический одно-таск scenario с apply: destiny (destinyIsolated
//     по конструкции изоляции destiny: register/state/essence/soulprint.hosts
//     недоступны), прогоняется через тот же [render.Pipeline], что scenario-runner;
//   - hosts — read-only из реестра souls (topology.Resolver.LoadByInventory),
//     без incarnation-spec-фазы (Role="" для всех);
//   - destiny — резолвится по `<name>@<ref>` через pushDestinyResolver, который
//     грузит артефакт тем же [artifact.DestinyLoader], что scenario-runner;
//   - dispatch — per-host SendApply через [push.SshDispatcher] (push S1+S5).
//
// Состояние прогона — таблица `push_runs` (миграция 051), per-apply_id с
// inventory[] и per-host summary в jsonb (отдельно от apply_runs, у которой
// per-(apply_id, sid) pull-семантика и cross-keeper barrier).
//
// Async-модель: HTTP-handler принимает запрос, делает Insert(pending) +
// возвращает 202 с apply_id; orchestrator-goroutine выполняет render+dispatch
// и проставляет терминал через MarkTerminal. Orphan-прогоны (Keeper умер
// во время выполнения) подбирает Reaper-rule purge_orphan_push_runs.
package pushorch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// reDestinyName — kebab-case имя destiny (зеркало shared/config/destiny.go
// reDestinyName). Дублируется здесь, чтобы pushorch не тянул config за единой
// строкой regex; синхронизация — через тест на эквивалентность форм.
var reDestinyName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ErrInvalidDestinyRef — `destiny`-поле запроса не соответствует форме
// `<name>@<ref>`. Sentinel для маппинга в 422 validation-failed.
var ErrInvalidDestinyRef = errors.New("pushorch: invalid destiny ref (expected <name>@<ref>)")

// ParseDestinyRef разбирает строку запроса `destiny` в (name, ref). Форма
// зафиксирована [docs/keeper/operator-api.md → Push endpoints]: ровно один `@`
// между непустыми частями, name — kebab-case (regex reDestinyName), ref —
// произвольная непустая строка (детальная валидация git-ref-формы — backlog,
// симметрично shared/config/service.go::DependencyRef.Ref).
//
// Whitespace по краям не допускается: оператор передал «грязный» ref —
// validation-failed, а не silent-trim (предсказуемое сравнение с реестром).
func ParseDestinyRef(s string) (name, ref string, err error) {
	if s == "" {
		return "", "", fmt.Errorf("%w: empty string", ErrInvalidDestinyRef)
	}
	if strings.TrimSpace(s) != s {
		return "", "", fmt.Errorf("%w: leading/trailing whitespace", ErrInvalidDestinyRef)
	}
	// ПЕРВЫЙ `@` — разделитель: имя destiny по regex reDestinyName не содержит
	// `@`, а git-ref может (branch-name типа `feature@odd`). Берём первый,
	// остальное (вплоть до конца строки) — ref.
	idx := strings.IndexByte(s, '@')
	if idx < 0 {
		return "", "", fmt.Errorf("%w: missing '@' separator in %q", ErrInvalidDestinyRef, s)
	}
	name = s[:idx]
	ref = s[idx+1:]
	if name == "" {
		return "", "", fmt.Errorf("%w: empty name in %q", ErrInvalidDestinyRef, s)
	}
	if ref == "" {
		return "", "", fmt.Errorf("%w: empty ref in %q", ErrInvalidDestinyRef, s)
	}
	if !reDestinyName.MatchString(name) {
		return "", "", fmt.Errorf("%w: name %q must match kebab-case", ErrInvalidDestinyRef, name)
	}
	return name, ref, nil
}
