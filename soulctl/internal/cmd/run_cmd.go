package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/output"
)

// newRunCmdCmd — `soulctl run cmd '<command>'`. An ad-hoc shell command on N
// hosts via Voyage `kind=command` (ADR-043). Backend: POST /v1/voyages body
// `{kind: "command", module: "core.cmd.shell", input: {cmd: "<command>"}, target, ...}`.
//
// Behaviour:
//   - target is required (no point running without a scope).
//   - 202 → prints voyage_id + scope_size + location.
//   - `--wait` → polls GET /v1/voyages/{id} every 3s until terminal.
func newRunCmdCmd() *cobra.Command {
	var (
		module      string
		concurrency int
		onFailure   string
		batchSize   int
		batch       string
		maxFailures string
		wait        bool
		waitTimeout time.Duration

		tflags targetFlags
	)
	c := &cobra.Command{
		Use:   "cmd <command>",
		Short: "ad-hoc выполнение shell-команды на N хостов (Voyage kind=command)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			shellCmd := args[0]
			if strings.TrimSpace(shellCmd) == "" {
				return fmt.Errorf("команда пуста")
			}
			target, err := tflags.resolve()
			if err != nil {
				return err
			}
			if err := target.require(); err != nil {
				return err
			}

			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			req := client.VoyageCreateRequest{
				Kind:   "command",
				Module: module,
				Input:  map[string]any{"cmd": shellCmd},
				Target: client.VoyageTarget{
					SIDs:  target.SIDs,
					Coven: target.Coven,
					Where: target.Where,
				},
				OnFailure:   onFailure,
				Concurrency: concurrency,
				BatchSize:   batchSize,
				Batch:       batch,
				MaxFailures: maxFailures,
			}
			reply, err := cl.Voyages.Create(ctx, req)
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
	c.Flags().StringVar(&module, "module", "core.cmd.shell",
		"command-модуль (whitelist Soul-side: core.cmd.shell / core.exec.run)")
	c.Flags().IntVar(&concurrency, "concurrency", 0,
		"semaphore-cap fan-out (0/missing → default 50, max 500)")
	c.Flags().StringVar(&onFailure, "on-failure", "",
		"failure-policy: continue (default) или abort")
	c.Flags().IntVar(&batchSize, "batch-size", 0,
		"размер Leg (0/missing → весь прогон один Leg)")
	c.Flags().StringVar(&batch, "batch", "",
		"размер Leg в формате N|N% (% от числа хостов); пусто → не задано, парсит Keeper")
	c.Flags().StringVar(&maxFailures, "max-failures", "",
		"порог провалов N|N% (% от числа хостов); пусто → не задано, парсит Keeper")
	c.Flags().BoolVar(&wait, "wait", false,
		"ждать терминал Voyage (poll GET /v1/voyages/{id})")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 10*time.Minute,
		"максимальное время ожидания для --wait")
	tflags.bind(c)
	return c
}

// waitForVoyage — polls every 3s until terminal (succeeded/failed/
// partial_failed/cancelled). Used only when --wait is set.
func waitForVoyage(parent context.Context, cl *client.Client, voyageID string, timeout time.Duration) (*client.Voyage, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := signalContext(parent)
	defer cancel()
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("ожидание voyage прервано: %w", err)
		}
		snap, err := cl.Voyages.Get(ctx, voyageID)
		if err != nil {
			return nil, renderAPIError(err)
		}
		if isVoyageTerminal(snap.Status) {
			return snap, nil
		}
		if time.Now().After(deadline) {
			return snap, fmt.Errorf("wait-timeout: voyage %s ещё в %s",
				voyageID, snap.Status)
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

func isVoyageTerminal(s string) bool {
	switch s {
	case "succeeded", "failed", "partial_failed", "cancelled":
		return true
	}
	return false
}

func renderVoyageSnapshot(w io.Writer, r *client.Voyage) {
	fmt.Fprintf(w, "voyage_id:    %s\n", r.VoyageID)
	fmt.Fprintf(w, "kind:         %s\n", r.Kind)
	fmt.Fprintf(w, "status:       %s\n", r.Status)
	fmt.Fprintf(w, "scope_size:   %d\n", r.ScopeSize)
	fmt.Fprintf(w, "current_done: %d\n", r.CurrentDone)
	if r.FinishedAt != "" {
		fmt.Fprintf(w, "finished_at:  %s\n", r.FinishedAt)
	}
	if r.Summary != nil {
		fmt.Fprintf(w, "summary:      total=%d succeeded=%d failed=%d cancelled=%d\n",
			r.Summary.Total, r.Summary.Succeeded, r.Summary.Failed, r.Summary.Cancelled)
	}
}
