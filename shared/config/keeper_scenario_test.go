package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Mutation: resolve an absent key to ScenarioExecutionStaged → red.
func TestKeeperScenario_ExecutionModel(t *testing.T) {
	if got := loadKeeperOrFail(t, consoleBaseConfig).ResolvedExecutionModel(); got != ScenarioExecutionLinear {
		t.Errorf("absent block → %q, want linear", got)
	}
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
scenario:
  execution_model: staged
`)
	if got := cfg.ResolvedExecutionModel(); got != ScenarioExecutionStaged {
		t.Errorf("execution_model: staged → %q", got)
	}
	var nilCfg *KeeperConfig
	if got := nilCfg.ResolvedExecutionModel(); got != ScenarioExecutionLinear {
		t.Errorf("nil config → %q, want linear", got)
	}
}

// A misspelt model must refuse the config: read as linear, an operator rolling
// back to staged would silently keep the new order.
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
