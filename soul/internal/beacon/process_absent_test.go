package beacon

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

func TestProcessAbsentPresent(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("pgrep nginx", util.Result{ExitCode: 0, Stdout: "1234\n"})

	b := &ProcessAbsent{Runner: r}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"pattern": "nginx"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateProcessPresent {
		t.Fatalf("state = %q, want present", state)
	}
	if data.GetFields()["pattern"].GetStringValue() != "nginx" {
		t.Error("data.pattern must carry the pattern")
	}
}

func TestProcessAbsentAbsent(t *testing.T) {
	r := internaltest.NewRunner()
	// pgrep exit 1 = no matches.
	r.On("pgrep nginx", util.Result{ExitCode: 1})

	b := &ProcessAbsent{Runner: r}
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"pattern": "nginx"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateProcessAbsent {
		t.Fatalf("state = %q, want absent", state)
	}
}

func TestProcessAbsentPgrepError(t *testing.T) {
	r := internaltest.NewRunner()
	// pgrep exit ≥2 — pgrep itself errored (bad pattern) → Check error.
	r.On("pgrep [bad", util.Result{ExitCode: 2, Stderr: "invalid pattern"})

	b := &ProcessAbsent{Runner: r}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"pattern": "[bad"})); err == nil {
		t.Fatal("expected an error on pgrep exit >=2")
	}
}

func TestProcessAbsentRunnerLaunchError(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("pgrep nginx", util.Result{Err: errors.New("pgrep: not found")})

	b := &ProcessAbsent{Runner: r}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"pattern": "nginx"})); err == nil {
		t.Fatal("expected an error when pgrep fails to start")
	}
}

func TestProcessAbsentMissingPattern(t *testing.T) {
	b := &ProcessAbsent{Runner: internaltest.NewRunner()}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("expected an error when param pattern is missing")
	}
}
