package main

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ★ GUARD (NIM-952): a keeper.yml without the key runs linear.
//
// Mutations: rename config.ScenarioExecutionStaged's value → the configured staged
// rollback maps to a model the Runner reads as linear; resolve an absent key to
// staged → the first two cases go red.
func TestExecutionModelOf_ConfigValuesAreRunnerModels(t *testing.T) {
	for i, tc := range []struct {
		cfg  *config.KeeperConfig
		want scenario.ExecutionModel
	}{
		{nil, scenario.ExecutionLinear},
		{&config.KeeperConfig{}, scenario.ExecutionLinear},
		{&config.KeeperConfig{Scenario: &config.KeeperScenario{ExecutionModel: config.ScenarioExecutionStaged}}, scenario.ExecutionStaged},
		{&config.KeeperConfig{Scenario: &config.KeeperScenario{ExecutionModel: config.ScenarioExecutionLinear}}, scenario.ExecutionLinear},
	} {
		if got := executionModelOf(tc.cfg); got != tc.want {
			t.Errorf("case %d: executionModelOf = %q, want %q", i, got, tc.want)
		}
	}
}
