package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/output"
)

func newIncarnationCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "incarnation",
		Short: "operations on incarnations (runtime instances of services)",
	}
	c.AddCommand(
		newIncarnationListCmd(),
		newIncarnationGetCmd(),
		newIncarnationRunCmd(),
		newIncarnationHistoryCmd(),
		newIncarnationRunsCmd(),
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
		Short: "list incarnations",
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
					it.ID, it.Service, it.ServiceVersion, it.Status,
					output.JoinList(it.Covens),
				})
			}
			return output.Table(cmd.OutOrStdout(),
				[]string{"ID", "SERVICE", "VERSION", "STATUS", "COVENS"},
				rows)
		},
	}
	c.Flags().StringVar(&service, "service", "", "filter by service name")
	c.Flags().StringVar(&status, "status", "", "filter by status (ready|applying|error_locked|...)")
	c.Flags().StringVar(&coven, "coven", "", "client-side filter by Coven label")
	c.Flags().IntVar(&limit, "limit", 0, "maximum records (1..1000, default server 50)")
	c.Flags().IntVar(&offset, "offset", 0, "pagination offset")
	return c
}

func newIncarnationGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "show an incarnation by name (spec/state/status/covens)",
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
			// get commands print the raw response in both table and json mode.
			// For get, "table" mode makes sense as a flat dump with no table
			// framing — JSON pretty-printed.
			return output.JSON(cmd.OutOrStdout(), item)
		},
	}
}

func newIncarnationRunCmd() *cobra.Command {
	var (
		inputJSON   string
		wait        bool
		waitTimeout time.Duration
	)
	c := &cobra.Command{
		Use:   "run <name> <scenario>",
		Short: "run a scenario on an incarnation",
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

			reply, err := cl.Incarnations.Run(ctx, args[0], args[1], input)
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
	c.Flags().StringVar(&inputJSON, "input", "", "scenario input as JSON (e.g. '{\"shards\":3}')")
	c.Flags().BoolVar(&wait, "wait", false, "wait for apply to finish (poll status + history)")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 5*time.Minute, "maximum wait time for --wait")
	return c
}

func newIncarnationHistoryCmd() *cobra.Command {
	var (
		limit  int
		offset int
	)
	c := &cobra.Command{
		Use:   "history <name>",
		Short: "incarnation state_history",
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
			// state_history has no STATUS/DURATION (a record only appears
			// after a successful commit); left as empty cells to keep the
			// header columns symmetric with the spec.
			return output.Table(cmd.OutOrStdout(),
				[]string{"APPLY_ID", "SCENARIO", "STATUS", "DURATION", "STARTED_BY", "STARTED_AT"},
				rows)
		},
	}
	c.Flags().IntVar(&limit, "limit", 0, "maximum records (1..1000, default server 50)")
	c.Flags().IntVar(&offset, "offset", 0, "pagination offset")
	return c
}

// newIncarnationRunsCmd — `runs <name> [apply_id]`: the applies of an
// incarnation, and one apply in detail. Distinct from `history`, which lists
// state_history — what the state BECAME — and therefore carries no run status
// and no failure reason: a run that never started shows there with an empty
// status, while here it reads `failed` with `no_hosts` against the host row.
//
// One command with an optional second argument rather than two: list and detail
// are the same object at two zoom levels, and `runs <name> <apply_id>` reads as
// the drill-down it is. `run` (singular) was not available anyway — it starts a
// scenario.
func newIncarnationRunsCmd() *cobra.Command {
	var (
		limit  int
		offset int
	)
	c := &cobra.Command{
		Use:   "runs <name> [apply_id]",
		Short: "apply runs of an incarnation (add apply_id for per-host detail)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			if len(args) == 2 {
				detail, err := cl.Incarnations.RunDetail(ctx, args[0], args[1])
				if err != nil {
					return renderAPIError(err)
				}
				if RootFlags(cmd).Output == output.FormatJSON {
					return output.JSON(cmd.OutOrStdout(), detail)
				}
				return printRunDetail(cmd, detail)
			}

			reply, err := cl.Incarnations.Runs(ctx, args[0], limit, offset)
			if err != nil {
				return renderAPIError(err)
			}
			if RootFlags(cmd).Output == output.FormatJSON {
				return output.JSON(cmd.OutOrStdout(), reply)
			}
			rows := make([][]string, 0, len(reply.Items))
			for _, r := range reply.Items {
				startedBy := ""
				if r.StartedByAID != nil {
					startedBy = *r.StartedByAID
				}
				finished := ""
				if r.FinishedAt != nil {
					finished = formatTimeShort(*r.FinishedAt)
				}
				rows = append(rows, []string{
					r.ApplyID, r.Scenario, r.Status,
					formatTimeShort(r.StartedAt), finished, startedBy,
				})
			}
			return output.Table(cmd.OutOrStdout(),
				[]string{"APPLY_ID", "SCENARIO", "STATUS", "STARTED_AT", "FINISHED_AT", "STARTED_BY"},
				rows)
		},
	}
	c.Flags().IntVar(&limit, "limit", 0, "maximum records (1..1000, default server 50)")
	c.Flags().IntVar(&offset, "offset", 0, "pagination offset")
	return c
}

// printRunDetail renders one run: header, per-host table, then the advisory
// notices (ADR-0076(u)).
//
// Notices are printed AFTER the table and not as a column, because they are the
// one thing here that is not about this run's outcome: the run succeeded, and a
// param it passed stops working in a future release. A column would be truncated
// to uselessness — the whole value is in the sentence, which names the deadline
// and the replacement — and would read as a per-host defect rather than as work
// to schedule.
//
// Grouped by host, because that is the granularity the data has: the contract a
// param is checked against is the manifest compiled into THAT agent, so a park
// mid-upgrade legitimately answers differently host to host. Collapsing them
// would hide exactly how far the agent rollout has reached.
func printRunDetail(cmd *cobra.Command, d *client.RunDetail) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "apply_id:   %s\n", d.ApplyID)
	fmt.Fprintf(out, "scenario:   %s\n", d.Scenario)
	fmt.Fprintf(out, "status:     %s\n", d.Status)
	fmt.Fprintf(out, "started_at: %s\n", formatTimeShort(d.StartedAt))
	if d.FinishedAt != nil {
		fmt.Fprintf(out, "finished:   %s\n", formatTimeShort(*d.FinishedAt))
	}
	if d.StartedByAID != nil {
		fmt.Fprintf(out, "started_by: %s\n", *d.StartedByAID)
	}
	fmt.Fprintln(out)

	rows := make([][]string, 0, len(d.Hosts))
	for _, h := range d.Hosts {
		errSummary := ""
		if h.ErrorSummary != nil {
			errSummary = *h.ErrorSummary
		}
		rows = append(rows, []string{
			h.SID, h.Status, strconv.Itoa(h.Passage), errSummary,
		})
	}
	if err := output.Table(out, []string{"SID", "STATUS", "PASSAGE", "ERROR"}, rows); err != nil {
		return err
	}

	printRunNotices(out, d.Hosts)
	return nil
}

// printRunNotices writes the deprecation block, or nothing at all when no host
// reported anything — a quiet run must look exactly as it did before this
// existed.
func printRunNotices(out io.Writer, hosts []client.RunHostStatus) {
	any := false
	for _, h := range hosts {
		if len(h.Notices) > 0 {
			any = true
			break
		}
	}
	if !any {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "notices (the run succeeded; these stop working in a future release):")
	for _, h := range hosts {
		for _, n := range h.Notices {
			fmt.Fprintf(out, "  %s  %s: %s\n", h.SID, n.Module, n.Message)
		}
	}
}

// parseInputJSON normalizes the --input flag. Empty string → nil input.
func parseInputJSON(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("--input: not a JSON object: %w", err)
	}
	return out, nil
}

// formatTimeShort truncates RFC3339 to YYYY-MM-DD HH:MM (UTC). An empty
// string stays empty, so output.Table replaces it with <none>.
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

// waitResult is the outcome of waitForApply: the final incarnation status +
// the history entry (if one showed up in time).
type waitResult struct {
	ApplyID      string                    `json:"apply_id"`
	FinalStatus  string                    `json:"final_status"`
	HistoryEntry *client.StateHistoryEntry `json:"history_entry,omitempty"`
}

// waitForApply — poll loop per the openapi MVP contract:
//   - /v1/incarnations/{name}/history — a record with apply_id shows up after a successful commit.
//   - /v1/incarnations/{name}        — current status (applying → ready / error_locked / migration_failed).
//
// Stop conditions:
//   - history contains a record with apply_id (success — final_status = the current incarnation status);
//   - status becomes blocking (error_locked / migration_failed / destroy_failed);
//   - waitTimeout exceeded (returns a meaningful error).
//
// There's no separate /v1/applies/{apply_id} in the MVP (operator-api.md → Async operations).
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
			return nil, fmt.Errorf("waiting for apply was interrupted: %w", err)
		}
		// 1. history is the most authoritative success signal.
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
		// 2. incarnation status — fail-fast on a blocking status.
		current, err := cl.Incarnations.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if isBlockingStatus(current.Status) {
			return &waitResult{ApplyID: applyID, FinalStatus: current.Status},
				fmt.Errorf("apply %s finished with status %s", applyID, current.Status)
		}
		if time.Now().After(deadline) {
			return &waitResult{ApplyID: applyID, FinalStatus: current.Status},
				errors.New("waiting for apply exceeded wait-timeout (status is still " + current.Status + ")")
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
