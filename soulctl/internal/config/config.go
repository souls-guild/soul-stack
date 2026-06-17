// Package config загружает credentials оператора для soulctl.
//
// Формат файла — YAML с двумя полями:
//
//	keeper_url: https://keeper.example.com:8443
//	archon_jwt: <JWT>
//
// Расположение по умолчанию — ~/.config/soul-stack/credentials.yaml. Если файла
// нет — возвращается осмысленная ошибка с подсказкой запустить `soulctl archon login`.
// Парсинг секретов в файлы за пределами home — намеренно НЕ поддерживается (любой
// override — отдельная задача через флаг --credentials, если потребуется).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
)

// Credentials — содержимое credentials.yaml оператора.
type Credentials struct {
	KeeperURL string `yaml:"keeper_url"`
	ArchonJWT string `yaml:"archon_jwt"`
}

// DefaultPath — каноническое расположение credentials-файла под $HOME.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("определить домашний каталог: %w", err)
	}
	return filepath.Join(home, ".config", "soul-stack", "credentials.yaml"), nil
}

// Load читает credentials из указанного пути. Если path пуст — берётся DefaultPath().
//
// Возвращаемые ошибки:
//   - отсутствует файл                → подсказка `soulctl archon login`;
//   - YAML невалиден / пустые поля    → конкретная диагностика.
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
