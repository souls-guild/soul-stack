package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"github.com/souls-guild/soul-stack/soulctl/internal/client"
	"github.com/souls-guild/soul-stack/soulctl/internal/config"
	"github.com/souls-guild/soul-stack/soulctl/internal/output"
)

func newArchonCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "archon",
		Short: "operator authentication and identity (Archon)",
	}
	c.AddCommand(newArchonLoginCmd(), newArchonWhoamiCmd(), newArchonLogoutCmd())
	return c
}

func newArchonLoginCmd() *cobra.Command {
	var (
		keeperURL string
		jwtFile   string
	)
	c := &cobra.Command{
		Use:   "login",
		Short: "save keeper_url + JWT to credentials.yaml (validated by ping)",
		Long: `archon login reads the JWT from --jwt-file, validates it by pinging
an authorized endpoint (GET /v1/incarnations?limit=1), and on success
saves credentials to ~/.config/soul-stack/credentials.yaml (mode 0600).

The Operator API MVP has no separate /v1/whoami endpoint, so any
authorized list is used for the check - this gives fast feedback on
401 (broken JWT) and 403 (JWT present, but no incarnation.list permission).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if keeperURL == "" {
				return errors.New("--keeper-url is required")
			}
			if jwtFile == "" {
				return errors.New("--jwt-file is required")
			}
			jwt, err := readJWTFile(jwtFile)
			if err != nil {
				return err
			}
			creds := &config.Credentials{KeeperURL: keeperURL, ArchonJWT: jwt}
			cl, err := client.New(creds)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			if err := cl.Ping(ctx); err != nil {
				return renderAPIError(err)
			}
			path, err := saveCredentials(RootFlags(cmd).ConfigPath, creds)
			if err != nil {
				return err
			}
			claims, decodeErr := client.DecodeJWTClaims(jwt)
			aid := ""
			if decodeErr == nil {
				aid = claims.Sub
			}
			if aid == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "logged in. credentials saved to %s\n", path)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "logged in as %s. credentials saved to %s\n", aid, path)
			}
			return nil
		},
	}
	c.Flags().StringVar(&keeperURL, "keeper-url", "", "base Keeper URL (https://keeper.example:8443)")
	c.Flags().StringVar(&jwtFile, "jwt-file", "", "path to a file with the JWT (single line)")
	return c
}

func newArchonWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "show the current Archon (AID + permissions from JWT claims)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := loadClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			claims, err := cl.Archon.Whoami(ctx)
			if err != nil {
				return renderAPIError(err)
			}
			rf := RootFlags(cmd)
			view := whoamiView{
				AID:              claims.Sub,
				Issuer:           claims.Iss,
				Roles:            claims.Roles,
				BootstrapInitial: claims.BootstrapInitial,
				KeeperURL:        cl.BaseURL(),
			}
			if claims.Iat > 0 {
				view.IssuedAt = time.Unix(claims.Iat, 0).UTC().Format(time.RFC3339)
			}
			if claims.Exp > 0 {
				view.ExpiresAt = time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339)
			}
			if rf.Output == output.FormatJSON {
				return output.JSON(cmd.OutOrStdout(), view)
			}
			return whoamiPrint(cmd, view)
		},
	}
}

type whoamiView struct {
	AID              string   `json:"aid"`
	Issuer           string   `json:"issuer,omitempty"`
	Roles            []string `json:"roles,omitempty"`
	BootstrapInitial bool     `json:"bootstrap_initial,omitempty"`
	IssuedAt         string   `json:"issued_at,omitempty"`
	ExpiresAt        string   `json:"expires_at,omitempty"`
	KeeperURL        string   `json:"keeper_url"`
}

func whoamiPrint(cmd *cobra.Command, v whoamiView) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "AID:        %s\n", v.AID)
	if v.Issuer != "" {
		fmt.Fprintf(out, "Issuer:     %s\n", v.Issuer)
	}
	fmt.Fprintf(out, "Keeper URL: %s\n", v.KeeperURL)
	if len(v.Roles) > 0 {
		fmt.Fprintf(out, "Roles:      %s\n", strings.Join(v.Roles, ", "))
	} else {
		fmt.Fprintln(out, "Roles:      <none>")
	}
	if v.IssuedAt != "" {
		fmt.Fprintf(out, "Issued at:  %s\n", v.IssuedAt)
	}
	if v.ExpiresAt != "" {
		fmt.Fprintf(out, "Expires at: %s\n", v.ExpiresAt)
	}
	if v.BootstrapInitial {
		fmt.Fprintln(out, "Note:       bootstrap-initial JWT (see ADR-013)")
	}
	return nil
}

func newArchonLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "delete credentials.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rf := RootFlags(cmd)
			path := rf.ConfigPath
			if path == "" {
				var err error
				path, err = config.DefaultPath()
				if err != nil {
					return err
				}
			}
			if err := os.Remove(path); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					fmt.Fprintf(cmd.OutOrStdout(), "credentials file %s already absent\n", path)
					return nil
				}
				return fmt.Errorf("remove %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "logged out. removed %s\n", path)
			return nil
		},
	}
}

// readJWTFile reads the JWT from a file, trimming whitespace and the
// trailing newline. The file must hold a single line; multiline → error.
func readJWTFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read JWT from %s: %w", path, err)
	}
	jwt := strings.TrimSpace(string(data))
	if jwt == "" {
		return "", fmt.Errorf("JWT file %s is empty", path)
	}
	if strings.ContainsAny(jwt, " \t\n\r") {
		return "", fmt.Errorf("JWT file %s contains whitespace inside the token", path)
	}
	return jwt, nil
}

// saveCredentials writes creds to credentials.yaml with mode 0600. Returns
// the actual path written.
func saveCredentials(override string, creds *config.Credentials) (string, error) {
	path := override
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create directory %s: %w", filepath.Dir(path), err)
	}
	data, err := yaml.Marshal(creds)
	if err != nil {
		return "", fmt.Errorf("serialize credentials: %w", err)
	}
	// Atomic write via tmp+rename, so a failure never leaves a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return path, nil
}
