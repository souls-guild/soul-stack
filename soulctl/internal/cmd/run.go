package cmd

import "github.com/spf13/cobra"

// newRunCmd is the root of `soulctl run <sub>`. A UX umbrella (Salt-parity), C1.
// Coexists with `soulctl incarnation run` / `soulctl errand exec` without
// deprecation (those are the low-level direct path for CI/scripts; `run` is
// the operator-facing frontend).
//
// Sub-commands:
//   - scenario <service>/<scenario> → POST /v1/voyages (kind=scenario, ADR-043).
//   - cmd <command>                 → POST /v1/voyages (kind=command, ADR-043).
//   - push <destiny@ref>            → POST /v1/push/apply.
//
// All three sub-commands share the `--target-*` flags (see run_target.go).
func newRunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "run",
		Short: "run a scenario / ad-hoc cmd / push program with universal targeting",
		Long: `soulctl run - a high-level UX umbrella over three execution paths:

  run scenario <service>/<scenario>  batch scenario via Voyage (kind=scenario)
  run cmd '<command>'                ad-hoc multi-target shell command (Voyage kind=command)
  run push <destiny@ref>             push-apply via the SSH provider

Universal target flags:
  --target-sids host1,host2          CSV exact-match
  --target-coven prod-eu,dc1         CSV Coven labels (AND)
  --target-glob 'web-*'              shell-glob → CEL sid.glob("X")
  --target-regex 'host-[0-9]+'       regex → CEL sid.matches("X")
  --target-where 'CEL-expression'    raw CEL, AND-merge with glob/regex`,
	}
	c.AddCommand(newRunScenarioCmd(), newRunCmdCmd(), newRunPushCmd())
	return c
}
