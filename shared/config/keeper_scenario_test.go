package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestKeeperScenario_ExecutionModel(t *testing.T) {
	if got := loadKeeperOrFail(t, consoleBaseConfig).ResolvedExecutionModel(); got != ScenarioExecutionStaged {
		t.Errorf("absent block → %q, want staged", got)
	}
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
scenario:
  execution_model: linear
`)
	if got := cfg.ResolvedExecutionModel(); got != ScenarioExecutionLinear {
		t.Errorf("execution_model: linear → %q", got)
	}
	var nilCfg *KeeperConfig
	if got := nilCfg.ResolvedExecutionModel(); got != ScenarioExecutionStaged {
		t.Errorf("nil config → %q, want staged", got)
	}
}

// A misspelt model must refuse the config: read as staged, a benchmark or a
// rollout labelled linear would silently compare the old order with itself.
//
// Mutation: drop the checkEnum call in schemaValidateKeeper → no error.
func TestKeeperScenario_UnknownExecutionModelRefused(t *testing.T) {
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(consoleBaseConfig+`
scenario:
  execution_model: liner
`), ValidateOptions{})
	for _, d := range diags {
		if d.Level == diag.LevelError && d.Code == "enum_invalid" {
			return
		}
	}
	t.Fatalf("execution_model: liner → diags %+v, want an enum_invalid error", diags)
}
