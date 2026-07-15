package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/output"
)

// newErrandCmd is the command root for the Errand registry (ADR-033):
// `soulctl errand list` / `get <errand_id>` (read) + `cancel <errand_id>`
// (slice E5). Running an Errand is `soulctl soul exec <sid> …`
// (singular `soul`, see souls.go).
func newErrandCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "errand",
		Short: "реестр Errand-ов (ADR-033): list / get / cancel",
	}
	c.AddCommand(newErrandListCmd(), newErrandGetCmd(), newErrandCancelCmd())
	return c
}

// newErrandCancelCmd — `soulctl errand cancel <errand_id>` (ADR-033 slice E5).
// Permission: errand.cancel. Returns success on 204; the final status
// (cancelled/success/failed) is visible via `soulctl errand get <id>`.
func newErrandCancelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cancel <errand_id>",
		Short: "отменить in-flight Errand",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			if err := cl.Errand.Cancel(ctx, args[0]); err != nil {
				return renderAPIError(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cancel signal sent to %s\n", args[0])
			return nil
		},
	}
	return c
}

func newErrandListCmd() *cobra.Command {
	var (
		sid          string
		status       string
		startedAfter string
		limit        int
		offset       int
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "перечислить Errand-ы с фильтрами",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			reply, err := cl.Errand.List(ctx, client.ErrandListOptions{
				SID: sid, Status: status, StartedAfter: startedAfter,
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
					it.ErrandID, it.SID, it.Module, it.Status,
					exitCodeShort(it.ExitCode), durationShort(it.DurationMs),
					formatTimeShort(it.StartedAt),
				})
			}
			return output.Table(cmd.OutOrStdout(),
				[]string{"ERRAND_ID", "SID", "MODULE", "STATUS", "EXIT", "DURATION", "STARTED_AT"},
				rows)
		},
	}
	c.Flags().StringVar(&sid, "sid", "", "фильтр по SID")
	c.Flags().StringVar(&status, "status", "", "фильтр по статусу (running|success|failed|timed_out|cancelled|module_not_allowed)")
	c.Flags().StringVar(&startedAfter, "started-after", "", "фильтр по started_at > <RFC3339>")
	c.Flags().IntVar(&limit, "limit", 0, "максимум записей (1..1000, default 50)")
	c.Flags().IntVar(&offset, "offset", 0, "смещение пагинации")
	return c
}

func newErrandGetCmd() *cobra.Command {
	var poll bool
	c := &cobra.Command{
		Use:   "get <errand_id>",
		Short: "показать состояние Errand-а",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			res, async, err := cl.Errand.Get(ctx, args[0])
			if err != nil {
				return renderAPIError(err)
			}
			if async && poll {
				res, err = pollErrandToTerminal(ctx, cl, res.ErrandID, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
			}
			if RootFlags(cmd).Output == output.FormatJSON {
				return output.JSON(cmd.OutOrStdout(), res)
			}
			return renderErrandResult(cmd.OutOrStdout(), res)
		},
	}
	c.Flags().BoolVar(&poll, "poll", false, "дожимать running-Errand до терминала (poll loop)")
	return c
}

// pollErrandToTerminal issues periodic GET /v1/errands/{errand_id} calls
// until a terminal state. Backoff is linear 1s..5s (small window: an Errand
// typically terminates within <1m). Errors are propagated as-is; ctx-Done
// exits immediately.
func pollErrandToTerminal(ctx context.Context, cl *client.Client, errandID string, stderrW io.Writer) (client.ErrandResult, error) {
	delay := time.Second
	const maxDelay = 5 * time.Second
	fmt.Fprintf(stderrW, "polling errand %s ...\n", errandID)
	for {
		select {
		case <-ctx.Done():
			return client.ErrandResult{}, ctx.Err()
		case <-time.After(delay):
		}
		res, async, err := cl.Errand.Get(ctx, errandID)
		if err != nil {
			// A 404 right after our own Exec is impossible (we just inserted
			// the row); 5xx is propagated so the operator sees the real error.
			return client.ErrandResult{}, renderAPIError(err)
		}
		if !async {
			return res, nil
		}
		if delay < maxDelay {
			delay += time.Second
		}
	}
}

// renderErrandResult prints a table-style view of a single Errand: status,
// exit, duration, stdout/stderr with a truncation marker. For JSON mode the
// caller uses output.JSON directly (renderErrandResult isn't called).
func renderErrandResult(w io.Writer, res client.ErrandResult) error {
	if _, err := fmt.Fprintf(w, "Errand:   %s\n", res.ErrandID); err != nil {
		return err
	}
	fmt.Fprintf(w, "SID:      %s\n", res.SID)
	fmt.Fprintf(w, "Module:   %s\n", res.Module)
	fmt.Fprintf(w, "Status:   %s\n", res.Status)
	if res.ExitCode != nil {
		fmt.Fprintf(w, "Exit:     %d\n", *res.ExitCode)
	}
	if res.DurationMs != nil {
		fmt.Fprintf(w, "Duration: %dms\n", *res.DurationMs)
	}
	if res.StartedAt != "" {
		fmt.Fprintf(w, "Started:  %s\n", res.StartedAt)
	}
	if res.FinishedAt != "" {
		fmt.Fprintf(w, "Finished: %s\n", res.FinishedAt)
	}
	if res.ErrorMessage != "" {
		fmt.Fprintf(w, "Error:    %s\n", res.ErrorMessage)
	}
	if res.Stdout != "" {
		fmt.Fprintln(w, "\n--- stdout ---")
		fmt.Fprint(w, res.Stdout)
		if !endsWithNewline(res.Stdout) {
			fmt.Fprintln(w)
		}
		if res.StdoutTruncated {
			fmt.Fprintln(w, "[truncated]")
		}
	}
	if res.Stderr != "" {
		fmt.Fprintln(w, "\n--- stderr ---")
		fmt.Fprint(w, res.Stderr)
		if !endsWithNewline(res.Stderr) {
			fmt.Fprintln(w)
		}
		if res.StderrTruncated {
			fmt.Fprintln(w, "[truncated]")
		}
	}
	if len(res.Output) > 0 {
		fmt.Fprintln(w, "\n--- output (structured) ---")
		if err := output.JSON(w, res.Output); err != nil {
			return errors.New("render output: " + err.Error())
		}
	}
	return nil
}

func endsWithNewline(s string) bool {
	return len(s) > 0 && s[len(s)-1] == '\n'
}

// exitCodeShort is a table helper: nil → "-", otherwise itoa.
func exitCodeShort(code *int32) string {
	if code == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *code)
}

// durationShort is a table helper: nil → "-", otherwise "<N>ms".
func durationShort(ms *int64) string {
	if ms == nil {
		return "-"
	}
	return fmt.Sprintf("%dms", *ms)
}
