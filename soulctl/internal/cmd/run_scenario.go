package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/output"
)

// newRunScenarioCmd — `soulctl run scenario <service>/<scenario>`. A batched
// scenario run via Voyage `kind=scenario` (ADR-043, UI parity). Backend:
// POST /v1/voyages body `{kind: "scenario", scenario_name, target:{...}, ...}`.
// Resolving the incarnation:
//   - explicit `--incarnation` (a single target);
//   - or auto-detect by service (exactly one incarnation per service).
//
// The `--target-*` flags apply only to `run cmd` (kind=command) and
// `run push` — a scenario run targets an incarnation, not a host; passing
// `--target-*` to scenario is an error (a clear signal the wrong sub-command
// was picked).
func newRunScenarioCmd() *cobra.Command {
	var (
		incarnation string
		inputJSON   string
		batchSize   int
		batch       string
		maxFailures string
		concurrency int
		onFailure   string
		wait        bool
		waitTimeout time.Duration

		tflags targetFlags
	)
	c := &cobra.Command{
		Use:   "scenario <service>/<scenario>",
		Short: "batched scenario run over incarnations (Voyage kind=scenario)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, scenario, err := parseServiceScenario(args[0])
			if err != nil {
				return err
			}
			// target flags don't apply to the scenario path (only cmd/push):
			// a scenario run targets an incarnation, not a host.
			if target, _ := tflags.resolve(); target.hasAny() {
				return fmt.Errorf("--target-* flags don't apply to `run scenario` " +
					"(the target is an incarnation); use `run cmd` for ad-hoc " +
					"multi-target or `run push`")
			}

			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}

			incName := incarnation
			if incName == "" {
				detected, derr := autoDetectIncarnation(cmd.Context(), cl, svc)
				if derr != nil {
					return derr
				}
				incName = detected
			}

			var input map[string]any
			if inputJSON != "" {
				if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
					return fmt.Errorf("--input is not a JSON object: %w", err)
				}
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			reply, err := cl.Voyages.Create(ctx, client.VoyageCreateRequest{
				Kind:         "scenario",
				ScenarioName: scenario,
				Input:        input,
				Target:       client.VoyageTarget{Incarnations: []string{incName}},
				BatchSize:    batchSize,
				Batch:        batch,
				MaxFailures:  maxFailures,
				Concurrency:  concurrency,
				OnFailure:    onFailure,
			})
			if err != nil {
				return renderAPIError(err)
			}
			out := cmd.OutOrStdout()
			rf := RootFlags(cmd)
			if !wait {
				if rf.Output == output.FormatJSON {
					return output.JSON(out, reply)
				}
				fmt.Fprintf(out, "voyage_id:  %s\n", reply.VoyageID)
				fmt.Fprintf(out, "scope_size: %d\n", reply.ScopeSize)
				fmt.Fprintf(out, "status:     %s\n", reply.Status)
				fmt.Fprintf(out, "location:   %s\n", reply.Location)
				return nil
			}
			final, err := waitForVoyage(cmd.Context(), cl, reply.VoyageID, waitTimeout)
			if err != nil {
				return err
			}
			if rf.Output == output.FormatJSON {
				return output.JSON(out, final)
			}
			renderVoyageSnapshot(out, final)
			return nil
		},
	}
	c.Flags().StringVar(&incarnation, "incarnation", "",
		"incarnation name (if unset - auto-detect by service)")
	c.Flags().StringVar(&inputJSON, "input", "",
		"JSON object scenario-input (e.g. '{\"shards\":3}')")
	c.Flags().IntVar(&batchSize, "batch-size", 0,
		"Leg size (0/missing -> the whole run is one Leg)")
	c.Flags().StringVar(&batch, "batch", "",
		"Leg size in N|N% format (% of incarnation count); empty -> unset, Keeper parses it")
	c.Flags().StringVar(&maxFailures, "max-failures", "",
		"failure threshold N|N% (% of incarnation count); empty -> unset, Keeper parses it")
	c.Flags().IntVar(&concurrency, "concurrency", 0,
		"semaphore-cap fan-out (0/missing → default 50, max 500)")
	c.Flags().StringVar(&onFailure, "on-failure", "",
		"failure-policy: continue (default) or abort")
	c.Flags().BoolVar(&wait, "wait", false,
		"wait for the Voyage terminal state (poll GET /v1/voyages/{id})")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 10*time.Minute,
		"maximum wait time for --wait")
	tflags.bind(c)
	return c
}

// parseServiceScenario splits `<service>/<scenario>` into exactly two
// non-empty parts. An extra `/` (nested path) is an error: scenario cannot
// contain `/`.
func parseServiceScenario(raw string) (service, scenario string, err error) {
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("expected <service>/<scenario>, got %q", raw)
	}
	service = strings.TrimSpace(parts[0])
	scenario = strings.TrimSpace(parts[1])
	if service == "" || scenario == "" {
		return "", "", fmt.Errorf("service/scenario are empty in %q", raw)
	}
	if strings.Contains(scenario, "/") {
		return "", "", fmt.Errorf("scenario must not contain `/`: %q", scenario)
	}
	return service, scenario, nil
}

// autoDetectIncarnation returns the service's single incarnation; 0 or N is
// an error hinting to pass `--incarnation` explicitly.
func autoDetectIncarnation(ctx context.Context, cl *client.Client, service string) (string, error) {
	page, err := cl.Incarnations.List(ctx, client.IncarnationListOptions{Service: service, Limit: 100})
	if err != nil {
		return "", renderAPIError(err)
	}
	var names []string
	for _, it := range page.Items {
		names = append(names, it.Name)
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return "", fmt.Errorf("service %q: no incarnation; create one or pass --incarnation", service)
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("service %q has multiple incarnations (%s); pass --incarnation explicitly",
			service, strings.Join(names, ", "))
	}
}
