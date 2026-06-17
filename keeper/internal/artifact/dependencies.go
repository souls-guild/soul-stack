package artifact

import (
	"fmt"
	"log/slog"
	"os"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// ServiceDependencies — проекция git-зависимостей одного снапшота Service-репо
// для UI Service Detail (`GET /v1/services/{name}/dependencies`): задекларированные
// в `service.yml` destiny-кирпичики и custom-модули, каждый со своим git-ref-ом
// (ADR-007: версия = git tag/branch). Источник — top-level блоки `destiny:` /
// `modules:` манифеста (shared/config.ServiceManifest); content самих
// destiny/модулей НЕ грузится — оператору в Detail нужны только `{name, ref}`
// (что и под каким тегом тянет сервис).
//
// Имена JSON-полей совпадают с UI-API (`ServiceDependenciesReply`); оба слайса
// non-nil после [ListDependencies] (сервис без зависимостей валиден → пустые
// массивы, не null — parity со [StateSchemaInfo.Migrations] / [ListScenarios]).
type ServiceDependencies struct {
	Destiny []Dependency `json:"destiny"`
	Modules []Dependency `json:"modules"`
}

// Dependency — одна запись `destiny[]` / `modules[]` манифеста (metadata-only):
// `name` (kebab-case destiny / двухуровневый `<namespace>.<module>`), `ref`
// (git tag или branch, ADR-007) и опц. `git` (per-entry override полного URL,
// поддержан только для destiny[] — для modules[] всегда пуст по контракту
// config.validateDependencyRef).
type Dependency struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	Git  string `json:"git,omitempty"`
}

// ListDependencies собирает [ServiceDependencies] из материализованного снапшота
// service-репо (serviceRoot — абсолютный путь, обычно [ServiceArtifact.LocalDir]).
//
// Парсит `service.yml` нормативным [config.LoadServiceManifestFromBytes];
// error-диагностика == битый манифест в репо → ошибка выше (caller отдаёт 502),
// parity со [ListStateSchema]. Сами блоки `destiny:` / `modules:` опциональны:
// отсутствуют → пустые слайсы (сервис без зависимостей валиден). Логгер
// опционален (nil → slog.Default); сейчас не используется (чтение манифеста —
// без partial-success), но сигнатура симметрична [ListStateSchema] / [ListScenarios].
func ListDependencies(serviceRoot string, logger *slog.Logger) (*ServiceDependencies, error) {
	if logger == nil {
		logger = slog.Default()
	}

	manifestPath, err := securejoin.SecureJoin(serviceRoot, serviceManifestFile)
	if err != nil {
		return nil, fmt.Errorf("artifact: небезопасный путь %s: %w", serviceManifestFile, err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("artifact: чтение %s: %w", serviceManifestFile, err)
	}
	manifest, _, diags, err := config.LoadServiceManifestFromBytes(serviceManifestFile, data, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: парсинг %s: %w", serviceManifestFile, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("artifact: %s невалиден: %s", serviceManifestFile, firstError(diags))
	}

	return &ServiceDependencies{
		Destiny: toDependencies(manifest.Destiny),
		Modules: toDependencies(manifest.Modules),
	}, nil
}

// toDependencies проецирует []config.DependencyRef в []Dependency (non-nil
// результат — пустой манифест-блок отдаётся как `[]`, не null).
func toDependencies(refs []config.DependencyRef) []Dependency {
	out := make([]Dependency, 0, len(refs))
	for _, r := range refs {
		out = append(out, Dependency{Name: r.Name, Ref: r.Ref, Git: r.Git})
	}
	return out
}
