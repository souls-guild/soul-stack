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

// Resolve assembles the effective essence map for one host by precedence
// (PM-decision 1):
//
//	essence/_default.yaml < essence/os/<family>.yaml < essence/coven/<c1>.yaml < essence/coven/<c2>.yaml... < IncarnationSpec
//
// Coven layers apply in name-sorted order (determinism across multiple
// covens). A missing layer file is not an error (PM-decision 3): the layer
// is skipped. Only actual read failures and invalid YAML are errors.
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

// readLayer reads and parses a YAML layer by its relative path inside
// serviceDir. Missing file → (nil, nil): the caller skips the layer. The
// path is resolved via securejoin (escaping serviceDir is excluded).
func (r *Resolver) readLayer(serviceDir, rel string) (map[string]any, error) {
	full, err := securejoin.SecureJoin(serviceDir, rel)
	if err != nil {
		return nil, fmt.Errorf("essence: unsafe layer path %q: %w", rel, err)
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.logger.Debug("essence: layer missing, skipping", "layer", rel)
			return nil, nil
		}
		return nil, fmt.Errorf("essence: read layer %q: %w", rel, err)
	}

	var layer map[string]any
	if err := yaml.Unmarshal(data, &layer); err != nil {
		return nil, fmt.Errorf("essence: parse layer %q: %w", rel, err)
	}
	return layer, nil
}
