// Package cmd assembles the soulctl cobra command tree.
//
// Structure: seven top-level command groups (incarnation / souls / soul / errand / archon / push-providers / run),
// each in its own file. Global flags (`--output`, `--config`) live on root and are read
// by commands via RootFlags(cmd).
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/config"
	"github.com/souls-guild/soul-stack/soulctl/internal/output"
)

const (
	flagOutput = "output"
	flagConfig = "config"
)

// rootFlags is a typed projection of the global flags. Stored in
// cmd.Context() via withRootFlags so subcommands can read it without reflection.
type rootFlags struct {
	Output     output.Format
	ConfigPath string
}

type rootFlagsKey struct{}

func withRootFlags(ctx context.Context, f rootFlags) context.Context {
	return context.WithValue(ctx, rootFlagsKey{}, f)
}

// RootFlags extracts rootFlags from cmd.Context(). If the flags were never
// set (e.g. in unit tests that don't call Execute), it returns the zero
// value, which is equivalent to table output and the DefaultPath credentials.
func RootFlags(cmd *cobra.Command) rootFlags {
	if f, ok := cmd.Context().Value(rootFlagsKey{}).(rootFlags); ok {
		return f
	}
	return rootFlags{Output: output.FormatTable}
}

// NewRoot builds the soulctl root command.
func NewRoot(version string) *cobra.Command {
	var outputFlag string
	var configFlag string

	root := &cobra.Command{
		Use:           "soulctl",
		Short:         "soul-stack operator CLI",
		Long:          "soulctl — клиентский CLI оператора Soul Stack, тонкая обёртка над Operator API Keeper-а.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			f, err := output.ParseFormat(outputFlag)
			if err != nil {
				return err
			}
			ctx := withRootFlags(cmd.Context(), rootFlags{
				Output:     f,
				ConfigPath: configFlag,
			})
			cmd.SetContext(ctx)
			return nil
		},
	}
	root.SetVersionTemplate("soulctl {{.Version}} — soul-stack operator CLI\n")
	root.PersistentFlags().StringVarP(&outputFlag, flagOutput, "o", "table", "формат вывода: table|json|yaml")
	root.PersistentFlags().StringVar(&configFlag, flagConfig, "", "путь к credentials.yaml (по умолчанию ~/.config/soul-stack/credentials.yaml)")

	root.AddCommand(
		newIncarnationCmd(),
		newSoulsCmd(),
		newSoulCmd(),
		newErrandCmd(),
		newArchonCmd(),
		newPushProvidersCmd(),
		newRunCmd(),
	)
	return root
}

// signalContext is a context cancelled on SIGINT/SIGTERM. Subcommands with
// long-running poll operations should use it.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(ch)
	}()
	return ctx, cancel
}

// loadClient is the shared credentials + client loader for commands that need the API.
func loadClient(cmd *cobra.Command) (*client.Client, error) {
	rf := RootFlags(cmd)
	creds, err := config.Load(rf.ConfigPath)
	if err != nil {
		return nil, err
	}
	return client.New(creds)
}

// renderAPIError wraps an HTTP call error into a kubectl-like form:
// 401 → a hint to log in, 403 → missing permission, 404 → not found,
// 5xx → keeper error.
func renderAPIError(err error) error {
	apiErr, ok := client.AsAPIError(err)
	if !ok {
		return err
	}
	switch apiErr.Status {
	case 401:
		return errors.New("not authenticated. Run `soulctl archon login`")
	case 403:
		detail := apiErr.Detail
		if detail == "" {
			detail = apiErr.Title
		}
		return fmt.Errorf("forbidden: %s", detail)
	case 404:
		detail := apiErr.Detail
		if detail == "" {
			detail = apiErr.Title
		}
		return fmt.Errorf("not found: %s", detail)
	}
	if apiErr.Status >= 500 {
		detail := apiErr.Detail
		if detail == "" {
			detail = apiErr.Title
		}
		return fmt.Errorf("keeper error: %s", detail)
	}
	return err
}
