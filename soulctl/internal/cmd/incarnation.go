package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/output"
)

func newIncarnationCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "incarnation",
		Short: "операции над incarnation (runtime-инстансами сервисов)",
	}
	c.AddCommand(
		newIncarnationListCmd(),
		newIncarnationGetCmd(),
		newIncarnationRunCmd(),
		newIncarnationHistoryCmd(),
		newIncarnationCheckDriftCmd(),
	)
	return c
}

func newIncarnationListCmd() *cobra.Command {
	var (
		service string
		status  string
		coven   string
		limit   int
		offset  int
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "перечислить incarnation",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			reply, err := cl.Incarnations.List(ctx, client.IncarnationListOptions{
				Service: service, Status: status, Coven: coven,
				Limit: limit, Offset: offset,
			})
			if err != nil {
				return renderAPIError(err)
			}
			if RootFlags(cmd).Output == output.FormatJSON {
				return output.JSON(cmd.OutOrStdout(), reply)
			}
			rows := make([][]string, 0, len(reply.Items))
			for _, it := range reply.Items {
				rows = append(rows, []string{
					it.Name, it.Service, it.ServiceVersion, it.Status,
					output.JoinList(it.Covens), formatTimeShort(it.LastDriftCheckAt),
				})
			}
			return output.Table(cmd.OutOrStdout(),
				[]string{"NAME", "SERVICE", "VERSION", "STATUS", "COVENS", "LAST_DRIFT"},
				rows)
		},
	}
	c.Flags().StringVar(&service, "service", "", "фильтр по имени сервиса")
	c.Flags().StringVar(&status, "status", "", "фильтр по статусу (ready|applying|error_locked|...)")
	c.Flags().StringVar(&coven, "coven", "", "client-side фильтр по Coven-метке")
	c.Flags().IntVar(&limit, "limit", 0, "максимум записей (1..1000, default server 50)")
	c.Flags().IntVar(&offset, "offset", 0, "смещение пагинации")
	return c
}

func newIncarnationGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "показать incarnation по имени (spec/state/status/covens)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			item, err := cl.Incarnations.Get(ctx, args[0])
			if err != nil {
				return renderAPIError(err)
			}
			// get-команды печатают raw response: и в table, и в json. table-форма
			// для get осмысленна как «всё подряд» без рамок таблицы — JSON pretty.
			return output.JSON(cmd.OutOrStdout(), item)
		},
	}
}

func newIncarnationRunCmd() *cobra.Command {
	var (
		inputJSON   string
		dryRun      bool
		wait        bool
		waitTimeout time.Duration
	)
	c := &cobra.Command{
		Use:   "run <name> <scenario>",
		Short: "запустить scenario на incarnation",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			input, err := parseInputJSON(inputJSON)
			if err != nil {
				return err
			}

			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			reply, err := cl.Incarnations.Run(ctx, args[0], args[1], input, dryRun)
			if err != nil {
				return renderAPIError(err)
			}
			rf := RootFlags(cmd)
			out := cmd.OutOrStdout()
			if !wait {
				if rf.Output == output.FormatJSON {
					return output.JSON(out, reply)
				}
				fmt.Fprintf(out, "apply_id: %s\nincarnation: %s\nscenario: %s\n",
					reply.ApplyID, reply.Incarnation, reply.Scenario)
				return nil
			}
			result, err := waitForApply(cmd.Context(), cl, args[0], reply.ApplyID, waitTimeout)
			if err != nil {
				return renderAPIError(err)
			}
			if rf.Output == output.FormatJSON {
				return output.JSON(out, result)
			}
			fmt.Fprintf(out, "apply_id:    %s\n", reply.ApplyID)
			fmt.Fprintf(out, "incarnation: %s\n", reply.Incarnation)
			fmt.Fprintf(out, "scenario:    %s\n", reply.Scenario)
			fmt.Fprintf(out, "status:      %s\n", result.FinalStatus)
			if result.HistoryEntry != nil {
				fmt.Fprintf(out, "history_id:  %s\n", result.HistoryEntry.HistoryID)
				fmt.Fprintf(out, "completed:   %s\n", result.HistoryEntry.CreatedAt)
			}
			return nil
		},
	}
	c.Flags().StringVar(&inputJSON, "input", "", "input scenario в JSON (например '{\"shards\":3}')")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "запустить scenario в режиме dry-run")
	c.Flags().BoolVar(&wait, "wait", false, "ждать завершения apply (poll status + history)")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 5*time.Minute, "максимальное время ожидания для --wait")
	return c
}

func newIncarnationHistoryCmd() *cobra.Command {
	var (
		limit  int
		offset int
	)
	c := &cobra.Command{
		Use:   "history <name>",
		Short: "история state_history incarnation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			reply, err := cl.Incarnations.History(ctx, args[0], limit, offset)
			if err != nil {
				return renderAPIError(err)
			}
			if RootFlags(cmd).Output == output.FormatJSON {
				return output.JSON(cmd.OutOrStdout(), reply)
			}
			rows := make([][]string, 0, len(reply.Items))
			for _, h := range reply.Items {
				rows = append(rows, []string{
					h.ApplyID, h.Scenario, "", "", h.ChangedByAID, h.CreatedAt,
				})
			}
			// STATUS/DURATION у state_history нет (запись появляется только при
			// успешном коммите), оставлены пустые ячейки для симметрии заголовков
			// из ТЗ.
			return output.Table(cmd.OutOrStdout(),
				[]string{"APPLY_ID", "SCENARIO", "STATUS", "DURATION", "STARTED_BY", "STARTED_AT"},
				rows)
		},
	}
	c.Flags().IntVar(&limit, "limit", 0, "максимум записей (1..1000, default server 50)")
	c.Flags().IntVar(&offset, "offset", 0, "смещение пагинации")
	return c
}

func newIncarnationCheckDriftCmd() *cobra.Command {
	var inputJSON string
	c := &cobra.Command{
		Use:   "check-drift <name>",
		Short: "Scry-проверка drift incarnation (ADR-031)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input, err := parseInputJSON(inputJSON)
			if err != nil {
				return err
			}
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			// Drift-проверка sync, но Soul-side обходит весь scenario с mod.Plan —
			// 30s мало; ставим 5 минут.
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			report, err := cl.Incarnations.CheckDrift(ctx, args[0], input)
			if err != nil {
				return renderAPIError(err)
			}
			if RootFlags(cmd).Output == output.FormatJSON {
				return output.JSON(cmd.OutOrStdout(), report)
			}
			return printDriftReport(cmd, report)
		},
	}
	c.Flags().StringVar(&inputJSON, "input", "", "override converge-input в JSON")
	return c
}

func printDriftReport(cmd *cobra.Command, r *client.DriftReport) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "incarnation: %s\n", r.Incarnation)
	fmt.Fprintf(out, "scenario:    %s\n", r.ScenarioRef)
	fmt.Fprintf(out, "checked_at:  %s\n", r.CheckedAt)
	fmt.Fprintf(out, "summary:     drifted=%d clean=%d unsupported=%d failed=%d\n",
		r.Summary.HostsDrifted, r.Summary.HostsClean,
		r.Summary.HostsUnsupported, r.Summary.HostsFailed)
	fmt.Fprintln(out)
	rows := make([][]string, 0, len(r.Hosts))
	for _, h := range r.Hosts {
		driftedTasks := 0
		for _, t := range h.Tasks {
			if t.Changed {
				driftedTasks++
			}
		}
		rows = append(rows, []string{
			h.SID, h.Status,
			fmt.Sprintf("%d/%d", driftedTasks, len(h.Tasks)),
		})
	}
	return output.Table(out, []string{"SID", "STATUS", "TASKS_DRIFTED"}, rows)
}

// parseInputJSON — нормализация флага --input. Пустая строка → nil input.
func parseInputJSON(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("--input: не JSON-объект: %w", err)
	}
	return out, nil
}

// formatTimeShort обрезает RFC3339 до YYYY-MM-DD HH:MM (UTC). Пустая строка
// остаётся пустой, чтобы output.Table заменила её на <none>.
func formatTimeShort(rfc3339 string) string {
	if rfc3339 == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// waitResult — итог waitForApply: финальный status incarnation + запись из
// history (если её удалось дождаться).
type waitResult struct {
	ApplyID      string                    `json:"apply_id"`
	FinalStatus  string                    `json:"final_status"`
	HistoryEntry *client.StateHistoryEntry `json:"history_entry,omitempty"`
}

// waitForApply — poll-цикл по openapi MVP-контракту:
//   - /v1/incarnations/{name}/history — запись с apply_id появится после успешного commit.
//   - /v1/incarnations/{name}        — текущий status (applying → ready / error_locked / migration_failed).
//
// Стоп-условия:
//   - history содержит запись с apply_id (успех — final_status = текущий status incarnation);
//   - status стал блокирующим (error_locked / migration_failed / destroy_failed);
//   - превышен waitTimeout (возврат осмысленной ошибки).
//
// Отдельного /v1/applies/{apply_id} в MVP нет (operator-api.md → Async operations).
func waitForApply(parent context.Context, cl *client.Client, name, applyID string, timeout time.Duration) (*waitResult, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := signalContext(parent)
	defer cancel()
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("ожидание apply прервано: %w", err)
		}
		// 1. history — самый авторитетный сигнал успеха.
		hist, err := cl.Incarnations.History(ctx, name, 50, 0)
		if err != nil {
			return nil, err
		}
		for i := range hist.Items {
			if hist.Items[i].ApplyID == applyID {
				current, gerr := cl.Incarnations.Get(ctx, name)
				if gerr != nil {
					return nil, gerr
				}
				return &waitResult{
					ApplyID:      applyID,
					FinalStatus:  current.Status,
					HistoryEntry: &hist.Items[i],
				}, nil
			}
		}
		// 2. status incarnation — fail-fast при блокирующем статусе.
		current, err := cl.Incarnations.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if isBlockingStatus(current.Status) {
			return &waitResult{ApplyID: applyID, FinalStatus: current.Status},
				fmt.Errorf("apply %s завершился со статусом %s", applyID, current.Status)
		}
		if time.Now().After(deadline) {
			return &waitResult{ApplyID: applyID, FinalStatus: current.Status},
				errors.New("ожидание apply превысило wait-timeout (статус всё ещё " + current.Status + ")")
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

func isBlockingStatus(s string) bool {
	switch s {
	case "error_locked", "migration_failed", "destroy_failed":
		return true
	}
	return false
}
