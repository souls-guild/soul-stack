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

// newRunPushCmd — `soulctl run push <destiny@ref>` (C1). A thin wrapper over
// POST /v1/push/apply.
//
// Target-flag specifics for push:
//   - PushApply's inventory is strictly a list of SIDs; coven/glob/regex/where
//     can only reach it via `--target-sids` (exact-match).
//   - coven/glob/regex/where are NOT supported by the backend for push; the
//     CLI rejects them with a validation error before the request (security:
//     tell the operator explicitly that push does no dynamic resolution — if
//     needed, the client must build the SID list itself, e.g. via
//     `soulctl souls list`).
func newRunPushCmd() *cobra.Command {
	var (
		sshProvider          string
		inputJSON            string
		cleanupStaleVersions bool

		tflags targetFlags
	)
	c := &cobra.Command{
		Use:   "push <destiny@ref>",
		Short: "push-apply destiny via an SSH provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			destiny := args[0]
			if destiny == "" {
				return errors.New("destiny is empty; expected format <name>@<ref>")
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
		"SshProvider plugin name (empty -> server-default)")
	c.Flags().StringVar(&inputJSON, "input", "", "input destiny as JSON")
	c.Flags().BoolVar(&cleanupStaleVersions, "cleanup-stale-versions", false,
		"remove stale soul-binary/module versions within the same SSH session")
	tflags.bind(c)
	return c
}

// validatePushTarget — the push-flow target is limited to `--target-sids` (see
// ADR-032 PushApplyRequest.inventory). Any dynamic selector → explicit error
// with a hint on how to get the inventory via a separate command.
func validatePushTarget(t resolvedTarget) error {
	if len(t.SIDs) == 0 {
		return errors.New("push requires --target-sids <host1,host2,...> (inventory exact-match)")
	}
	if len(t.Coven) > 0 || t.Where != "" {
		return errors.New("push supports only --target-sids; coven/glob/regex/where are not available (use `soulctl souls list --coven=…` to get the inventory)")
	}
	return nil
}
