package essence

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/goccy/go-yaml"
)

// Resolve собирает effective essence-map для одного хоста по precedence
// (PM-decision 1):
//
//	essence/_default.yaml < essence/os/<family>.yaml < essence/coven/<c1>.yaml < essence/coven/<c2>.yaml... < IncarnationSpec
//
// Coven-слои применяются в порядке сортировки имён (детерминизм при нескольких
// covens). Отсутствие любого файла-слоя — не ошибка (PM-decision 3): слой
// пропускается. Ошибкой считаются только реальные сбои чтения и невалидный
// YAML.
func (r *Resolver) Resolve(in ResolveInput) (map[string]any, error) {
	result := make(map[string]any)

	layer, err := r.readLayer(in.ServiceDir, defaultFile)
	if err != nil {
		return nil, err
	}
	result = mergeInto(result, layer)

	if in.OSFamily != "" {
		osPath := path.Join(osDir, in.OSFamily+".yaml")
		layer, err = r.readLayer(in.ServiceDir, osPath)
		if err != nil {
			return nil, err
		}
		result = mergeInto(result, layer)
	}

	covens := append([]string(nil), in.Covens...)
	sort.Strings(covens)
	for _, coven := range covens {
		if coven == "" {
			continue
		}
		covenPath := path.Join(covenDir, coven+".yaml")
		layer, err = r.readLayer(in.ServiceDir, covenPath)
		if err != nil {
			return nil, err
		}
		result = mergeInto(result, layer)
	}

	if in.IncarnationSpec != nil {
		result = mergeInto(result, in.IncarnationSpec)
	}

	return result, nil
}

// readLayer читает и парсит YAML-слой по relative-path внутри serviceDir.
// Отсутствие файла → (nil, nil): слой пропускается вызывающим. Путь
// резолвится через securejoin (выход за пределы serviceDir исключён).
func (r *Resolver) readLayer(serviceDir, rel string) (map[string]any, error) {
	full, err := securejoin.SecureJoin(serviceDir, rel)
	if err != nil {
		return nil, fmt.Errorf("essence: небезопасный путь слоя %q: %w", rel, err)
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.logger.Debug("essence: слой отсутствует, пропуск", "layer", rel)
			return nil, nil
		}
		return nil, fmt.Errorf("essence: чтение слоя %q: %w", rel, err)
	}

	var layer map[string]any
	if err := yaml.Unmarshal(data, &layer); err != nil {
		return nil, fmt.Errorf("essence: парсинг слоя %q: %w", rel, err)
	}
	return layer, nil
}
