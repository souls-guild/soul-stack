package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// catalogSHA is a well-formed digest for the fixtures below. The rows are checked
// against `^[0-9a-f]{64}$`, so a short placeholder would fail for the wrong reason.
const catalogSHA = "1111111111111111111111111111111111111111111111111111111111111111"

// artifactCatalog renders a keeper.yml with one `kind: artifact` soul_modules entry
// built from the given body lines.
func artifactCatalog(entry string) string {
	return keeperBaseRequired + "plugins:\n  soul_modules:\n" + entry
}

// The shape the epic fixed (NIM-793): kind + base_url + a row per platform, each with
// its own digest. Parsed, and accepted by the schema phase.
func TestPluginsCatalog_ArtifactEntryParsed(t *testing.T) {
	src := artifactCatalog(`    - name: redis
      kind: artifact
      base_url: https://nexus.internal/plugins/redis
      ref: v1.4.0
      artifacts:
        - { os: linux, arch: amd64, path: redis_linux_amd64, sha256: "` + catalogSHA + `" }
        - { os: linux, arch: arm64, path: redis_linux_arm64, sha256: "` + catalogSHA + `" }
`)
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("a well-formed artifact entry must validate")
	}
	if cfg.Plugins == nil || len(cfg.Plugins.SoulModules) != 1 {
		t.Fatalf("SoulModules: want 1 entry, got %+v", cfg.Plugins)
	}
	e := cfg.Plugins.SoulModules[0]
	if e.ResolvedKind() != plugin.SourceKindArtifact {
		t.Errorf("ResolvedKind = %q, want artifact", e.ResolvedKind())
	}
	// The two catalog keys collapse into the one value the grant is signed on.
	if e.SourceURL() != "https://nexus.internal/plugins/redis" {
		t.Errorf("SourceURL = %q, want the base_url", e.SourceURL())
	}
	if len(e.Artifacts) != 2 || e.Artifacts[0].Arch != "amd64" || e.Artifacts[1].Arch != "arm64" {
		t.Fatalf("artifacts = %+v", e.Artifacts)
	}
}

// An entry with no `kind:` is a git entry, and everything written before NIM-793 keeps
// its meaning. This is the compatibility guarantee stated as a test rather than as a
// sentence in a comment.
func TestPluginsCatalog_OmittedKindIsGit(t *testing.T) {
	src := keeperBaseRequired + `plugins:
  soul_modules:
    - { name: redis, source: "https://example.com/soul-mod-redis.git", ref: "v1.2.0" }
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("a git entry with no `kind:` must keep validating")
	}
	e := cfg.Plugins.SoulModules[0]
	if e.ResolvedKind() != plugin.SourceKindGit {
		t.Errorf("ResolvedKind = %q, want git", e.ResolvedKind())
	}
	if e.SourceURL() != "https://example.com/soul-mod-redis.git" {
		t.Errorf("SourceURL = %q, want the source", e.SourceURL())
	}
}

// Every way an artifact entry can fail to describe a fetchable release. All errors, not
// warnings: this catalog decides which executable bytes a Keeper will fetch and sign
// for, so a malformed entry stops the config rather than being skipped at runtime with
// a line in a log nobody reads.
func TestPluginsCatalog_ArtifactEntryRejections(t *testing.T) {
	cases := map[string]struct {
		entry string
		code  string
	}{
		"no artifacts row at all": {
			entry: `    - { name: redis, kind: artifact, base_url: "https://n.internal/redis", ref: v1 }
`,
			code: "field_required",
		},
		"no base_url": {
			entry: `    - name: redis
      kind: artifact
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: p, sha256: "` + catalogSHA + `" }
`,
			code: "field_required",
		},
		// The fields of the other kind are refused rather than ignored: a `source`
		// sitting unread under an artifact entry is an operator believing the Keeper
		// resolves something it does not.
		"source under an artifact entry": {
			entry: `    - name: redis
      kind: artifact
      source: "https://example.com/soul-mod-redis.git"
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: p, sha256: "` + catalogSHA + `" }
`,
			code: "field_not_allowed",
		},
		"base_url under a git entry": {
			entry: `    - { name: redis, source: "https://example.com/r.git", base_url: "https://n.internal/redis", ref: v1 }
`,
			code: "field_not_allowed",
		},
		"artifacts under a git entry": {
			entry: `    - name: redis
      source: "https://example.com/r.git"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: p, sha256: "` + catalogSHA + `" }
`,
			code: "field_not_allowed",
		},
		// One platform, two rows: the approved bytes would depend on which row a
		// reader hit first.
		"duplicate (os, arch)": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: a, sha256: "` + catalogSHA + `" }
        - { os: linux, arch: amd64, path: b, sha256: "` + catalogSHA + `" }
`,
			code: "duplicate_entry",
		},
		"row missing arch": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, path: p, sha256: "` + catalogSHA + `" }
`,
			code: "field_required",
		},
		"digest is not 64 lower-hex": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: p, sha256: deadbeef }
`,
			code: "value_invalid",
		},
		// The path names a file inside the approved source; anything that leaves it is
		// an address the operator did not approve.
		"path escapes the base_url": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: "../../etc/shadow", sha256: "` + catalogSHA + `" }
`,
			code: "value_invalid",
		},
		"path is another address": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: "https://evil.example/x", sha256: "` + catalogSHA + `" }
`,
			code: "value_invalid",
		},
		// The path is SIGNED verbatim and separately CONCATENATED onto base_url, so
		// anything a URL parser reads differently from a path reader would sign one
		// address and fetch another. `p#v2` fetches `…/p`.
		"path carries a fragment": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: "p#v2", sha256: "` + catalogSHA + `" }
`,
			code: "value_invalid",
		},
		"path carries a query": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: "p?raw=1", sha256: "` + catalogSHA + `" }
`,
			code: "value_invalid",
		},
		// Percent-encoding is refused rather than decoded: decoding would put a second
		// URL parser in the trust path, and `%2e%2e%2f` is traversal the segment check
		// never sees.
		"path is percent-encoded": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: "a%2e%2e%2fb", sha256: "` + catalogSHA + `" }
`,
			code: "value_invalid",
		},
		// Some servers normalize a backslash to `/`, which would smuggle a segment
		// past the `..` check.
		"path carries a backslash": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: "..\\redis", sha256: "` + catalogSHA + `" }
`,
			code: "value_invalid",
		},
		"platform token is a path segment": {
			entry: `    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: "..", arch: amd64, path: p, sha256: "` + catalogSHA + `" }
`,
			code: "value_invalid",
		},
		"unknown kind": {
			entry: `    - { name: redis, kind: torrent, ref: v1 }
`,
			code: "enum_invalid",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(artifactCatalog(tc.entry)), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadKeeperFromBytes: %v", err)
			}
			if !diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatal("the entry validated — a catalog that cannot be resolved must not load")
			}
			if !hasCode(diags, tc.code) {
				dump(t, diags)
				t.Fatalf("no diagnostic with code %q", tc.code)
			}
		})
	}
}

// The yaml path in the message points at the offending row, not at the block. An
// operator with fifteen platforms in a catalog reads the path, not the prose.
func TestPluginsCatalog_ArtifactDiagnosticNamesTheRow(t *testing.T) {
	src := artifactCatalog(`    - name: redis
      kind: artifact
      base_url: "https://n.internal/redis"
      ref: v1
      artifacts:
        - { os: linux, arch: amd64, path: p, sha256: "` + catalogSHA + `" }
        - { os: linux, arch: arm64, path: p, sha256: nothex }
`)
	_, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	var found bool
	for _, d := range diags {
		if strings.Contains(d.Message, "nothex") {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatal("no diagnostic mentions the offending digest")
	}
}
