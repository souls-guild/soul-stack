// Package artifact loads git artifacts of Service repositories on the Keeper
// side: clones/updates the repository, materializes an immutable snapshot at
// a git ref (ADR-007: ref = tag or branch, semver ranges forbidden), and
// parses the root `service.yml` via the normative `shared/config` parser.
//
// Snapshots are cached at `<cacheRoot>/<name>/<sha1>/`, where `sha1` is ref
// resolved to a commit hash. A tag is immutable by nature; a branch resolves
// to its current tip on every [ServiceLoader.Load] (PM decision: always
// fetch + checkout, throttling is a separate slice). A snapshot contains no
// `.git` — it's a clean tree of the service's files.
//
// Transport is pure Go (go-git): supports `file://` (local-dev + tests),
// `https://`, and `ssh://`/scp form (auth via SSH-agent, Vault auth is
// post-MVP). Zone per architect-recon slice .a.
package artifact

import "github.com/souls-guild/soul-stack/shared/config"

// ServiceRef — coordinates of a Service repository to load.
//
// Name — the name the service is REGISTERED under: the `service_registry` primary key
// ([ADR-029](docs/adr/0029-service-registry.md)), used as the first segment of the cache
// path. The manifest states no name to match it against and has not since NIM-726 — this
// IS the authority, and the own-namespace Vault fence keys on it
// ([ADR-0083] §7, LoadScenarioManifestResolved). Git — repository URL (`file://`/`https://`/
// `ssh://`). Ref — git tag or branch (ADR-007); an empty Ref is treated as
// the repository's default `HEAD`.
type ServiceRef struct {
	Name string
	Git  string
	Ref  string
}

// ServiceArtifact — a materialized immutable snapshot of a Service repository
// at a specific commit.
//
// LocalDir points to the snapshot directory (`<cacheRoot>/<name>/<sha1>`),
// ready to be read via [ServiceLoader.ReadFile]. Manifest — the parsed root
// `service.yml`.
type ServiceArtifact struct {
	Ref      ServiceRef
	SHA1     string
	LocalDir string
	Manifest *config.ServiceManifest
	// StateSchemaVersion is the version of `incarnation.state` this snapshot
	// expects. It is DERIVED at load time from the migration ladder — the top of
	// `migrations/<NNN>_<slug>/`, [config.BaseStateSchemaVersion] when that
	// directory is empty (NIM-735) — and is deliberately NOT on the Manifest: the
	// snapshot states it, the manifest does not.
	StateSchemaVersion int
}
