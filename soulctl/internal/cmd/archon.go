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
		Short: "аутентификация и идентичность оператора (Archon)",
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
		Short: "сохранить keeper_url + JWT в credentials.yaml (валидируется ping-ом)",
		Long: `archon login читает JWT из --jwt-file, валидирует его пингом
авторизованного эндпоинта (GET /v1/incarnations?limit=1), и при успехе
сохраняет credentials в ~/.config/soul-stack/credentials.yaml (mode 0600).

В Operator API MVP нет отдельного /v1/whoami endpoint-а, поэтому для проверки
используется любой авторизованный list — это даёт быструю обратную связь по
401 (битый JWT) и 403 (есть JWT, но нет права incarnation.list).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if keeperURL == "" {
				return errors.New("--keeper-url обязателен")
			}
			if jwtFile == "" {
				return errors.New("--jwt-file обязателен")
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
	c.Flags().StringVar(&keeperURL, "keeper-url", "", "базовый URL Keeper-а (https://keeper.example:8443)")
	c.Flags().StringVar(&jwtFile, "jwt-file", "", "путь к файлу с JWT (одной строкой)")
	return c
}

func newArchonWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "показать текущего Архонта (AID + permissions из JWT-claims)",
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
		fmt.Fprintln(out, "Note:       bootstrap-initial JWT (см. ADR-013)")
	}
	return nil
}

func newArchonLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "удалить credentials.yaml",
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
				return fmt.Errorf("удалить %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "logged out. removed %s\n", path)
			return nil
		},
	}
}

// readJWTFile читает JWT из файла, trim-ит whitespace и trailing newline.
// JWT в файле — одна строка; multiline → ошибка.
func readJWTFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("прочитать JWT из %s: %w", path, err)
	}
	jwt := strings.TrimSpace(string(data))
	if jwt == "" {
		return "", fmt.Errorf("JWT-файл %s пуст", path)
	}
	if strings.ContainsAny(jwt, " \t\n\r") {
		return "", fmt.Errorf("JWT-файл %s содержит пробельные символы внутри токена", path)
	}
	return jwt, nil
}

// saveCredentials записывает creds в credentials.yaml с mode 0600. Возвращает
// фактический путь записи.
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
		return "", fmt.Errorf("создать каталог %s: %w", filepath.Dir(path), err)
	}
	data, err := yaml.Marshal(creds)
	if err != nil {
		return "", fmt.Errorf("сериализовать credentials: %w", err)
	}
	// Атомарная запись через tmp+rename, чтобы не оставить полу-файл при сбое.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", fmt.Errorf("записать %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("переименовать %s → %s: %w", tmp, path, err)
	}
	return path, nil
}
