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

// newRunScenarioCmd — `soulctl run scenario <service>/<scenario>`. Батчевый
// scenario-прогон через Voyage `kind=scenario` (ADR-043, паритет с UI). Backend:
// POST /v1/voyages body `{kind: "scenario", scenario_name, target:{...}, ...}`.
// Резолв набора инкарнаций:
//   - явный `--incarnation` (одна цель);
//   - либо auto-detect по service (ровно одна incarnation на сервис).
//
// target-флаги (`--target-*`) применимы только к `run cmd` (kind=command) и
// `run push` — для scenario-прогона цель — инкарнация, не хост; передача
// `--target-*` к scenario — ошибка (явный сигнал, что выбран не тот sub-command).
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
		Short: "батчевый scenario-прогон над инкарнациями (Voyage kind=scenario)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, scenario, err := parseServiceScenario(args[0])
			if err != nil {
				return err
			}
			// target-флаги не применимы к scenario-пути (только cmd/push):
			// цель scenario-прогона — инкарнация, не хост.
			if target, _ := tflags.resolve(); target.hasAny() {
				return fmt.Errorf("--target-* флаги не применимы к `run scenario` " +
					"(цель — инкарнация); используйте `run cmd` для ad-hoc " +
					"multi-target или `run push`")
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
					return fmt.Errorf("--input не JSON-объект: %w", err)
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
		"имя incarnation (если не задано — auto-detect по service)")
	c.Flags().StringVar(&inputJSON, "input", "",
		"JSON-объект scenario-input (например '{\"shards\":3}')")
	c.Flags().IntVar(&batchSize, "batch-size", 0,
		"размер Leg (0/missing → весь прогон один Leg)")
	c.Flags().StringVar(&batch, "batch", "",
		"размер Leg в формате N|N% (% от числа инкарнаций); пусто → не задано, парсит Keeper")
	c.Flags().StringVar(&maxFailures, "max-failures", "",
		"порог провалов N|N% (% от числа инкарнаций); пусто → не задано, парсит Keeper")
	c.Flags().IntVar(&concurrency, "concurrency", 0,
		"semaphore-cap fan-out (0/missing → default 50, max 500)")
	c.Flags().StringVar(&onFailure, "on-failure", "",
		"failure-policy: continue (default) или abort")
	c.Flags().BoolVar(&wait, "wait", false,
		"ждать терминал Voyage (poll GET /v1/voyages/{id})")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 10*time.Minute,
		"максимальное время ожидания для --wait")
	tflags.bind(c)
	return c
}

// parseServiceScenario разбирает `<service>/<scenario>` строго на две непустые
// части. Лишний `/` (вложенный путь) — ошибка: scenario не может содержать `/`.
func parseServiceScenario(raw string) (service, scenario string, err error) {
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("ожидается <service>/<scenario>, получено %q", raw)
	}
	service = strings.TrimSpace(parts[0])
	scenario = strings.TrimSpace(parts[1])
	if service == "" || scenario == "" {
		return "", "", fmt.Errorf("service/scenario пусты в %q", raw)
	}
	if strings.Contains(scenario, "/") {
		return "", "", fmt.Errorf("scenario не должен содержать `/`: %q", scenario)
	}
	return service, scenario, nil
}

// autoDetectIncarnation возвращает единственную incarnation сервиса; 0 или N —
// ошибка с подсказкой указать `--incarnation` явно.
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
		return "", fmt.Errorf("сервиса %q: ни одной incarnation; создайте её или укажите --incarnation", service)
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("у сервиса %q несколько incarnation (%s); укажите --incarnation явно",
			service, strings.Join(names, ", "))
	}
}
