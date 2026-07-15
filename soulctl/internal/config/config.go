// Package config loads operator credentials for soulctl.
//
// The file format is YAML with two fields:
//
//	keeper_url: https://keeper.example.com:8443
//	archon_jwt: <JWT>
//
// The default location is ~/.config/soul-stack/credentials.yaml. If the file
// is missing, a meaningful error is returned with a hint to run
// `soulctl archon login`. Reading secrets from files outside home is
// intentionally NOT supported (any override is a separate task via a
// --credentials flag, if needed).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
)

// Credentials is the content of the operator's credentials.yaml.
type Credentials struct {
	KeeperURL string `yaml:"keeper_url"`
	ArchonJWT string `yaml:"archon_jwt"`
}

// DefaultPath is the canonical location of the credentials file under $HOME.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("определить домашний каталог: %w", err)
	}
	return filepath.Join(home, ".config", "soul-stack", "credentials.yaml"), nil
}

// Load reads credentials from the given path. If path is empty, DefaultPath()
// is used.
//
// Errors returned:
//   - missing file                 → hint to run `soulctl archon login`;
//   - invalid YAML / empty fields  → specific diagnostics.
func Load(path string) (*Credentials, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("credentials-файл %s не найден — выполните `soulctl archon login` для аутентификации", path)
		}
		return nil, fmt.Errorf("прочитать %s: %w", path, err)
	}
	var c Credentials
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("разобрать %s: %w", path, err)
	}
	if c.KeeperURL == "" {
		return nil, fmt.Errorf("в %s не задан keeper_url", path)
	}
	if c.ArchonJWT == "" {
		return nil, fmt.Errorf("в %s не задан archon_jwt — выполните `soulctl archon login`", path)
	}
	return &c, nil
}
