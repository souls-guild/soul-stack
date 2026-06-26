package statemigrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// fixtureDir — путь к реальным фикстурам консолидированного redis (авторитет
// над docs-примерами). Миграция 001_to_002 — демо грамматики DSL (rename + set +
// foreach + delete), перенос инварианта из прежнего redis-cluster.
const fixtureDir = "../../../examples/service/redis/migrations"

func mustEvaluator(t *testing.T) Evaluator {
	t.Helper()
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return ev
}

func mustParseFile(t *testing.T, path string) *Migration {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse %s: %v", path, err)
	}
	return m
}

// migrationTestCase — формат tests/<case>.yml (state_before → migration →
// assert state_after).
type migrationTestCase struct {
	Name        string         `yaml:"name"`
	StateBefore map[string]any `yaml:"state_before"`
	StateAfter  map[string]any `yaml:"state_after"`
}

// TestApply_AllFixtures — GENERIC-обход всех фикстур миграций redis-сервиса. Для
// каждой пары «файл миграции <N>_to_<M>.yml + каталог <N>_to_<M>/tests/*.yml»
// прогоняет ОДИН шаг <N>→<M> на state_before и сверяет с state_after. Это гейт от
// молчаливой регрессии: любая существующая фикстура (вкл. 4 шт. 005_to_006)
// исполняется без ручного хардкода пути. Авторитет над docs-примерами.
//
// Каждый каталог тестов привязан к одношаговой миграции (имя каталога = имя файла
// без .yml), а версии шага берутся из самого файла (Parse). Поэтому достаточно
// одношаговой Chain — multi-step цепочки покрываются step-снапшот-тестами ниже.
func TestApply_AllFixtures(t *testing.T) {
	ev := mustEvaluator(t)

	migFiles, err := filepath.Glob(filepath.Join(fixtureDir, "*_to_*.yml"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(migFiles) == 0 {
		t.Fatalf("не найдено ни одного файла миграции в %s", fixtureDir)
	}

	var totalCases int
	for _, migFile := range migFiles {
		stepName := strings.TrimSuffix(filepath.Base(migFile), ".yml") // напр. 005_to_006
		caseFiles, err := filepath.Glob(filepath.Join(fixtureDir, stepName, "tests", "*.yml"))
		if err != nil {
			t.Fatalf("glob cases %s: %v", stepName, err)
		}
		if len(caseFiles) == 0 {
			// Миграция без фикстур — потенциальная дыра покрытия, но не падение
			// (некоторые шаги могут быть тривиальными). Помечаем явно.
			t.Logf("миграция %s: тест-фикстур нет", stepName)
			continue
		}

		mig := mustParseFile(t, migFile)
		for _, caseFile := range caseFiles {
			caseName := stepName + "/" + strings.TrimSuffix(filepath.Base(caseFile), ".yml")
			t.Run(caseName, func(t *testing.T) {
				data, err := os.ReadFile(caseFile)
				if err != nil {
					t.Fatalf("read case: %v", err)
				}
				var tc migrationTestCase
				if err := yaml.Unmarshal(data, &tc); err != nil {
					t.Fatalf("unmarshal case: %v", err)
				}

				res, err := Apply(context.Background(), tc.StateBefore, Chain{mig}, ev)
				if err != nil {
					t.Fatalf("Apply %s: %v", caseName, err)
				}
				assertDeepEqualJSON(t, res.FinalState, tc.StateAfter)
			})
			totalCases++
		}
	}

	t.Logf("прогнано миграций: %d, тест-кейсов: %d", len(migFiles), totalCases)
}

// TestApply_RealFixture_EmptyUsers — пустой список пользователей даёт пустой map.
// Миграция 001_to_002 явным `set state.redis_users {}` ПЕРЕД foreach
// материализует целевой ключ (intent «список стал map»), поэтому foreach по []
// (no-op) оставляет redis_users: {}, а не отсутствие ключа.
func TestApply_RealFixture_EmptyUsers(t *testing.T) {
	mig := mustParseFile(t, filepath.Join(fixtureDir, "001_to_002.yml"))
	ev := mustEvaluator(t)

	in := map[string]any{"redis_users": []any{}, "redis_type": "cluster"}
	res, err := Apply(context.Background(), in, Chain{mig}, ev)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertDeepEqualJSON(t, res.FinalState, map[string]any{
		"redis_users": map[string]any{},
		"redis_type":  "cluster",
	})
}

// TestApply_EmptyForeachNoMaterialize — engine-инвариант: foreach по пустому
// списку сам по себе ключ не создаёт (no-op без тела). Проверяется на
// синтетической миграции БЕЗ предварительного set, чтобы зафиксировать
// поведение движка независимо от intent конкретной фикстуры.
func TestApply_EmptyForeachNoMaterialize(t *testing.T) {
	ev := mustEvaluator(t)
	mig := &Migration{FromVersion: 1, ToVersion: 2, Transform: []Op{
		{Rename: &RenameOp{From: "state.redis_users", To: "state.redis_users_legacy_v1"}},
		{Foreach: &ForeachOp{In: "${ state.redis_users_legacy_v1 }", As: "user_name", Do: []Op{
			{Set: &SetOp{Path: "state.redis_users.${ user_name }", Value: map[string]any{"perms": "x"}}},
		}}},
		{Delete: &DeleteOp{Path: "state.redis_users_legacy_v1"}},
	}}

	in := map[string]any{"redis_users": []any{}, "redis_type": "cluster"}
	res, err := Apply(context.Background(), in, Chain{mig}, ev)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertDeepEqualJSON(t, res.FinalState, map[string]any{"redis_type": "cluster"})
}

// TestApply_DoesNotMutateInput — входной state caller-а не мутируется.
func TestApply_DoesNotMutateInput(t *testing.T) {
	mig := mustParseFile(t, filepath.Join(fixtureDir, "001_to_002.yml"))
	ev := mustEvaluator(t)

	in := map[string]any{"redis_users": []any{"app"}, "redis_type": "standalone"}
	snapshot := map[string]any{"redis_users": []any{"app"}, "redis_type": "standalone"}

	if _, err := Apply(context.Background(), in, Chain{mig}, ev); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("входной state мутирован: %#v", in)
	}
}

// TestApply_StepSnapshots — snapshot до/после на каждый шаг цепочки.
func TestApply_StepSnapshots(t *testing.T) {
	ev := mustEvaluator(t)
	chain := Chain{
		{FromVersion: 1, ToVersion: 2, Transform: []Op{
			{Set: &SetOp{Path: "state.a", Value: 1}},
		}},
		{FromVersion: 2, ToVersion: 3, Transform: []Op{
			{Set: &SetOp{Path: "state.b", Value: 2}},
		}},
	}
	res, err := Apply(context.Background(), map[string]any{}, chain, ev)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2", len(res.Steps))
	}
	if _, ok := res.Steps[0].StateBefore["a"]; ok {
		t.Errorf("step0.StateBefore не должен содержать a")
	}
	if res.Steps[0].StateAfter["a"] != float64(1) && res.Steps[0].StateAfter["a"] != 1 {
		t.Errorf("step0.StateAfter[a] = %v", res.Steps[0].StateAfter["a"])
	}
	if res.Steps[1].FromVersion != 2 || res.Steps[1].ToVersion != 3 {
		t.Errorf("step1 версии = %d→%d", res.Steps[1].FromVersion, res.Steps[1].ToVersion)
	}
}

// TestApply_ChainVersionGap — разрыв версий в цепочке → ошибка.
func TestApply_ChainVersionGap(t *testing.T) {
	ev := mustEvaluator(t)
	chain := Chain{
		{FromVersion: 1, ToVersion: 2},
		{FromVersion: 3, ToVersion: 4}, // разрыв 2 != 3
	}
	_, err := Apply(context.Background(), map[string]any{}, chain, ev)
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassChainVersion {
		t.Fatalf("ошибка = %v, want ClassChainVersion", err)
	}
}

func assertDeepEqualJSON(t *testing.T, got, want map[string]any) {
	t.Helper()
	// Нормализуем числовые типы через JSON round-trip (YAML int vs Apply
	// сохраняет cel int64 — сравниваем в единой форме).
	if !reflect.DeepEqual(normalizeJSON(t, got), normalizeJSON(t, want)) {
		t.Errorf("state mismatch:\n got = %#v\nwant = %#v", got, want)
	}
}

func normalizeJSON(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	return deepCopyMap(m)
}
