package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"

	yaml "gopkg.in/yaml.v3"
)

// essenceDefaultFile — baseline-слой essence сервиса (`essence/_default.yaml`),
// host-agnostic. Каталог директив (`redis_directives`) живёт здесь; полная
// host-резолюция для его чтения не нужна. Параллель typesCatalogFile.
const essenceDefaultFile = "essence/_default.yaml"

// DirectiveCatalog — снапшот-каталог директив: SHA1 материализованного снапшота
// (служит ETag-ом, каталог immutable на git-ref) + карта `серия(major.minor) →
// отсортированные имена директив`. Форма результата lister-а /directives.
type DirectiveCatalog struct {
	SHA1       string
	Directives map[string][]string
}

// LoadDirectiveCatalog читает каталог валидных имён директив сервиса из
// `essence/_default.yaml` снапшота (ключ `redis_directives`, карта серия→[]имя)
// и, если version непуст, сужает его до серии major.minor этой версии (та же
// логика, что assert рендера, см. FilterDirectivesByVersion). serviceRoot —
// абсолютный путь к снапшоту (ServiceArtifact.LocalDir).
//
// Сервис без каталога (нет essence/_default.yaml ИЛИ нет ключа redis_directives)
// → непустой пустой map + nil-ошибка (фронт мягко деградирует, HTTP 200). Ошибка
// чтения (кроме NotExist) / невалидный YAML → ошибка (handler маппит в 502).
func LoadDirectiveCatalog(serviceRoot, version string) (map[string][]string, error) {
	full, err := loadDirectiveCatalogFull(serviceRoot)
	if err != nil {
		return nil, err
	}
	return FilterDirectivesByVersion(full, version), nil
}

// loadDirectiveCatalogFull читает весь каталог (все серии) из
// `essence/_default.yaml`. Отсутствие файла/ключа → пустой non-nil map (soft).
func loadDirectiveCatalogFull(serviceRoot string) (map[string][]string, error) {
	data, err := readSnapshotFile(serviceRoot, essenceDefaultFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string][]string{}, nil
		}
		return nil, err
	}
	// Узкий срез top-level essence: остальные ключи yaml.Unmarshal игнорирует.
	var raw struct {
		RedisDirectives map[string][]string `yaml:"redis_directives"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("artifact: парсинг %s: %w", essenceDefaultFile, err)
	}
	if raw.RedisDirectives == nil {
		return map[string][]string{}, nil
	}
	// Defensive-сортировка (генератор каталога обычно уже отсортировал имена).
	for _, names := range raw.RedisDirectives {
		sort.Strings(names)
	}
	return raw.RedisDirectives, nil
}

// FilterDirectivesByVersion сужает каталог до серий, к которым относится version
// (напр. "8.2.2" → серия "8.2"). version=="" → каталог целиком (тот же map).
// Правило членства — зеркало assert'а create/update_config (essence #6): серия s
// матчит version, если version ~ `^([0-9]+:)?<s>[.]` (опц. epoch-prefix distro-
// пина `5:7.0.15…`; трейлинг-точка = граница серии, чтобы 7.0 не цеплял 7.04).
// version без известной серии → пустой non-nil map (не блокируем, как assert-skip).
func FilterDirectivesByVersion(catalog map[string][]string, version string) map[string][]string {
	if version == "" {
		return catalog
	}
	out := make(map[string][]string, 1)
	for series, names := range catalog {
		if directiveSeriesMatchesVersion(series, version) {
			out[series] = names
		}
	}
	return out
}

// directiveSeriesMatchesVersion — регексп-мэтч серии против версии, идентичный
// CEL-assert'у рендера (RE2 в обоих). series приходит из доверенного каталога
// (major.minor), потому Compile не падает; err → false (defensive).
func directiveSeriesMatchesVersion(series, version string) bool {
	re, err := regexp.Compile("^([0-9]+:)?" + series + "[.]")
	if err != nil {
		return false
	}
	return re.MatchString(version)
}
