package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/output"
)

// newRunPushCmd — `soulctl run push <destiny@ref>` (C1). Тонкая обёртка над
// POST /v1/push/apply.
//
// Особенности target-флагов в push:
//   - inventory PushApply строго список SID-ов; coven/glob/regex/where сюда
//     приходят только через `--target-sids` (exact-match).
//   - coven/glob/regex/where для push НЕ поддерживаются backend-ом, ошибка
//     валидации на CLI до запроса (security: явно сказать оператору, что
//     push не делает dynamic-resolution; если нужно — клиент должен составить
//     список SID-ов сам, например через `soulctl souls list`).
func newRunPushCmd() *cobra.Command {
	var (
		sshProvider          string
		inputJSON            string
		cleanupStaleVersions bool

		tflags targetFlags
	)
	c := &cobra.Command{
		Use:   "push <destiny@ref>",
		Short: "push-применение destiny через SSH-провайдер",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			destiny := args[0]
			if destiny == "" {
				return errors.New("destiny пуст; ожидался формат <name>@<ref>")
			}
			target, err := tflags.resolve()
			if err != nil {
				return err
			}
			if err := validatePushTarget(target); err != nil {
				return err
			}

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

			reply, err := cl.Push.Apply(ctx, client.PushApplyRequest{
				Inventory:            target.SIDs,
				Destiny:              destiny,
				Input:                input,
				SSHProvider:          sshProvider,
				CleanupStaleVersions: cleanupStaleVersions,
			})
			if err != nil {
				return renderAPIError(err)
			}
			out := cmd.OutOrStdout()
			if RootFlags(cmd).Output == output.FormatJSON {
				return output.JSON(out, reply)
			}
			fmt.Fprintf(out, "apply_id: %s\n", reply.ApplyID)
			fmt.Fprintf(out, "destiny:  %s\n", destiny)
			fmt.Fprintf(out, "hosts:    %d\n", len(target.SIDs))
			return nil
		},
	}
	c.Flags().StringVar(&sshProvider, "ssh-provider", "",
		"имя SshProvider-плагина (pусто → server-default)")
	c.Flags().StringVar(&inputJSON, "input", "", "input destiny в JSON")
	c.Flags().BoolVar(&cleanupStaleVersions, "cleanup-stale-versions", false,
		"удалить устаревшие версии soul-бинаря/модулей в этой же SSH-сессии")
	tflags.bind(c)
	return c
}

// validatePushTarget — push-flow target ограничен `--target-sids` (см. ADR-032
// PushApplyRequest.inventory). Любой динамический селектор → явная ошибка с
// подсказкой, как получить inventory отдельной командой.
func validatePushTarget(t resolvedTarget) error {
	if len(t.SIDs) == 0 {
		return errors.New("push требует --target-sids <host1,host2,...> (inventory exact-match)")
	}
	if len(t.Coven) > 0 || t.Where != "" {
		return errors.New("push поддерживает только --target-sids; coven/glob/regex/where недоступны (используйте `soulctl souls list --coven=…` для получения inventory)")
	}
	return nil
}
