package applyrun

// Guard-тесты сортировки глобального read-view прогонов (ADR-068 §B1) на чистой
// функции buildRunsOrderBy: whitelist полей + направление + стабильный tie-break
// apply_id DESC + NULLS LAST для finished_at + byte-exact дефолт. Реальный порядок
// строк на PG — в runsglobal_integration_test.go.

import (
	"errors"
	"strings"
	"testing"
)

// defaultRunsOrderBy — прежний (до ADR-068) хардкод ORDER BY listSQL. Дефолт
// сортировки обязан быть byte-exact ему (guard 6: без sort-параметров ничего не
// меняется).
const defaultRunsOrderBy = "started_at DESC, apply_id DESC"

func TestBuildRunsOrderBy_DefaultByteExact(t *testing.T) {
	got, err := buildRunsOrderBy("", "")
	if err != nil {
		t.Fatalf("buildRunsOrderBy(\"\",\"\"): %v", err)
	}
	if got != defaultRunsOrderBy {
		t.Errorf("дефолт = %q, want byte-exact %q", got, defaultRunsOrderBy)
	}
}

// TestBuildRunsOrderBy_Columns — каждая из 5 whitelist-колонок в обоих
// направлениях даёт корректное выражение со стабильным tie-break (guard 1, 2, 5).
func TestBuildRunsOrderBy_Columns(t *testing.T) {
	cases := []struct {
		sort, dir string
		want      string
	}{
		{"started_at", "asc", "started_at ASC, apply_id DESC"},
		{"started_at", "desc", "started_at DESC, apply_id DESC"},
		{"finished_at", "asc", "finished_at ASC NULLS LAST, apply_id DESC"},
		{"finished_at", "desc", "finished_at DESC NULLS LAST, apply_id DESC"},
		{"status", "asc", "status ASC, apply_id DESC"},
		{"status", "desc", "status DESC, apply_id DESC"},
		{"incarnation", "asc", "incarnation ASC, apply_id DESC"},
		{"incarnation", "desc", "incarnation DESC, apply_id DESC"},
		{"scenario", "asc", "scenario ASC, apply_id DESC"},
		{"scenario", "desc", "scenario DESC, apply_id DESC"},
		// Дефолты применяются независимо: пустое поле → started_at, пустое
		// направление → desc.
		{"", "asc", "started_at ASC, apply_id DESC"},
		{"status", "", "status DESC, apply_id DESC"},
	}
	for _, c := range cases {
		got, err := buildRunsOrderBy(c.sort, c.dir)
		if err != nil {
			t.Errorf("buildRunsOrderBy(%q,%q): %v", c.sort, c.dir, err)
			continue
		}
		if got != c.want {
			t.Errorf("buildRunsOrderBy(%q,%q) = %q, want %q", c.sort, c.dir, got, c.want)
		}
		// Tie-break обязателен во всех кейсах (стабильная пагинация).
		if !strings.HasSuffix(got, ", apply_id DESC") {
			t.Errorf("buildRunsOrderBy(%q,%q) = %q: нет tie-break apply_id DESC", c.sort, c.dir, got)
		}
	}
}

// TestBuildRunsOrderBy_FinishedAtNullsLast — applying-прогоны (finished_at IS NULL)
// уходят в конец при ЛЮБОМ направлении (guard 5).
func TestBuildRunsOrderBy_FinishedAtNullsLast(t *testing.T) {
	for _, dir := range []string{"asc", "desc"} {
		got, err := buildRunsOrderBy("finished_at", dir)
		if err != nil {
			t.Fatalf("buildRunsOrderBy(finished_at,%q): %v", dir, err)
		}
		if !strings.Contains(got, "NULLS LAST") {
			t.Errorf("finished_at %s: %q без NULLS LAST", dir, got)
		}
	}
	// NULLS LAST — только для finished_at (у not-null колонок бессмысленно).
	got, _ := buildRunsOrderBy("started_at", "asc")
	if strings.Contains(got, "NULLS LAST") {
		t.Errorf("started_at не должен нести NULLS LAST: %q", got)
	}
}

// TestBuildRunsOrderBy_InvalidField — не-whitelist поле → sentinel (→ 422). Ловим
// и попытку инъекции, и валидную-в-другом-контексте колонку (created_at из
// incarnation-whitelist здесь недопустима) (guard 3).
func TestBuildRunsOrderBy_InvalidField(t *testing.T) {
	for _, bad := range []string{"created_at", "name", "apply_id", "started_at; DROP TABLE apply_runs", "STARTED_AT"} {
		_, err := buildRunsOrderBy(bad, "asc")
		if !errors.Is(err, ErrInvalidRunsSortField) {
			t.Errorf("buildRunsOrderBy(%q): err=%v, want ErrInvalidRunsSortField", bad, err)
		}
	}
}

// TestBuildRunsOrderBy_InvalidDir — не-asc/desc направление → sentinel (→ 422).
// Верхний регистр не проходит (whitelist строгий) (guard 3).
func TestBuildRunsOrderBy_InvalidDir(t *testing.T) {
	for _, bad := range []string{"sideways", "ASC", "DESC", "ascending", "1"} {
		_, err := buildRunsOrderBy("started_at", bad)
		if !errors.Is(err, ErrInvalidRunsSortDir) {
			t.Errorf("buildRunsOrderBy(started_at,%q): err=%v, want ErrInvalidRunsSortDir", bad, err)
		}
	}
}
