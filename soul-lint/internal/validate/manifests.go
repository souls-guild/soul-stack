package validate

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// dirManifests is a [config.ModuleManifestResolver] over manifests found on
// disk — what `soul-lint --modules <dir>` builds.
//
// Offline is the whole point: keeper resolves a plugin manifest from its Sigil
// grants, which an author writing a definition on their laptop does not have.
// Where both halves sit in one tree (a service checkout that vendors its
// plugins, or this repo's own examples/), the author gets the same four checks
// keeper would run, before the run rather than as a module.unknown_param on the
// host.
type dirManifests map[string]*plugin.Manifest

func (d dirManifests) ResolveModule(namespace, name string) (*plugin.Manifest, bool) {
	m, ok := d[namespace+"."+name]
	return m, ok
}

// LoadModuleManifests walks dir for `manifest.yaml` files and indexes the
// SoulModule ones by `<namespace>.<name>`.
//
// Indexed by what the manifest DECLARES, never by the directory it sits in: the
// address a task writes is the module's own, and a plugin directory is named by
// its binary (`soul-mod-community-redis`) rather than its address
// (`community.redis`).
//
// A file that does not parse, or that is a cloud_driver / ssh_provider rather
// than a soul_module, is skipped rather than fatal — the flag points at a tree,
// and one unrelated manifest in it must not cost the author every other check.
// A module that ends up unindexed is reported per definition as
// `plugin_params_unchecked`, so the skip is visible where it matters.
//
// A missing or unreadable dir IS fatal: the author asked for these checks, and
// silently running without them is the failure this ticket exists to remove.
func LoadModuleManifests(dir string) (config.ModuleManifestResolver, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("--modules %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("--modules %s: not a directory", dir)
	}

	out := dirManifests{}
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != plugin.FileName {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		m, diags := plugin.LoadFromBytes(path, src)
		if m == nil || diag.HasErrors(diags) || m.Kind != plugin.KindSoulModule {
			return nil
		}
		if m.Namespace == "" || m.Name == "" {
			return nil
		}
		out[m.Namespace+"."+m.Name] = m
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("--modules %s: %w", dir, walkErr)
	}
	return out, nil
}
