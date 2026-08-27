package harness

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The release tarballs a redis create fetches from the public internet, and the
// fixture's local stand-in for them.
//
// NIM-542. `make e2e-live-gate` is a blocking pre-tag step (RELEASING.md step e),
// and six of its nine tests run a create of examples/service/redis. Every one of
// those creates reached out to github.com three times — node_exporter,
// redis_exporter and vector, ~18 downloads per gate run — so the gate's verdict
// was decided partly by a network nobody in the release process controls. NIM-406
// measured what that costs: three consecutive runs on a slice that did not change
// by a byte gave three different answers, and one of the two reds died in
// `core.url` resolving github.com. A blocking gate whose red is sometimes about
// the weather is a gate people learn to rerun, and that habit is what retires a
// regression.
//
// The lever is the example's own, not one invented for the test.
// examples/service/redis/vars/00-base.yaml already documents it: point
// `<prefix>_base_url` at "an internal raw-proxy mirroring the github path 1:1,
// plus `<prefix>_allow_private: true` when that mirror resolves to a private IP".
// The fixture is such a mirror. Nothing in examples/ changes — it is the subject
// of these tests (NIM-211) and bending the subject to fit the test would leave
// the gate green about a service nobody runs.
//
// Deliberately untagged, like setupdecl.go and waitstrategy.go: everything below
// is a claim about what the example declares, and a claim that only compiles with
// docker present is one that drifts unobserved. artifactcatalog_test.go checks
// every field here against examples/, docker-free, inside `make e2e-live-gate`'s
// first step.

// artifactArch — the only architecture the fixture caches for.
//
// The L3b soul runs in dockerfiles/debian-12.Dockerfile on a linux/amd64 image
// and `make build-linux` cross-compiles linux-amd64, so `soulprint.self.os.arch`
// inside the container is amd64 and the destinies build an amd64 URL. Caching one
// arch is therefore not a limitation but a statement of what this tier is; a host
// that somehow produced another arch would miss every cache entry, which
// startArtifactMirrorFor turns into a named failure rather than a silent fetch
// from upstream.
const artifactArch = "amd64"

// upstreamArtifact — one external tarball: where the example says it lives, which
// version the example pins, and what the file is called there.
type upstreamArtifact struct {
	// varPrefix — the service-vars prefix the create scenario reads. For
	// node_exporter that is `vars.node_exporter_base_url` /
	// `vars.node_exporter_allow_private` / `vars.node_exporter_version`
	// (examples/service/redis/scenario/create/main.yml).
	varPrefix string

	// upstreamBase — the public release root, and the literal the scenario falls
	// back to in `default(vars.<prefix>_base_url, '…')`. Held here so the cache
	// primer and the non-gate real-path test fetch from the same place the
	// example does, rather than from a second copy of the URL that can drift.
	upstreamBase string

	// version — the value of `vars.<prefix>_version` in the example's
	// 00-base.yaml. Part of the path on both sides: `<base>/v<version>/<file>`.
	version string

	// file — the tarball's name at that path.
	file string

	// sha256 — the digest of those bytes, lower-case hex, no `sha256:` prefix.
	//
	// Pinned here even for node_exporter, which the example deliberately does NOT
	// pin (examples/destiny/node-exporter/tasks/install.yml: no `sha256` input, and
	// `core.url` falls back to content-hash idempotency). That is not a reason to
	// leave it unpinned here — it is the reason to pin it here hardest. For
	// redis_exporter and vector, bad bytes in the cache would at least come back as
	// the product's own checksum failure, wrongly worn as a finding about the code.
	// For node_exporter nothing downstream is looking: a short or swapped tarball
	// goes in, `tar` fails on garbage, and the report is an extraction error in a
	// destiny that did nothing wrong. Verifying on the way into the cache is the
	// only place any of the three is checked against a value this fixture chose.
	sha256 string
}

// upstreamURL — where the primer fetches it from, and the URL the destiny itself
// builds when the mirror is out of the picture.
func (a upstreamArtifact) upstreamURL() string {
	return a.upstreamBase + "/v" + a.version + "/" + a.file
}

// cacheRel — the artifact's path inside the cache directory, which is also its
// path under the mirror's root. Prefixed by varPrefix so the three release roots
// stay three roots: the mirror hands each destiny `<mirror>/<varPrefix>` as its
// base_url and the rest of the path is the upstream layout, verbatim.
func (a upstreamArtifact) cacheRel() string {
	return path.Join(a.varPrefix, "v"+a.version, a.file)
}

// artifactCatalog — the three tarballs, as examples/service/redis declares them.
//
// Every field is checked against the example by artifactcatalog_test.go. A
// version bump there, a fourth fetch added to the scenario, or a changed release
// root all go red there rather than quietly restoring the outbound dependency
// this ticket removed.
func artifactCatalog() []upstreamArtifact {
	return []upstreamArtifact{
		{
			varPrefix:    "node_exporter",
			upstreamBase: "https://github.com/prometheus/node_exporter/releases/download",
			version:      "1.8.2",
			file:         "node_exporter-1.8.2.linux-" + artifactArch + ".tar.gz",
			sha256:       "6809dd0b3ec45fd6e992c19071d6b5253aed3ead7bf0686885a51d85c6643c66",
		},
		{
			varPrefix:    "redis_exporter",
			upstreamBase: "https://github.com/oliver006/redis_exporter/releases/download",
			version:      "1.62.0",
			file:         "redis_exporter-v1.62.0.linux-" + artifactArch + ".tar.gz",
			sha256:       "a09f92a6b366e37c654e50522c7b80e4a625396b2499fd42cf17e1aa91e56d5e",
		},
		{
			varPrefix:    "vector",
			upstreamBase: "https://github.com/vectordotdev/vector/releases/download",
			version:      "0.40.0",
			// vector names its tarballs by Rust triplet, not by GOARCH — the
			// mapping is the example's own (examples/destiny/vector/vars.yml,
			// vars.arch_triplet: amd64 -> x86_64-unknown-linux-gnu).
			file:   "vector-0.40.0-x86_64-unknown-linux-gnu.tar.gz",
			sha256: "112b047df17df46feb22fc69234e8fc2ad5a411cd2e7d369f3b70c00617a4e90",
		},
	}
}

// artifactMirrorVarsFile — the layer the fixture adds to the materialized service
// repo, relative to the service root.
//
// A vars LAYER, not an edit: shared/config and keeper/internal/servicevars
// assemble `vars/*.yaml` in lexical order and a later file wins (ADR-0082 §3), so
// a file that sorts after the example's own leaves 00-base.yaml untouched and
// overrides three keys. `99-` puts it last among numbered layers, and
// TestArtifactMirrorOverlaySortsLast checks that against the directory as it
// actually is rather than against that intention — `_` is 0x5F and lower-case
// letters are above every digit, so "sorts last" is a property of the siblings,
// not of the prefix.
const artifactMirrorVarsFile = "vars/99-e2e-live-artifact-mirror.yaml"

// artifactMirrorOverlay renders that layer for a mirror rooted at mirrorBase
// (scheme, host and port, no trailing slash).
//
// `allow_private: true` is the other half and is not optional: mirrorBase is the
// host's LAN address (keeperEndpointHost — the same one the soul container
// already dials for keeper), core.url's SSRF guard refuses to dial RFC1918, and
// the destinies expose the per-apply opt-out precisely for a mirror on an
// internal network (examples/destiny/*/tasks: `allow_private: "${ input.allow_private }"`).
func artifactMirrorOverlay(mirrorBase string, cat []upstreamArtifact) []byte {
	var b strings.Builder
	b.WriteString(`# GENERATED BY tests/e2e-live — NOT part of examples/service/redis.
#
# Written into the fixture's throwaway copy of the service repo, never into the
# tree (NIM-542, NIM-211: examples/ is the subject of these tests). It points the
# three release fetches of scenario/create/main.yml at the harness's local mirror,
# so a gate run needs nothing from github.com. The real upstream path stays
# covered outside the blocking gate — see TestL3bRedisLive_UpstreamArtifactsLive.
#
# 00-base.yaml documents this exact override as the supported way to run against
# an internal mirror. This layer is that, with the mirror served out of the
# harness process.
`)
	for _, a := range cat {
		fmt.Fprintf(&b, "\n%s_base_url: %q\n%s_allow_private: true\n",
			a.varPrefix, mirrorBase+"/"+a.varPrefix, a.varPrefix)
	}
	return []byte(b.String())
}

// artifactMirrorPrefixes — the catalog's varPrefixes, sorted. Used by the guards
// and by the served-something-real assertion.
func artifactMirrorPrefixes(cat []upstreamArtifact) []string {
	out := make([]string, 0, len(cat))
	for _, a := range cat {
		out = append(out, a.varPrefix)
	}
	sort.Strings(out)
	return out
}

// reScenarioBaseURLDefault — the shape a scenario uses to name an external
// download root: `${ default(vars.<prefix>_base_url, '<url>') }`. Captures the
// prefix and the fallback, which is empty for the vars an operator has to switch
// on (`modules_base_url`, `binary_base_url`) and a real URL for the fetches that
// happen by default.
var reScenarioBaseURLDefault = regexp.MustCompile(`default\(vars\.([A-Za-z0-9_]+)_base_url,\s*'([^']*)'\)`)

// scenarioArtifactPrefixes — the prefixes a materialized service actually fetches
// over http by default, read out of its own scenario/ tree.
//
// Read from the COPY rather than assumed from the catalog, and that is the point:
// the overlay is written only for the services that read these vars, so a stand
// on smoke-nginx-live gets no artifact layer and is not later accused of failing
// to use a mirror it never had a reason to touch. `scenario/*/tests/` is skipped —
// those are render fixtures, and the URLs in them are never dialed.
func scenarioArtifactPrefixes(serviceDir string) (map[string]bool, error) {
	root := filepath.Join(serviceDir, "scenario")
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	out := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "tests" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(p); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range reScenarioBaseURLDefault.FindAllStringSubmatch(string(body), -1) {
			if strings.HasPrefix(m[2], "http://") || strings.HasPrefix(m[2], "https://") {
				out[m[1]] = true
			}
		}
		return nil
	})
	return out, err
}

// catalogSubset — the catalog entries whose prefix appears in want, in catalog
// order.
func catalogSubset(cat []upstreamArtifact, want map[string]bool) []upstreamArtifact {
	var out []upstreamArtifact
	for _, a := range cat {
		if want[a.varPrefix] {
			out = append(out, a)
		}
	}
	return out
}
