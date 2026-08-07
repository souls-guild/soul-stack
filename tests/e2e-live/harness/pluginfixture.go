package harness

// Identity of the SoulModule plugin fixture the live suite delivers to the stand.
// The builder that publishes it is in plugin.go and needs a stand; these names do
// not, and the guard in pluginfixture_test.go checks them against the artifact
// model without one.

// communityRedisPluginDir - plugin sources relative to the repo root.
const communityRedisPluginDir = "examples/module/soul-mod-community-redis"

// CommunityRedisPluginRef - tag under which the harness publishes the plugin
// in the per-test git repo; ref for the catalog entry and Sigil-allow.
const CommunityRedisPluginRef = "v1.0.0"

// CommunityRedisAlias - the registration alias the stand gives the artifact:
// the `plugins.soul_modules[].name` of the catalog entry, the Sigil `alias`, the
// cache_root slot name, the artifact's filename inside that slot, and the name
// of the per-host slot under `paths.modules`. It is address LEVEL 1, not the
// module: the artifact declares one module named [communityRedisModule], the
// registry key is `<alias>.<module>`, and the scenarios address
// `community.redis.<state>`. Registering it as `redis` would key it `redis.redis`
// and resolve nothing.
const CommunityRedisAlias = "community"

// communityRedisModule - the module the artifact declares, address level 2.
// The artifact does not name ITSELF since NIM-377, but it still names what is
// inside it, and the two halves of the address come from different places: the
// alias is the operator's choice at registration, this is the author's.
//
// Together they are `community.redis`, which is what the fixtures write:
// `tests/e2e-live/module-delivery-live/service.yml`, `examples/service/redis`
// (its `modules:` entry and its scenarios) and the asserts in
// module_delivery_live_test.go. None of those is Go, so nothing but the guard
// makes them agree with the artifact.
const communityRedisModule = "redis"

// communityRedisBinaryName - the filename `dist/` gives the artifact in the
// published repo. Since NIM-377 it means NOTHING to any reader: `Manifest.BinaryName()`
// is gone, the resolver takes the one executable in `dist/` whatever it is called
// (plugingit TestResolveEntry_ArtifactNameIsIrrelevant) and renames it to the
// registration alias in the slot. Kept only so the fixture repo looks like a real
// one an author would publish.
const communityRedisBinaryName = "soul-mod-redis"
