package cmd

import "github.com/spf13/cobra"

// newRunCmd — корень `soulctl run <sub>`. UX-зонтик (Salt-parity), C1.
// Coexist с `soulctl incarnation run` / `soulctl errand exec` без deprecation
// (low-level прямой путь для CI/скриптов; `run` — оператор-frontend).
//
// Sub-команды:
//   - scenario <service>/<scenario> → POST /v1/voyages (kind=scenario, ADR-043).
//   - cmd <command>                 → POST /v1/voyages (kind=command, ADR-043).
//   - push <destiny@ref>            → POST /v1/push/apply.
//
// Все три sub-команды разделяют `--target-*` флаги (см. run_target.go).
func newRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "run",
		Short: "запуск scenario / ad-hoc cmd / push-prog с универсальным таргетингом",
		Long: `soulctl run — высокоуровневый UX-зонтик над тремя путями исполнения:

  run scenario <service>/<scenario>  батчевый scenario через Voyage (kind=scenario)
  run cmd '<команда>'                ad-hoc multi-target shell-команда (Voyage kind=command)
  run push <destiny@ref>             push-применение через SSH-провайдер

Универсальные target-флаги:
  --target-sids host1,host2          CSV exact-match
  --target-coven prod-eu,dc1         CSV Coven-меток (AND)
  --target-glob 'web-*'              shell-glob → CEL sid.glob("X")
  --target-regex 'host-[0-9]+'       regex → CEL sid.matches("X")
  --target-where 'CEL-выражение'     raw CEL, AND-merge с glob/regex`,
	}
	c.AddCommand(newRunScenarioCmd(), newRunCmdCmd(), newRunPushCmd())
	return c
}
