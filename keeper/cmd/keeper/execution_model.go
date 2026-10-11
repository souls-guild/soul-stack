package main

import (
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/config"
)

// executionModelOf maps keeper.yml::scenario.execution_model onto the Runner's model.
// The two vocabularies are spelled separately (shared/config cannot import the
// scenario package), and a value the Runner does not know runs linear, so a rename on
// either side would pass config validation and quietly drop the staged rollback.
func executionModelOf(cfg *config.KeeperConfig) scenario.ExecutionModel {
	return scenario.ExecutionModel(cfg.ResolvedExecutionModel())
}
