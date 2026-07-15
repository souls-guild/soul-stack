package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/output"
)

func newSoulsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "souls",
		Short: "операции над Souls (управляемыми агентами)",
	}
	c.AddCommand(newSoulsListCmd(), newSoulsGetCmd(), newSoulsSshTargetCmd())
	return c
}

// newSoulsSshTargetCmd — `soulctl souls ssh-target set <sid> --port … --user …
// --soul-path … [--ssh-provider <name>]` ↔ PUT /v1/souls/{sid}/ssh-target
// (ADR-032 amendment 2026-05-26 S7-1 + amendment 2026-05-27 P2 W-1). Plus
// `bulk-set --coven <name> --ssh-provider <name>` — bulk PATCH of SIDs in a
// given Coven.
func newSoulsSshTargetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ssh-target",
		Short: "управление per-host SSH-реквизитами push-flow (souls.ssh_target)",
	}
	c.AddCommand(newSoulsSshTargetSetCmd(), newSoulsSshTargetBulkSetCmd())
	return c
}

func newSoulsSshTargetSetCmd() *cobra.Command {
	var (
		port        int
		user        string
		soulPath    string
		sshProvider string
	)
	c := &cobra.Command{
		Use:   "set <sid>",
		Short: "задать per-host ssh_target (ssh_port/ssh_user/soul_path[/ssh_provider])",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sid := args[0]
			if port < 1 || port > 65535 {
				return fmt.Errorf("--port обязателен и должен быть в [1..65535]")
			}
			if user == "" {
				return fmt.Errorf("--user обязателен")
			}
			if soulPath == "" || soulPath[0] != '/' {
				return fmt.Errorf("--soul-path обязателен и должен быть абсолютным Unix-путём")
			}
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			reply, err := cl.Souls.SetSshTarget(ctx, sid, client.SoulSshTargetBody{
				SSHPort:     port,
				SSHUser:     user,
				SoulPath:    soulPath,
				SSHProvider: sshProvider,
			})
			if err != nil {
				return renderAPIError(err)
			}
			return output.JSON(cmd.OutOrStdout(), reply)
		},
	}
	c.Flags().IntVar(&port, "port", 22, "SSH-порт (1..65535)")
	c.Flags().StringVar(&user, "user", "root", "SSH-пользователь")
	c.Flags().StringVar(&soulPath, "soul-path", "/usr/local/bin/soul", "абсолютный путь к soul-бинарю на хосте")
	c.Flags().StringVar(&sshProvider, "ssh-provider", "", "per-SID explicit SshProvider plugin name (Level 1 routing); пусто → routing по coven/cluster-default")
	return c
}

// newSoulsSshTargetBulkSetCmd — `soulctl souls ssh-target bulk-set --coven=Z
// --ssh-provider=Y` (P2 W-4): bulk-PATCHes ssh_provider for every Soul in the
// given Coven.
//
// Client-side implementation: list-by-coven → per-SID PUT (there's no
// server-side bulk endpoint for ssh-target; adding a dedicated route just for
// this CLI command would be overkill). With many Souls, the operator sees a
// success/fail summary without abort-on-first — one SID failing doesn't stop
// the rest.
func newSoulsSshTargetBulkSetCmd() *cobra.Command {
	var (
		coven       string
		port        int
		user        string
		soulPath    string
		sshProvider string
	)
	c := &cobra.Command{
		Use:   "bulk-set",
		Short: "массово задать ssh_provider всем Souls в Coven (P2 W-4)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if coven == "" {
				return fmt.Errorf("--coven обязателен")
			}
			if sshProvider == "" {
				return fmt.Errorf("--ssh-provider обязателен (bulk-set настраивает именно provider)")
			}
			if port < 1 || port > 65535 {
				return fmt.Errorf("--port обязателен и должен быть в [1..65535]")
			}
			if user == "" {
				return fmt.Errorf("--user обязателен")
			}
			if soulPath == "" || soulPath[0] != '/' {
				return fmt.Errorf("--soul-path обязателен и должен быть абсолютным Unix-путём")
			}
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			listReply, err := cl.Souls.List(ctx, client.SoulListOptions{
				Covens: []string{coven},
				Limit:  1000,
			})
			if err != nil {
				return renderAPIError(err)
			}
			body := client.SoulSshTargetBody{
				SSHPort:     port,
				SSHUser:     user,
				SoulPath:    soulPath,
				SSHProvider: sshProvider,
			}
			type bulkRes struct {
				SID    string `json:"sid"`
				Status string `json:"status"`
				Error  string `json:"error,omitempty"`
			}
			results := make([]bulkRes, 0, len(listReply.Items))
			ok, fail := 0, 0
			for _, it := range listReply.Items {
				_, perr := cl.Souls.SetSshTarget(ctx, it.SID, body)
				if perr != nil {
					results = append(results, bulkRes{SID: it.SID, Status: "error", Error: perr.Error()})
					fail++
					continue
				}
				results = append(results, bulkRes{SID: it.SID, Status: "updated"})
				ok++
			}
			return output.JSON(cmd.OutOrStdout(), map[string]any{
				"coven":         coven,
				"ssh_provider":  sshProvider,
				"total":         len(listReply.Items),
				"updated_count": ok,
				"failed_count":  fail,
				"results":       results,
			})
		},
	}
	c.Flags().StringVar(&coven, "coven", "", "Coven-метка (обязательно)")
	c.Flags().StringVar(&sshProvider, "ssh-provider", "", "имя SshProvider-плагина (обязательно)")
	c.Flags().IntVar(&port, "port", 22, "SSH-порт (1..65535)")
	c.Flags().StringVar(&user, "user", "root", "SSH-пользователь")
	c.Flags().StringVar(&soulPath, "soul-path", "/usr/local/bin/soul", "абсолютный путь к soul-бинарю на хосте")
	return c
}

// newSoulCmd — the singular root for single-target operations (exec, etc.).
// `soulctl soul exec <sid> ...` ↔ `POST /v1/souls/{sid}/exec` (ADR-033).
// Deliberately separate from `souls` (plural) — that one is about the
// registry (list/get across hosts); `soul` is about acting on one.
func newSoulCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "soul",
		Short: "одиночные действия на конкретном Soul (exec)",
	}
	c.AddCommand(newSoulExecCmd())
	return c
}

// newSoulExecCmd — `soulctl soul exec <sid> --module … --input …` ↔
// POST /v1/souls/{sid}/exec (Errand, ADR-033). The module whitelist and the
// stdout/stderr cap (64 KiB) are enforced by the Soul-side errand-runner.
func newSoulExecCmd() *cobra.Command {
	var (
		module  string
		input   string
		timeout int
		dryRun  bool
		poll    bool
	)
	c := &cobra.Command{
		Use:   "exec <sid>",
		Short: "запустить одиночный модуль на Soul (Errand, ADR-033)",
		Long: `Pull-ad-hoc exec одиночного модуля на Soul через mTLS EventStream.
Whitelist на Soul-side: core.cmd.shell, core.exec.run + read-safe модули.

Examples:
  soulctl soul exec web-01.example.com --module core.cmd.shell --input '{"command":"uptime"}'
  soulctl soul exec web-01.example.com --module core.exec.run --input '{"argv":["uname","-a"]}' --timeout 60
  soulctl soul exec web-01.example.com --module core.http.probe --input '{"url":"http://localhost:8080/health"}' --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sid := args[0]
			if module == "" {
				return fmt.Errorf("--module обязателен")
			}
			var inputObj map[string]any
			if input != "" && input != "{}" {
				if err := json.Unmarshal([]byte(input), &inputObj); err != nil {
					return fmt.Errorf("--input: невалидный JSON: %w", err)
				}
			}

			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			// The overall ctx timeout shall be greater than the server cap (30s)
			// and greater than the requested timeout — otherwise the CLI would
			// bail out before Keeper returns 202 on async escalation.
			ctxTimeout := time.Duration(timeout)*time.Second + 60*time.Second
			ctx, cancel := context.WithTimeout(cmd.Context(), ctxTimeout)
			defer cancel()

			res, async, err := cl.Errand.Exec(ctx, client.ErrandExecRequest{
				SID:            sid,
				Module:         module,
				Input:          inputObj,
				TimeoutSeconds: timeout,
				DryRun:         dryRun,
			})
			if err != nil {
				return renderAPIError(err)
			}

			if async && poll {
				res, err = pollErrandToTerminal(ctx, cl, res.ErrandID, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				async = false
			}

			if RootFlags(cmd).Output == output.FormatJSON {
				return output.JSON(cmd.OutOrStdout(), res)
			}

			if async {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Errand %s принят асинхронно (server-cap превышен); poll: soulctl errand get %s\n",
					res.ErrandID, res.ErrandID)
				return nil
			}
			return renderErrandResult(cmd.OutOrStdout(), res)
		},
	}
	c.Flags().StringVar(&module, "module", "", "адрес модуля (core.cmd.shell / core.exec.run / core.<class>.<state>); обязателен")
	c.Flags().StringVar(&input, "input", "{}", "input JSON-объект модуля")
	c.Flags().IntVar(&timeout, "timeout", 30, "timeout в секундах (1..300)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "Plan вместо Apply (только read-safe модули)")
	c.Flags().BoolVar(&poll, "poll", true, "дожимать async-результат через keeper.errand.get до терминала")
	return c
}

func newSoulsListCmd() *cobra.Command {
	var (
		covens    []string
		status    string
		transport string
		limit     int
		offset    int
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "перечислить зарегистрированные Souls",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			reply, err := cl.Souls.List(ctx, client.SoulListOptions{
				Covens: covens, Status: status, Transport: transport,
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
					it.SID, it.Status, it.Transport,
					output.JoinList(it.Covens), formatTimeShort(it.LastSeenAt),
				})
			}
			return output.Table(cmd.OutOrStdout(),
				[]string{"SID", "STATUS", "TRANSPORT", "COVENS", "LAST_SEEN"},
				rows)
		},
	}
	c.Flags().StringSliceVar(&covens, "coven", nil, "фильтр по Coven-метке (можно повторять)")
	c.Flags().StringVar(&status, "status", "", "фильтр по статусу (pending|connected|disconnected|expired)")
	c.Flags().StringVar(&transport, "transport", "", "фильтр по транспорту (agent|ssh)")
	c.Flags().IntVar(&limit, "limit", 0, "максимум записей (1..1000, default server 50)")
	c.Flags().IntVar(&offset, "offset", 0, "смещение пагинации")
	return c
}

func newSoulsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <sid>",
		Short: "показать Soul по SID (фоллбэк через list — soul.get не выставлен в MVP)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			item, err := cl.Souls.Get(ctx, args[0])
			if err != nil {
				return renderAPIError(err)
			}
			return output.JSON(cmd.OutOrStdout(), item)
		},
	}
}
