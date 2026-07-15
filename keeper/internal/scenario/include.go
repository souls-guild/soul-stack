package scenario

import (
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// scenarioIncludeResolver builds a [config.IncludeResolver] with a two-tier
// scenario-include resolve (orchestration.md §6): local
// `scenario/<name>/<file>` first, then service-level fallback
// `scenario/<file>`. The engine does the fallback, the author only writes the
// file name; `../` is already rejected at the validation phase
// (scenario_task.go reIncludeFile). securejoin inside
// [artifact.ServiceLoader.ReadFile] clamps any escape from the snapshot.
//
// Name collisions are shadowing: the local file fully overrides service-level
// (§6, no merge). The display path is the resolved path inside the snapshot:
// it's printed in diagnostics and serves as the cycle-detection key (two
// different resolved paths = two different sources).
func scenarioIncludeResolver(loader *artifact.ServiceLoader, art *artifact.ServiceArtifact, scenarioName string) config.IncludeResolver {
	localDir := path.Join("scenario", scenarioName)
	serviceDir := "scenario"
	return func(name string) ([]byte, string, error) {
		local := path.Join(localDir, name)
		data, err := loader.ReadFile(art, local)
		if err == nil {
			return data, local, nil
		}
		// Fall back to service-level ONLY when the local file is absent; an
		// I/O error (permission denied, broken symlink) must never be masked.
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
