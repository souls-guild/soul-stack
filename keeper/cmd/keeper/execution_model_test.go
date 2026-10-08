package main

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Mutation: rename config.ScenarioExecutionLinear's value → the configured linear
// maps to a model the Runner reads as staged.
func TestExecutionModelOf_ConfigValuesAreRunnerModels(t *testing.T) {
	for i, tc := range []struct {
		cfg  *config.KeeperConfig
		want scenario.ExecutionModel
	}{
		{nil, scenario.ExecutionStaged},
		{&config.KeeperConfig{}, scenario.ExecutionStaged},
		{&config.KeeperConfig{Scenario: &config.KeeperScenario{ExecutionModel: config.ScenarioExecutionStaged}}, scenario.ExecutionStaged},
		{&config.KeeperConfig{Scenario: &config.KeeperScenario{ExecutionModel: config.ScenarioExecutionLinear}}, scenario.ExecutionLinear},
	} {
		if got := executionModelOf(tc.cfg); got != tc.want {
			t.Errorf("case %d: executionModelOf = %q, want %q", i, got, tc.want)
		}
	}
}
