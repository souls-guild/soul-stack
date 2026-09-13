//go:build e2e_live

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// SoulModule plugin channel (NIM-32 S1, ADR-065(b)/(f)/(g)): helpers to deliver
// the `redis` plugin to the stand via the REGULAR path - build the
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
	redisBuildOnce sync.Once
	redisBinPath   string
	redisDocPath   string
	redisBuildErr  error
)

// BuildRedisPlugin builds redis (linux/amd64,
// per-process cache) and materializes a per-test git repo in the layout the
// plugingit resolver expects (ADR-026(g) F-fetch, parity with fixtureRepo in
// keeper/internal/plugingit/resolver_test.go): dist/ holding the stamped
// artifact and the published schema.json, one commit on main, tag
// [RedisPluginRef]. There is no manifest.yaml anywhere — NIM-377
// replaced it with the generated document, and ADR-065(g) says the slot holds
// the artifact and nothing else.
//
// Returns the file:// URL of the repo for Config.SoulModules[].Source - the
// file:// scheme works for plugingit under SOUL_STACK_ALLOW_FILE_REPOS=1, which
// NewStack already sets for keeper processes (stack.go::runKeeperInit/startKeeperRun).
func BuildRedisPlugin(t *testing.T) string {
	t.Helper()

	// Both of these stay OUTSIDE the declared region on purpose, and for the same
	// reason: they are statements about THIS REPOSITORY, not about the machine.
	// `go build` over our own redis plugin fails when an SDK change
	// stopped compiling against it; the document read fails when the plugin's
	// published schema moved or went away — which is exactly how NIM-377 broke
	// this gate, and it was reported as "the stand didn't come up" on all nine
	// tests (NIM-515). That is the one direction this mechanism must never be
	// wrong in (NIM-406).
	bin, doc := buildRedisArtifact(t)
	assertStampMatchesPublished(t, doc)

	// From here on it is fixture plumbing — tempdirs, file copies, a throwaway
	// git repo — whose failures are facts about the machine. Tests call this
	// BEFORE NewStack, so without a declaration of its own those would read as
	// assertions.
	infraUp := false
	defer declareStandSetupFailure(t, t.Failed(), &infraUp)

	repoDir := filepath.Join(t.TempDir(), "redis-repo")
	distDir := filepath.Join(repoDir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatalf("BuildRedisPlugin: mkdir %s: %v", distDir, err)
	}

	// dist/ holds the stamped artifact and, beside it, the published schema.json —
	// exactly what the stamp wrote in the build dir. Both are copies: the stamping
	// itself happened above, outside the region.
	if err := copyFileMode(filepath.Join(distDir, redisBinaryName), bin, 0o755); err != nil {
		t.Fatalf("BuildRedisPlugin: write dist artifact: %v", err)
	}
	// It is not executable, so it does not make dist/ ambiguous (plugingit
	// TestResolveEntry_DistWithSchemaFileIsNotAmbiguous) — having it here is what
	// proves that in live.
	if err := copyFileMode(filepath.Join(distDir, schema.SchemaFileName), doc, 0o644); err != nil {
		t.Fatalf("BuildRedisPlugin: write dist %s: %v", schema.SchemaFileName, err)
	}

	runGit(t, "", "init", "-q", "-b", "main", repoDir)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "redis plugin snapshot")
	// tag.gpgsign=false is scoped to this call (like commit.gpgsign above): a
	// global tag.gpgsign=true would turn the lightweight tag into annotated and
	// require a message.
	runGit(t, repoDir, "-c", "tag.gpgsign=false", "tag", RedisPluginRef)

	infraUp = true
	return "file://" + repoDir
}

// readRedisDocument returns the schema document the plugin PUBLISHES in this
// repository — the generated canonical JSON that replaced manifest.yaml in
// NIM-377. It is the expected value the stamp above is checked against, not the
// value delivered: since NIM-525 the delivered one is derived by `soul-mod stamp`
// from the freshly built artifact, and the two agreeing is the property worth
// asserting.
//
// The repository's copy is what `make lint` validates and what the plugin's own
// manifest_test.go checks against the implementation, so a disagreement here means
// the committed document is stale — which soul-lint would then check the whole
// corpus against.
func readRedisDocument(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(repoRoot(t), redisPluginDir, schema.SchemaFileName)
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readRedisDocument: %v", err)
	}
	// Refuse what keeper would refuse, in the order soul-mod's derivedDocument does:
	// parse, validate, then canonical form. Validity is not implied by canonicality —
	// a document can be perfectly formed bytes and still name a kind that does not
	// exist — and keeper answers an invalid one with ErrSchemaUnreadable, which
	// ResolveCatalog demotes to a per-entry warning. The stand then comes up green
	// with the plugin silently absent, which is the failure this fixture exists to
	// reproduce, not to inherit. dev/stamp-artifact.go checks the same three things.
	doc, err := schema.Unmarshal(document)
	if err != nil {
		t.Fatalf("readRedisDocument: %s: %v", path, err)
	}
	if issues := schema.Validate(doc); schema.HasErrors(issues) {
		for _, i := range issues {
			if i.Level == schema.LevelError {
				t.Errorf("readRedisDocument: %s: %s %s: %s", path, i.Path, i.Code, i.Message)
			}
		}
		t.Fatalf("readRedisDocument: %s is invalid — regenerate it from the Go definition", path)
	}
	canonical, err := schema.IsCanonical(document)
	if err != nil {
		t.Fatalf("readRedisDocument: %s: %v", path, err)
	}
	if !canonical {
		t.Fatalf("readRedisDocument: %s is not in canonical form — regenerate it, do not hand-edit", path)
	}
	return document
}

// assertStampMatchesPublished — what `soul-mod stamp` derived from the fresh build
// must be the document this repository publishes. A disagreement is the NIM-525
// failure in its other direction: soul-lint checks the whole corpus against the
// committed schema.json, so a stale one means the corpus is validated against a
// contract the delivered binary no longer implements, and live stays green.
//
// Like the build and the document read, this stays OUTSIDE the declared bring-up
// region: it is a statement about THIS REPOSITORY, not about the machine.
func assertStampMatchesPublished(t *testing.T, docPath string) {
	t.Helper()
	stamped, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read the document `soul-mod stamp` wrote: %v", err)
	}
	published := readRedisDocument(t)
	if !bytes.Equal(stamped, published) {
		t.Fatalf("`soul-mod stamp` derived a document the repository does not publish "+
			"(%d bytes stamped, %d bytes in %s/%s) — rebuild the artifact, re-run `soul-mod stamp` on it, and commit the result",
			len(stamped), len(published), redisPluginDir, schema.SchemaFileName)
	}
}

// copyFileMode writes src's bytes to dst with the given mode.
func copyFileMode(dst, src string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}

// buildRedisArtifact builds the plugin, runs the plugin author's own build-time
// tool over it, and returns the paths of the stamped artifact the stand will be
// given and of the schema.json that tool derived. Cached per process — none of it
// is fast, and all of it is deterministic.
//
// It builds the plugin TWICE, from the same sources, and the reason is the one
// thing worth reading here:
//
//   - a NATIVE build is what `soul-mod stamp` and `soul-mod verify` run against,
//     because stamp derives the document by EXECUTING the artifact's own `schema`
//     subcommand — the artifact is the source of truth about itself. Running the
//     real tool is the point of NIM-525: reproducing its trailer format here left
//     the author's build path as the one thing live never touched. It must be the
//     native build, or this fixture would silently acquire a host-platform
//     requirement it never had — a linux/amd64 binary does not exec on a darwin
//     host, and the failure would land at bring-up.
//   - a linux/amd64 build is what the STAND runs, inside a container. It gets the
//     document the stamp just derived, written with the same `sdk/schema` trailer
//     writer `soul-mod stamp` itself calls — not a second implementation of the
//     format, and not a second document.
//
// Keeper, by contrast, reads that trailer by seeking from the end WITHOUT
// executing anything, because at plugin.allow the binary is not approved yet — an
// unstamped artifact has no readable disclosure and every reader fails closed
// (shared/plugin).
//
// Every half is a statement about THIS REPOSITORY, which is why callers invoke
// this BEFORE their declared bring-up region.
func buildRedisArtifact(t *testing.T) (artifact, document string) {
	t.Helper()
	redisBuildOnce.Do(func() {
		outDir, err := os.MkdirTemp("", "build-")
		if err != nil {
			redisBuildErr = fmt.Errorf("mkdtemp: %w", err)
			return
		}

		// The native half: build, stamp, verify. `soul-mod verify` re-derives the
		// document and compares it against both the trailer and the schema.json
		// beside it, so a green verify is the author's own CI gate passing here.
		native := filepath.Join(outDir, redisBinaryName+"-native")
		if err := goBuildPlugin(t, native); err != nil {
			redisBuildErr = err
			return
		}
		for _, verb := range []string{"stamp", "verify"} {
			// GOWORK= (empty, not `off`): soul-mod lives in the sdk module and
			// resolves through the workspace, and an operator's exported
			// GOWORK=off would hide it.
			cmd := exec.Command("go", "run", "./sdk/cmd/soul-mod", verb, native)
			cmd.Dir = repoRoot(t)
			cmd.Env = append(os.Environ(), "GOWORK=")
			if output, err := cmd.CombinedOutput(); err != nil {
				redisBuildErr = fmt.Errorf("soul-mod %s %s: %w\nOUTPUT:\n%s", verb, native, err, output)
				return
			}
		}
		docPath := filepath.Join(outDir, schema.SchemaFileName)
		doc, err := os.ReadFile(docPath)
		if err != nil {
			redisBuildErr = fmt.Errorf("read the document soul-mod stamp wrote: %w", err)
			return
		}

		// The delivered half: the artifact the container will exec, carrying the
		// very bytes the stamp derived.
		out := filepath.Join(outDir, redisBinaryName)
		if err := goBuildPlugin(t, out, "GOOS=linux", "GOARCH=amd64"); err != nil {
			redisBuildErr = err
			return
		}
		if err := schema.WriteTrailerFile(out, doc); err != nil {
			redisBuildErr = fmt.Errorf("write the trailer onto the linux artifact: %w", err)
			return
		}

		redisBinPath, redisDocPath = out, docPath
	})
	if redisBuildErr != nil {
		t.Fatalf("buildRedisArtifact: %v", redisBuildErr)
	}
	return redisBinPath, redisDocPath
}

// goBuildPlugin — `GOWORK=off CGO_ENABLED=0 go build` of the plugin sources (its
// go.mod replace directives resolve inside the repo), with redisBuildFlags for a
// reproducible artifact. Output goes outside the repo.
func goBuildPlugin(t *testing.T, out string, env ...string) error {
	t.Helper()
	args := append([]string{"build"}, redisBuildFlags...)
	cmd := exec.Command("go", append(args, "-o", out, ".")...)
	cmd.Dir = filepath.Join(repoRoot(t), redisPluginDir)
	cmd.Env = append(append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0"), env...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s (%v): %w\nOUTPUT:\n%s", redisPluginDir, env, err, output)
	}
	return nil
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
// this at startup (pluginsource.Catalog over `plugins.soul_modules[]`), so it's
// enough to pass the entry in Config.SoulModules.
//
// Returns the digest of the artifact approved for THIS platform. The 201 body
// describes a release (NIM-793), so the reply carries every approved file; a
// live test asserting "the bytes the Soul will install" wants the row that
// applies to the host it runs on, and a release with no such row is a failure
// here rather than a nil later.
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
		Artifacts []PluginSigilArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("AllowSoulModule %s (%s@%s): decode: %v (body=%s)", alias, source, ref, err, string(resp))
	}
	sha := SelectArtifactSHA(out.Artifacts)
	if sha == "" {
		t.Fatalf("AllowSoulModule %s (%s@%s): the approved release covers no artifact for %s/%s; body=%s",
			alias, source, ref, runtime.GOOS, runtime.GOARCH, string(resp))
	}
	return sha
}

// PluginSigilArtifact - one approved file of a release on the wire.
type PluginSigilArtifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// SelectArtifactSHA picks this host's digest out of a release, mirroring
// shared/pluginhost.SelectArtifact: the exact platform, or the single
// unplatformed row a kind=git grant carries. "" = the release does not cover
// this platform.
func SelectArtifactSHA(artifacts []PluginSigilArtifact) string {
	var fallback string
	for _, a := range artifacts {
		if a.OS == runtime.GOOS && a.Arch == runtime.GOARCH {
			return a.SHA256
		}
		if a.OS == "" && a.Arch == "" {
			fallback = a.SHA256
		}
	}
	return fallback
}

// PluginSigilItem - an items[] element of GET /v1/plugins/sigils (subset of
// PluginSigilView wire fields needed by the asserts). Keyed by (source, ref)
// with the alias as a plain column since NIM-377 — the pre-NIM-377
// {namespace, name} pair is not on the wire at all, and a struct still naming
// it would decode to two empty strings and match nothing.
type PluginSigilItem struct {
	Alias     string                `json:"alias"`
	Source    string                `json:"source"`
	Ref       string                `json:"ref"`
	Kind      string                `json:"kind"`
	Artifacts []PluginSigilArtifact `json:"artifacts"`
}

// SHA256 is this host's approved digest in the item, or "" when the release
// covers no artifact for it.
func (i PluginSigilItem) SHA256() string { return SelectArtifactSHA(i.Artifacts) }

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
