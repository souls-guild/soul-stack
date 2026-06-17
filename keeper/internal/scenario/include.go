package scenario

import (
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// scenarioIncludeResolver строит [config.IncludeResolver] с двухуровневым
// резолвом scenario-include (orchestration.md §6): сначала локально в
// `scenario/<name>/<file>`, затем service-level fallback `scenario/<file>`.
// Fallback делает движок, автор пишет только имя файла; `../` запрещён ещё на
// фазе валидации (scenario_task.go reIncludeFile). securejoin внутри
// [artifact.ServiceLoader.ReadFile] клампит выход за пределы снапшота.
//
// Коллизия имён — shadowing: локальный файл полностью перекрывает service-level
// (§6, без merge). display-путь — resolved-путь внутри снапшота: он же
// печатается в диагностике и служит ключом cycle-detection (два разных
// resolved-пути = два разных источника).
func scenarioIncludeResolver(loader *artifact.ServiceLoader, art *artifact.ServiceArtifact, scenarioName string) config.IncludeResolver {
	localDir := path.Join("scenario", scenarioName)
	serviceDir := "scenario"
	return func(name string) ([]byte, string, error) {
		local := path.Join(localDir, name)
		data, err := loader.ReadFile(art, local)
		if err == nil {
			return data, local, nil
		}
		// На service-level фоллбэкаем ТОЛЬКО при отсутствии локального файла;
		// I/O-ошибку (permission denied, битый симлинк) маскировать нельзя.
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("include %q: чтение локально (%s): %w", name, local, err)
		}
		service := path.Join(serviceDir, name)
		data, err = loader.ReadFile(art, service)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, "", fmt.Errorf("include %q не найден ни локально (%s), ни на service-level (%s)", name, local, service)
			}
			return nil, "", fmt.Errorf("include %q: чтение service-level (%s): %w", name, service, err)
		}
		return data, service, nil
	}
}
