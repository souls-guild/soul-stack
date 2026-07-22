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

// newPushProvidersCmd — `soulctl push-providers …` (ADR-032 amendment 2026-05-26, S7-2).
// Replaces the inline `keeper.yml::push.providers[]` form (pilot S6 / S7-1).
func newPushProvidersCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "push-providers",
		Short:   "manage per-provider params of push-flow SSH plugins (S7-2, push_providers)",
		Aliases: []string{"push-provider"},
	}
	c.AddCommand(
		newPushProvidersCreateCmd(),
		newPushProvidersUpdateCmd(),
		newPushProvidersDeleteCmd(),
		newPushProvidersListCmd(),
		newPushProvidersGetCmd(),
	)
	return c
}

func newPushProvidersCreateCmd() *cobra.Command {
	var paramsJSON string
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "create a Push Provider record",
		Long: `Creates a record in push_providers (per-provider env-payload params of the push-flow SSH plugin).

Sensitive params (secret_id/token/password/private_key) MUST be vault-refs (vault:<path>).
Permission: push-provider.create.

Examples:
  soulctl push-providers create vault-bastion --params '{"vault_addr":"https://vault.example.com","role":"keeper","secret_id":"vault:secret/keeper/vault-approle#secret_id"}'
  soulctl push-providers create static-key`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			var params map[string]any
			if paramsJSON != "" {
				if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
					return fmt.Errorf("--params is not JSON: %w", err)
				}
			}
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			reply, err := cl.PushProviders.Create(ctx, client.PushProviderBody{Name: name, Params: params})
			if err != nil {
				return renderAPIError(err)
			}
			return output.JSON(cmd.OutOrStdout(), reply)
		},
	}
	c.Flags().StringVar(&paramsJSON, "params", "", "params as a JSON object (sensitive keys must be vault-refs)")
	return c
}

func newPushProvidersUpdateCmd() *cobra.Command {
	var paramsJSON string
	c := &cobra.Command{
		Use:   "update <name>",
		Short: "replace a Push Provider's params (replace semantics)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if paramsJSON == "" {
				return fmt.Errorf("--params is required")
			}
			var params map[string]any
			if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
				return fmt.Errorf("--params is not JSON: %w", err)
			}
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			reply, err := cl.PushProviders.Update(ctx, name, client.PushProviderUpdateBody{Params: params})
			if err != nil {
				return renderAPIError(err)
			}
			return output.JSON(cmd.OutOrStdout(), reply)
		},
	}
	c.Flags().StringVar(&paramsJSON, "params", "", "new set of params as a JSON object (full replace)")
	return c
}

func newPushProvidersDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "delete a Push Provider record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			if err := cl.PushProviders.Delete(ctx, args[0]); err != nil {
				return renderAPIError(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "deleted")
			return nil
		},
	}
}

func newPushProvidersListCmd() *cobra.Command {
	var (
		namePattern string
		limit       int
		offset      int
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "list Push Providers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			reply, err := cl.PushProviders.List(ctx, client.PushProviderListOptions{
				NamePattern: namePattern,
				Limit:       limit,
				Offset:      offset,
			})
			if err != nil {
				return renderAPIError(err)
			}
			return output.JSON(cmd.OutOrStdout(), reply)
		},
	}
	c.Flags().StringVar(&namePattern, "name-pattern", "", "LIKE-style name filter (e.g. vault%)")
	c.Flags().IntVar(&limit, "limit", 100, "max records per page (1..1000)")
	c.Flags().IntVar(&offset, "offset", 0, "offset from the start")
	return c
}

func newPushProvidersGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "read a Push Provider record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			reply, err := cl.PushProviders.Get(ctx, args[0])
			if err != nil {
				return renderAPIError(err)
			}
			return output.JSON(cmd.OutOrStdout(), reply)
		},
	}
}
