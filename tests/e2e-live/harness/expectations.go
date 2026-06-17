//go:build e2e_live

package harness

// YAML loader для post-apply expectations (L3b-5).
//
// Формат — расширение L3a (см. tests/e2e/harness/fixtures.go +
// docs/testing/e2e.md): к apply_runs / incarnation_state / audit_events / metrics
// добавляется новая секция host_state — per-soul container-side ожидания
// (packages / services / files), которые проверяются Exec-ом внутри
// privileged-Debian-12-soul-контейнера через AssertHost*-методы (L3b-4).
//
// LoadExpectations читает YAML с диска и валидирует структуру (ровно
// yaml.Unmarshal без скрытых трансформаций). AssertExpectations применяет весь
// набор проверок одним вызовом — caller не разбирается с поэтапной
// валидацией. parseMetricThreshold обрабатывает constraint-выражения вида
// ">= 1" / "== 0" / "> 3" / "1" (голое число эквивалентно ">= число").

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Expectations — пост-apply ожидания одного scenario-прогона (формат
// `<test>/expectations/<phase>.yaml`).
//
// apply_runs/incarnation_state/audit_events/metrics — симметричны L3a-фикстуре
// (tests/e2e/harness/fixtures.ExpectationsAfter); host_state — новая секция
// L3b для container-side ожиданий.
type Expectations struct {
	ApplyRuns        ApplyRunsExpectation    `yaml:"apply_runs"`
	IncarnationState map[string]any          `yaml:"incarnation_state"`
	AuditEvents      []AuditEventExpectation `yaml:"audit_events"`
	Metrics          map[string]string       `yaml:"metrics"`
	HostState        []HostStateExpectation  `yaml:"host_state"`
}

// ApplyRunsExpectation — ожидаемая форма apply_runs.status (значение из
// real-enum keeper/internal/applyrun).
type ApplyRunsExpectation struct {
	Status string `yaml:"status"`
}

// AuditEventExpectation — presence-проверка по audit_log (type обязателен,
// payload — deep-subset).
type AuditEventExpectation struct {
	Type    string         `yaml:"type"`
	Payload map[string]any `yaml:"payload"`
}

// HostStateExpectation — container-side ожидания одного soul-хоста.
//
// Soul — FQDN, должен совпадать с одним из Stack.SoulContainers[i].SID;
// AssertExpectations резолвит индекс через findSoulIdx и фейлит, если sid
// неизвестен (помогает поймать опечатки в YAML).
type HostStateExpectation struct {
	Soul     string                `yaml:"soul"`
	Packages map[string]string     `yaml:"packages"` // pkg → status (только "installed" в MVP)
	Services map[string]string     `yaml:"services"` // svc → state  (только "active"    в MVP)
	Files    []HostFileExpectation `yaml:"files"`
}

// HostFileExpectation — ожидание по файлу внутри контейнера.
//
// Path обязателен (AssertHostFileExists); Contains опционален: если непустой,
// дополнительно проверяется AssertHostFileContent.
type HostFileExpectation struct {
	Path     string `yaml:"path"`
	Contains string `yaml:"contains"`
}

// LoadExpectations читает YAML-файл expectations с диска и парсит в типизированную
// структуру. Strict-mode: KnownFields(true) — лишние ключи (опечатки в схеме)
// ловятся на старте теста, не на assert-фазе.
func LoadExpectations(t *testing.T, path string) *Expectations {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("LoadExpectations(%s): read: %v", path, err)
	}
	var e Expectations
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("LoadExpectations(%s): yaml decode: %v", path, err)
	}
	if e.ApplyRuns.Status != "" {
		CheckApplyRunsStatusValid(t, e.ApplyRuns.Status)
	}
	return &e
}

// AssertExpectations применяет все четыре блока expectations к стенду в
// детерминированном порядке: apply_runs → incarnation_state → audit_events →
// metrics → host_state. Каждый блок — отдельный assert-метод; при провале
// одного fail переходит в t.Fatal внутри метода (короткий цикл — не имеет
// смысла продолжать).
func (s *Stack) AssertExpectations(t *testing.T, e *Expectations, applyID, incName string) {
	t.Helper()

	if e.ApplyRuns.Status != "" {
		s.AssertApplyRunsStatus(t, applyID, e.ApplyRuns.Status)
	}

	if len(e.IncarnationState) > 0 {
		s.AssertIncarnationState(t, incName, e.IncarnationState)
	}

	for _, ae := range e.AuditEvents {
		s.AssertAuditEvent(t, ae.Type, ae.Payload)
	}

	for query, expr := range e.Metrics {
		threshold, op, err := parseMetricThreshold(expr)
		if err != nil {
			t.Fatalf("AssertExpectations: metric %q: %v", query, err)
		}
		switch op {
		case ">=":
			s.AssertMetricGE(t, query, threshold)
		default:
			// MVP: только `>=` поддерживается (как и L3a/L3b к моменту L3b-5).
			// Остальные операторы — расширение без breaking change YAML-формата.
			t.Fatalf("AssertExpectations: metric %q: оператор %q пока не поддержан (только '>=')",
				query, op)
		}
	}

	for _, hs := range e.HostState {
		soulIdx := s.findSoulIdx(hs.Soul)
		if soulIdx < 0 {
			knownSIDs := make([]string, 0, len(s.SoulContainers))
			for _, sc := range s.SoulContainers {
				knownSIDs = append(knownSIDs, sc.SID)
			}
			t.Fatalf("host_state: soul %q не найден в Stack.SoulContainers (known=%v)",
				hs.Soul, knownSIDs)
		}
		for pkg, status := range hs.Packages {
			switch status {
			case "installed":
				s.AssertHostPkgInstalled(t, soulIdx, pkg)
			default:
				t.Fatalf("host_state(%s).packages[%s]: статус %q не поддержан (только 'installed')",
					hs.Soul, pkg, status)
			}
		}
		for svc, state := range hs.Services {
			switch state {
			case "active":
				s.AssertHostServiceActive(t, soulIdx, svc)
			default:
				t.Fatalf("host_state(%s).services[%s]: state %q не поддержан (только 'active')",
					hs.Soul, svc, state)
			}
		}
		for _, f := range hs.Files {
			if f.Path == "" {
				t.Fatalf("host_state(%s).files: пустой path", hs.Soul)
			}
			s.AssertHostFileExists(t, soulIdx, f.Path)
			if f.Contains != "" {
				s.AssertHostFileContent(t, soulIdx, f.Path, f.Contains)
			}
		}
	}
}

// findSoulIdx возвращает индекс SoulContainer-а по SID или -1, если нет.
func (s *Stack) findSoulIdx(sid string) int {
	for i, sc := range s.SoulContainers {
		if sc != nil && sc.SID == sid {
			return i
		}
	}
	return -1
}

// parseMetricThreshold парсит constraint-выражение вида ">= 1" / "== 0" / "> 3"
// или голое число "1" (трактуется как ">= 1"). Возвращает (значение,
// оператор, error). Оператор нормализован к одному из {">=", "==", ">", "<=", "<"};
// для голого числа — ">=".
func parseMetricThreshold(expr string) (float64, string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return 0, "", fmt.Errorf("пустое constraint-выражение")
	}

	for _, op := range []string{">=", "<=", "==", ">", "<"} {
		if strings.HasPrefix(expr, op) {
			rest := strings.TrimSpace(expr[len(op):])
			v, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				return 0, "", fmt.Errorf("число после %q не парсится: %q", op, rest)
			}
			return v, op, nil
		}
	}

	v, err := strconv.ParseFloat(expr, 64)
	if err != nil {
		return 0, "", fmt.Errorf("ни оператор, ни голое число: %q", expr)
	}
	return v, ">=", nil
}
