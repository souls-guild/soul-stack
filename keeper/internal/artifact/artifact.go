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
// Name — kebab-case service name (matches `service.yml → name`), used as the
// first segment of the cache path. Git — repository URL (`file://`/`https://`/
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
}
