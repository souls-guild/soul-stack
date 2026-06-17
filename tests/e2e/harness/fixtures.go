//go:build e2e

package harness

// YAML loader для fixtures/<*>.yaml и expectations/<*>.yaml — типизированные
// структуры, отражающие формат spec-а (см. docs/testing/e2e.md).
//
// На pilot-фазе типы определены, чтение из YAML — не реализовано
// (L3a-implementation slice). Согласованность типов с docs/testing/e2e.md
// ловится статически: правка spec-а без правки типов поймается smoke-тестом
// (loader зафейлит ParseError на расхождении).

// SoulsFixture — `tests/e2e/<name>/fixtures/souls.yaml`. Описывает множество
// soul-stub-ов, которые harness регистрирует в Keeper-е перед прогоном.
type SoulsFixture []SoulFixtureEntry

// SoulFixtureEntry — одна строка SoulsFixture.
//
// Status — желаемый статус souls.<sid>.status на момент готовности Stack-а
// ("connected" — стандартный happy-path). Covens — членство в Coven для
// `where:`-таргетинга. Soulprint — содержимое soulprint_facts, кладётся в
// БД через тот же путь, что Soul-side SoulprintReport (но без gRPC, прямой INSERT).
type SoulFixtureEntry struct {
	SID       string         `yaml:"sid"`
	Status    string         `yaml:"status"`
	Covens    []string       `yaml:"covens"`
	Soulprint map[string]any `yaml:"soulprint"`
}

// StubResponsesFixture — `tests/e2e/<name>/fixtures/stub-responses.yaml`.
// Скрипт ответов soul-stub-а: per scenario-name → список scripted RunResult-ов
// на каждый ApplyRequest, который придёт от Keeper-а.
type StubResponsesFixture struct {
	Scenarios map[string]ScenarioScript `yaml:"scenarios"`
}

// ScenarioScript — скрипт ответов soul-stub-а на конкретный scenario-name.
type ScenarioScript struct {
	ApplyResponses []ApplyResponseScript `yaml:"apply_responses"`
}

// ApplyResponseScript — один scripted ответ soul-stub-а.
//
// TaskName — имя задачи (для matching: stub отвечает на ApplyRequest с этим
// task_name выбранным RunResult-ом). RunResult — payload, который stub
// упакует в FromSoul.RunResult и отправит Keeper-у.
type ApplyResponseScript struct {
	TaskName  string         `yaml:"task_name"`
	RunResult map[string]any `yaml:"run_result"`
}

// ExpectationsAfter — `tests/e2e/<name>/expectations/after-<scenario>.yaml`.
// Post-apply ожидания: apply_runs / incarnation.state / audit / metrics.
type ExpectationsAfter struct {
	ApplyRuns        ApplyRunsExpectation    `yaml:"apply_runs"`
	IncarnationState map[string]any          `yaml:"incarnation_state"`
	AuditEvents      []AuditEventExpectation `yaml:"audit_events"`
	Metrics          map[string]string       `yaml:"metrics"`
}

// ApplyRunsExpectation — ожидаемая форма строки apply_runs (status — обязательно).
type ApplyRunsExpectation struct {
	Status string `yaml:"status"`
}

// AuditEventExpectation — ожидание по строке audit_log (type обязательно,
// Payload — deep-subset).
type AuditEventExpectation struct {
	Type    string         `yaml:"type"`
	Payload map[string]any `yaml:"payload"`
}
