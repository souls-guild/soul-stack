package harness

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Identity of the SoulModule plugin fixture the live suite delivers to the stand.
// The builder that publishes it is in plugin.go and needs a stand; these names do
// not, and the guard in pluginfixture_test.go checks them against the artifact
// model without one.

// redisDocumentDir - where this tree keeps the redis module's schema document,
// relative to the repo root.
//
// ★ This is NOT where the sources are, and until NIM-868 it was both. The plugin left
// for github.com/soul-stack-plugin/redis with the rest of `examples/module/*`, and what
// stayed here is the VENDORED document — kept because soul-lint can only check a plugin
// step's `params:` where both halves are present, and eleven steps across
// examples/service/{redis,dragonfly} depend on that. `make check-plugin-schema` holds
// this copy byte-for-byte to what the pinned artifact publishes.
//
// Splitting the two meanings is what lets every docker-free guard in this package go on
// reading the document with no network and no cache: the fixture's claims about the
// artifact MODEL are claims about this file.
const redisDocumentDir = "examples/module/redis"

// redisPluginAlias - the catalog key scripts/plugin-source.sh knows the sources by.
const redisPluginAlias = "redis"

// redisPluginSourceDir - the checkout of the plugin at its PINNED commit, filling the
// cache if this machine does not have it yet.
//
// The pin, the cache and the overrides all live in scripts/plugin-source.sh and not here,
// because the Makefile needs the same answer for `check-plugin-schema`: a pin spelled in
// two places is a pin that drifts in one of them (NIM-507's shape). Read that script's
// header for why it is a pinned commit rather than a clone per run or a checkout on disk —
// the argument is NIM-876's, made for the out-of-tree service repositories first.
//
// Outside the declared bring-up region at every call site: a pin that cannot be
// materialised is a fact about THIS REPOSITORY and its cache, not about the machine's
// docker, and the classifier must not report it as a stand failure (NIM-406).
func redisPluginSourceDir(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", "plugin-source.sh"), "dir", redisPluginAlias)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolving the pinned %s plugin sources: %v\nOUTPUT:\n%s\n"+
			"\tThe sources left this tree in NIM-868. `make plugin-sources` fills the cache;\n"+
			"\tSOUL_STACK_PLUGIN_OFFLINE=1 with a cold cache fails here by design.",
			redisPluginAlias, err, out)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatalf("scripts/plugin-source.sh dir %s printed nothing", redisPluginAlias)
	}
	return dir
}

// redisBuildFlags - what makes the build of that artifact reproducible.
// plugin.go passes them here, dev/provision.sh passes them on a dev stand, and
// devprovision_test.go holds the script to this list. A Sigil grant is keyed on the
// artifact's sha256: two producers of "the same" plugin emitting different bytes are
// two different plugins, so the fixture could not reproduce a stand's failure and a
// repeat provision would invalidate a grant the operator already issued. Lives here
// rather than in plugin.go because that file needs a stand and this guard does not.
var redisBuildFlags = []string{"-trimpath", "-ldflags", "-buildid="}

// RedisPluginRef - tag under which the harness publishes the plugin
// in the per-test git repo; ref for the catalog entry and Sigil-allow.
const RedisPluginRef = "v1.0.0"

// RedisAlias - the registration alias the stand gives the artifact:
// the `plugins.soul_modules[].name` of the catalog entry, the Sigil `alias`, the
// cache_root slot name, the artifact's filename inside that slot, and the name
// of the per-host slot under `paths.modules`. It is address LEVEL 1, and it is
// the OPERATOR's word, not the artifact's: the registry key is
// `<alias>.<module>` and the scenarios address `redis.<object>.<action>`.
//
// It was `community` until NIM-766 — a word naming where the plugin came from
// rather than what it manages, which is the grouping level ADR-020's 2026-09-02
// amendment removed.
const RedisAlias = "redis"

// redisObjects - the modules the artifact declares, address level 2, one per
// OBJECT it manages. The artifact does not name ITSELF since NIM-377, but it
// still names what is inside it, and the two halves of the address come from
// different places: the alias is the operator's choice at registration, these
// are the author's.
//
// Together they are `redis.<object>`, which is what the fixtures write:
// `tests/e2e-live/module-delivery-live/service.yml` and the asserts in
// module_delivery_live_test.go. Neither is Go, so nothing but the guard
// makes them agree with the artifact.
var redisObjects = []string{"acl", "cluster", "command", "instance", "replica", "sentinel", "user"}

// redisBinaryName - the filename `dist/` gives the artifact in the
// published repo. Since NIM-377 it means NOTHING to any reader: `Manifest.BinaryName()`
// is gone, the resolver takes the one executable in `dist/` whatever it is called
// (plugingit TestResolveEntry_ArtifactNameIsIrrelevant) and renames it to the
// registration alias in the slot. Kept only so the fixture repo looks like a real
// one an author would publish.
const redisBinaryName = "redis"
