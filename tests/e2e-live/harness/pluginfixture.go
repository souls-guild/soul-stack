package harness

// Identity of the SoulModule plugin fixture the live suite delivers to the stand.
// The builder that publishes it is in plugin.go and needs a stand; these names do
// not, and the guard in pluginfixture_test.go checks them against the artifact
// model without one.

// redisPluginDir - plugin sources relative to the repo root.
const redisPluginDir = "examples/module/redis"

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
// `tests/e2e-live/module-delivery-live/service.yml`, `examples/service/redis`
// (its `modules:` entries and its scenarios) and the asserts in
// module_delivery_live_test.go. None of those is Go, so nothing but the guard
// makes them agree with the artifact.
var redisObjects = []string{"acl", "cluster", "command", "instance", "replica", "sentinel", "user"}

// redisBinaryName - the filename `dist/` gives the artifact in the
// published repo. Since NIM-377 it means NOTHING to any reader: `Manifest.BinaryName()`
// is gone, the resolver takes the one executable in `dist/` whatever it is called
// (plugingit TestResolveEntry_ArtifactNameIsIrrelevant) and renames it to the
// registration alias in the slot. Kept only so the fixture repo looks like a real
// one an author would publish.
const redisBinaryName = "redis"
