//go:build e2e_live

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// SoulModule plugin channel (NIM-32 S1, ADR-065(b)/(f)/(g)): helpers to deliver
// the `community.redis` plugin to the stand via the REGULAR path - build the
// binary -> per-test git repo in the layout the plugingit resolver expects ->
// `plugins.soul_modules[]` catalog (Config.SoulModules) -> Sigil-allow via the
// Operator API. No trust mechanism is invented: allow is a keeper-side seal
// (the Signer signs the binary from the cache_root slot on POST
// /v1/plugins/sigils), no seal artifact is needed in the plugin git repo.

// The fixture's identity - plugin directory, ref, alias, module, artifact
// filename - is in pluginfixture.go, untagged, so the docker-free guard can hold
// it against the artifact model without a stand.

// Build cache - once per process (go build of the plugin isn't fast; Go's
// build cache makes repeated processes cheap, but we don't rebuild within one run).
var (
	communityRedisBuildOnce sync.Once
	communityRedisBinPath   string
	communityRedisBuildErr  error
)

// BuildCommunityRedisPlugin builds soul-mod-community-redis (linux/amd64,
// per-process cache) and materializes a per-test git repo in the layout the
// plugingit resolver expects (ADR-026(g) F-fetch, parity with fixtureRepo in
// keeper/internal/plugingit/resolver_test.go): dist/ holding the stamped
// artifact and the published schema.json, one commit on main, tag
// [CommunityRedisPluginRef]. There is no manifest.yaml anywhere — NIM-377
// replaced it with the generated document, and ADR-065(g) says the slot holds
// the artifact and nothing else.
//
// Returns the file:// URL of the repo for Config.SoulModules[].Source - the
// file:// scheme works for plugingit under SOUL_STACK_ALLOW_FILE_REPOS=1, which
// NewStack already sets for keeper processes (stack.go::runKeeperInit/startKeeperRun).
func BuildCommunityRedisPlugin(t *testing.T) string {
	t.Helper()

	// Both of these stay OUTSIDE the declared region on purpose, and for the same
	// reason: they are statements about THIS REPOSITORY, not about the machine.
	// `go build` over our own community-redis plugin fails when an SDK change
	// stopped compiling against it; the document read fails when the plugin's
	// published schema moved or went away — which is exactly how NIM-377 broke
	// this gate, and it was reported as "the stand didn't come up" on all nine
	// tests (NIM-515). That is the one direction this mechanism must never be
	// wrong in (NIM-406).
	bin := buildCommunityRedisBinary(t)
	document := readCommunityRedisDocument(t)

	// From here on it is fixture plumbing — tempdirs, file copies, a throwaway
	// git repo — whose failures are facts about the machine. Tests call this
	// BEFORE NewStack, so without a declaration of its own those would read as
	// assertions.
	infraUp := false
	defer declareStandSetupFailure(t, t.Failed(), &infraUp)

	repoDir := filepath.Join(t.TempDir(), "soul-mod-community-redis-repo")
	distDir := filepath.Join(repoDir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatalf("BuildCommunityRedisPlugin: mkdir %s: %v", distDir, err)
	}

	binary, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("BuildCommunityRedisPlugin: read built binary: %v", err)
	}
	// Stamp the trailer the way `soul-mod stamp` does (sdk/cmd/soul-mod), by
	// appending the same bytes that go to schema.json. Keeper reads the document
	// by seeking from the end WITHOUT executing the artifact — at plugin.allow it
	// is not yet approved — so an unstamped binary has no readable disclosure and
	// every reader fails closed (shared/plugin).
	if err := os.WriteFile(filepath.Join(distDir, communityRedisBinaryName),
		schema.AppendTrailer(binary, document), 0o755); err != nil {
		t.Fatalf("BuildCommunityRedisPlugin: write dist artifact: %v", err)
	}
	// The published copy sits beside the artifact in a real build, for soul-lint,
	// which should not have to download a binary to check a destiny. It is not
	// executable, so it does not make dist/ ambiguous (plugingit
	// TestResolveEntry_DistWithSchemaFileIsNotAmbiguous) — having it here is what
	// proves that in live.
	if err := os.WriteFile(filepath.Join(distDir, schema.SchemaFileName), document, 0o644); err != nil {
		t.Fatalf("BuildCommunityRedisPlugin: write dist %s: %v", schema.SchemaFileName, err)
	}

	runGit(t, "", "init", "-q", "-b", "main", repoDir)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "community.redis plugin snapshot")
	// tag.gpgsign=false is scoped to this call (like commit.gpgsign above): a
	// global tag.gpgsign=true would turn the lightweight tag into annotated and
	// require a message.
	runGit(t, repoDir, "-c", "tag.gpgsign=false", "tag", CommunityRedisPluginRef)

	infraUp = true
	return "file://" + repoDir
}

// readCommunityRedisDocument returns the schema document the plugin publishes —
// the generated canonical JSON that replaced manifest.yaml in NIM-377. These are
// the bytes `soul-mod stamp` would derive by running the artifact's own `schema`
// subcommand, and the ones its `verify` requires the trailer and schema.json to
// agree on, so stamping the fresh build with them keeps the artifact
// self-consistent by construction.
//
// It is read from the plugin's source directory rather than regenerated, because
// the repository's copy is what `make lint` validates and what the plugin's own
// manifest_test.go checks against the implementation — the harness must deliver
// the document this repo publishes, not a second one it made up.
func readCommunityRedisDocument(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(repoRoot(t), communityRedisPluginDir, schema.SchemaFileName)
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readCommunityRedisDocument: %v", err)
	}
	// Canonicality is the property the signature depends on (ADR-026): the bytes
	// are hashed, and a reformatted copy is a different artifact. Checking it here
	// means a hand edit to the published document is a finding in this gate too,
	// not a confusing verify failure inside keeper three steps later.
	canonical, err := schema.IsCanonical(document)
	if err != nil || !canonical {
		t.Fatalf("readCommunityRedisDocument: %s is not canonical (%v) — regenerate it, do not hand-edit", path, err)
	}
	return document
}

// buildCommunityRedisBinary - `GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64
// go build` of the plugin sources (go.mod replace directives resolve inside the
// repo). Output goes outside the repo (MkdirTemp), path is cached per process.
func buildCommunityRedisBinary(t *testing.T) string {
	t.Helper()
	communityRedisBuildOnce.Do(func() {
		outDir, err := os.MkdirTemp("", "soul-mod-build-")
		if err != nil {
			communityRedisBuildErr = fmt.Errorf("mkdtemp: %w", err)
			return
		}
		out := filepath.Join(outDir, communityRedisBinaryName)
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = filepath.Join(repoRoot(t), communityRedisPluginDir)
		cmd.Env = append(os.Environ(),
			"GOWORK=off", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
		if output, err := cmd.CombinedOutput(); err != nil {
			communityRedisBuildErr = fmt.Errorf("go build %s: %w\nOUTPUT:\n%s",
				communityRedisPluginDir, err, output)
			return
		}
		communityRedisBinPath = out
	})
	if communityRedisBuildErr != nil {
		t.Fatalf("buildCommunityRedisBinary: %v", communityRedisBuildErr)
	}
	return communityRedisBinPath
}

// AllowSoulModule allows the artifact registered under `alias` on the identity
// (source, ref) via Operator API POST /v1/plugins/sigils (ADR-026 S4a, re-keyed
// by NIM-377): keeper reads the artifact from the `<cache_root>/<alias>/current/`
// slot, signs it with the Signer, and writes the allow entry to plugin_sigils.
// Returns the sha256 of the allowed binary from the 201 response.
//
// The body carries the two identities NIM-377 separated: `alias` is the
// registration (address level 1, and the slot to read), `source`+`ref` are what
// the operator asserts about the artifact in it. The artifact carries no
// self-name, so there is no third "plugin name" to send — the request schema is
// additionalProperties:false and a stale `{namespace, name}` body is rejected
// 400 before it reaches any of this.
//
// The slot must be materialized BEFORE this call - normally `keeper run` does
// this at startup (plugingit.ResolveCatalog over `plugins.soul_modules[]`), so
// it's enough to pass the entry in Config.SoulModules.
func (s *Stack) AllowSoulModule(t *testing.T, alias, source, ref string) string {
	t.Helper()
	c := s.opClient(t)
	resp, status, err := c.post(context.Background(), "/v1/plugins/sigils", map[string]any{
		"alias":  alias,
		"source": source,
		"ref":    ref,
	})
	if err != nil {
		t.Fatalf("AllowSoulModule %s (%s@%s): http: %v", alias, source, ref, err)
	}
	if status != http.StatusCreated {
		t.Fatalf("AllowSoulModule %s (%s@%s): status %d, body=%s", alias, source, ref, status, string(resp))
	}
	var out struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("AllowSoulModule %s (%s@%s): decode: %v (body=%s)", alias, source, ref, err, string(resp))
	}
	if out.SHA256 == "" {
		t.Fatalf("AllowSoulModule %s (%s@%s): empty sha256 in 201 body=%s", alias, source, ref, string(resp))
	}
	return out.SHA256
}

// PluginSigilItem - an items[] element of GET /v1/plugins/sigils (subset of
// PluginSigilView wire fields needed by the asserts). Keyed by (source, ref)
// with the alias as a plain column since NIM-377 — the pre-NIM-377
// {namespace, name} pair is not on the wire at all, and a struct still naming
// it would decode to two empty strings and match nothing.
type PluginSigilItem struct {
	Alias  string `json:"alias"`
	Source string `json:"source"`
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

// ListPluginSigils returns active Sigil allows via Operator API
// GET /v1/plugins/sigils.
func (s *Stack) ListPluginSigils(t *testing.T) []PluginSigilItem {
	t.Helper()
	c := s.opClient(t)
	resp, status, err := c.get(context.Background(), "/v1/plugins/sigils")
	if err != nil {
		t.Fatalf("ListPluginSigils: http: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("ListPluginSigils: status %d, body=%s", status, string(resp))
	}
	var out struct {
		Items []PluginSigilItem `json:"items"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("ListPluginSigils: decode: %v (body=%s)", err, string(resp))
	}
	return out.Items
}
