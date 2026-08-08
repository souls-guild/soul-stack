package harness

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// devProvisionScript builds the community.redis plugin for a dev stand the way
// plugin.go builds it for L3b. Two implementations of one thing, and only one of
// them is in the gate — so these guards hold the other one against it.
const devProvisionScript = "dev/provision.sh"

// devProvisionStamper is the seam provision.sh uses to append the schema trailer.
// Shell cannot write a trailer without restating the wire format, and a restatement
// is a second definition that goes on agreeing with the old model after sdk/schema
// has left it.
const devProvisionStamper = "dev/stamp-artifact.go"

// communityRedisProvisionFunc is the step itself — the shell function these guards
// read, and the line at the bottom of the script that runs it.
const communityRedisProvisionFunc = "provision_community_redis_plugin"

// TestDevProvisionReadsOnlyFilesThatExist — every file the community.redis step
// reads out of the plugin's source directory is a file that is there.
//
// This is the whole of NIM-516. NIM-377 deleted manifest.yaml and moved the
// plugin's disclosure into a trailer; dev/provision.sh went on requiring the
// deleted file and calling `fail` when it was missing, so `make dev-provision` died
// at step 9b on a clean checkout of the release and NOT ONE fresh stand came up.
// Four sessions owed a live-stand acceptance and none could raise one. Nothing in
// the tree connected the two facts — the script names its inputs in shell, which no
// test binary loads and no compiler reads.
//
// The check costs a stat per referenced file and would have failed on the NIM-377
// commit itself.
func TestDevProvisionReadsOnlyFilesThatExist(t *testing.T) {
	code := communityRedisProvisionStep(t)
	dir := filepath.Join(repoRoot(t), communityRedisPluginDir)

	for _, name := range pluginDirReferences(t, code) {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s reads %s/%s, which is not there: %v\n"+
				"\tthe step calls `fail` on it, so `make dev-provision` stops and no stand comes up at all — "+
				"fix the step to read what the plugin publishes now, do not soften the fail to a warn "+
				"(the plugin would then silently not arrive and the failure would move into a scenario run)",
				devProvisionScript, communityRedisPluginDir, name, err)
		}
	}

	// Named separately from the stat above because the message is the model, not the
	// missing file: manifest.yaml is not "absent for now", it is gone. The plugin's
	// contract is the generated document, and the git slot holds the artifact and
	// nothing else (ADR-065(g)). A copy reappearing here means both models are live
	// at once, which is how the last disagreement lasted a whole release.
	if strings.Contains(code, "manifest.yaml") {
		t.Errorf("%s still names manifest.yaml — NIM-377 replaced it with the generated %s, "+
			"and %s asserts it stays deleted",
			devProvisionScript, schema.SchemaFileName, communityRedisPluginDir)
	}
}

// TestDevProvisionPublishesTheSameArtifactAsTheFixture — the dev stand and the L3b
// fixture put the same thing in the git slot.
//
// They are separate implementations on purpose (one is bash on an operator's
// machine, one is Go under a test binary), and the resolver cannot tell them apart:
// it takes the single executable in dist/ and reads a trailer off it
// (ADR-026 F-fetch, without executing anything — at plugin.allow the artifact is
// not approved yet). A dev stand that differs from the fixture is a stand whose
// failures the gate cannot reproduce, and whose passes prove nothing about it.
// Every assertion below anchors on the command that performs the act, not on the
// path appearing somewhere in the step. The step's own `log` line names dist/ and
// the tag, so a plain substring match stays green while the write it describes is
// deleted — a guard that reports what the script SAYS it did.
func TestDevProvisionPublishesTheSameArtifactAsTheFixture(t *testing.T) {
	code := communityRedisProvisionStep(t)

	// (1) It stamps. An artifact with no trailer has no disclosure, plugingit fails
	// that entry closed, and the stand comes up looking healthy with the plugin
	// silently absent — the expensive failure, discovered inside a scenario run.
	stamps := regexp.MustCompile(`go run[^\n]*` + regexp.QuoteMeta(filepath.Base(devProvisionStamper)))
	if !stamps.MatchString(code) {
		t.Errorf("%s never runs %s: the artifact it commits carries no schema trailer, "+
			"so plugingit rejects the entry and community.redis never arrives on the stand",
			devProvisionScript, devProvisionStamper)
	}
	if _, err := os.Stat(filepath.Join(repoRoot(t), devProvisionStamper)); err != nil {
		t.Errorf("%s is gone (%v) — provisioning a stand fails at the stamping step", devProvisionStamper, err)
	}

	// (2) It publishes the document beside the artifact, as a real build does, for
	// soul-lint — which should not have to download a binary to check a destiny.
	// plugingit tolerates it there precisely because it is not executable
	// (TestResolveEntry_DistWithSchemaFileIsNotAmbiguous); the dev stand is where
	// that stops being a unit-test claim.
	publishes := regexp.MustCompile(`(?m)^[[:space:]]*(cp|install)[[:space:]][^\n]*dist/` +
		regexp.QuoteMeta(schema.SchemaFileName) + `"`)
	if !publishes.MatchString(code) {
		t.Errorf("%s does not copy the document to dist/%s next to the artifact — plugin.go does, "+
			"and the two slots must match",
			devProvisionScript, schema.SchemaFileName)
	}

	// (3) The artifact itself lands in dist/, anchored on the copy that puts it there.
	// The name alone is not evidence: `local bin="soul-mod-redis"` satisfies a substring
	// match on its own, so deleting the copy left every other assertion in this test
	// green while dist/ held nothing but the document — no executable, ErrArtifactNotFound,
	// a per-entry warning, and a stand that comes up healthy with community.redis absent.
	// That is the failure this whole file exists to make impossible.
	binVar := communityRedisBinaryVar(t, code)
	installs := regexp.MustCompile(`(?m)^[[:space:]]*(cp|install)[[:space:]][^\n]*dist/(` +
		regexp.QuoteMeta(communityRedisBinaryName) + `|\$\{` + binVar + `\})"`)
	if !installs.MatchString(code) {
		t.Errorf("%s never copies the built artifact into dist/ — the slot would hold the document and "+
			"no executable, plugingit fails that entry closed, and the stand comes up with community.redis missing",
			devProvisionScript)
	}

	// (4) And it stamps BEFORE it publishes. Both acts can be present and the artifact
	// still ship bare: stamp the temporary build, copy it to dist/, and the trailer
	// travels; reverse them and the copy in dist/ is the unstamped one while the stamped
	// binary is the one `rm -rf "${tmp}"` deletes. Presence is not order, and the reader
	// downstream sees only the result — no disclosure, entry closed, warning, absent.
	if stampAt, installAt := stamps.FindStringIndex(code), installs.FindStringIndex(code); stampAt != nil &&
		installAt != nil && stampAt[0] > installAt[0] {
		t.Errorf("%s stamps the artifact AFTER copying it into dist/ — the published copy is the unstamped "+
			"one, carries no schema trailer, and community.redis never arrives", devProvisionScript)
	}

	// (5) dist/ ends up holding exactly one executable, which is what the resolver
	// selects on (pluginhost.SingleArtifactIn). Zero and two are the same outcome —
	// ErrArtifactNotFound, a per-entry warning, a green stand with no plugin — and both
	// are one chmod away. The step asserts the recorded modes at provisioning time; this
	// holds the chmods themselves, so the two layers fail on different mistakes.
	artifact := `dist/(` + regexp.QuoteMeta(communityRedisBinaryName) + `|\$\{` + binVar + `\})"`
	for _, m := range []struct{ perm, path, why string }{
		{"0755", artifact, "the resolver takes the one EXECUTABLE in dist/, so a non-executable artifact is no artifact"},
		{"0644", regexp.QuoteMeta("dist/"+schema.SchemaFileName) + `"`, "an executable document makes TWO executables in dist/ and the resolver cannot tell which one is the artifact"},
	} {
		sets := regexp.MustCompile(`(?m)^[[:space:]]*(chmod[[:space:]]+` + m.perm + `[^\n]*` + m.path +
			`|install[^\n]*-m[[:space:]]+` + m.perm + `[^\n]*` + m.path + `)`)
		if !sets.MatchString(code) {
			t.Errorf("%s never sets %s on %s — %s", devProvisionScript, m.perm, m.path, m.why)
		}
	}

	// (6) Both producers build reproducibly, with the same flags. A Sigil grant is keyed
	// on the artifact's sha256: producers that disagree here build two different plugins,
	// and a repeat provision invalidates a grant the operator already issued.
	for _, flag := range communityRedisBuildFlags {
		if !strings.Contains(code, flag) {
			t.Errorf("%s does not pass %s to `go build` — plugin.go does (communityRedisBuildFlags), "+
				"so the same sources would give the stand and this fixture different bytes",
				devProvisionScript, flag)
		}
	}

	// (7) The tag is the half that matters at resolve time: dev/keeper.dev.yml asks for
	// it by name, and a tag that moved leaves the entry unresolvable.
	tags := regexp.MustCompile(`(?m)^[[:space:]]*git[^\n]*[[:space:]]tag[[:space:]][^\n]*` +
		regexp.QuoteMeta(CommunityRedisPluginRef))
	if !tags.MatchString(code) {
		t.Errorf("%s does not tag the snapshot %s — the catalog entry asks for that ref and would resolve to nothing",
			devProvisionScript, CommunityRedisPluginRef)
	}

	// (8) And the step is actually CALLED. Everything above reads the inside of a shell
	// function, and a function nobody invokes is a correct definition of nothing: the
	// repo never gets built, the catalog's file:// source resolves to a directory that
	// is not there (ErrSourceUnavailable), and the entry closes into the same warning.
	// Deleting one line at the bottom of the step is the cheapest way to reintroduce
	// NIM-516, and it is the one thing a guard reading only the body cannot see.
	calls := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(communityRedisProvisionFunc) + `[[:space:]]*$`)
	if !calls.MatchString(provisionScriptSource(t)) {
		t.Errorf("%s defines %s but never calls it — no plugin repo is built, the catalog's file:// source "+
			"does not exist, and community.redis silently never arrives",
			devProvisionScript, communityRedisProvisionFunc)
	}
}

// communityRedisProvisionStep returns the CODE of provision.sh's
// provision_community_redis_plugin function — its body with comment lines dropped.
//
// Scoped to the function because `src` is a local: provision_git_repo above it uses
// the same name for a different directory, and a guard reading the whole file would
// check the wrong paths. Comments are dropped because they are not acts: a comment
// saying the step no longer touches manifest.yaml would otherwise fail the guard
// that says exactly the same thing.
func communityRedisProvisionStep(t *testing.T) string {
	t.Helper()
	const opener = communityRedisProvisionFunc + "() {"
	lines := strings.Split(provisionScriptSource(t), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, opener) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s no longer defines %s — rename it here too rather than leaving these guards with nothing to hold",
			devProvisionScript, opener)
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "}" {
			continue
		}
		code := make([]string, 0, i-start)
		for _, line := range lines[start : i+1] {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				code = append(code, line)
			}
		}
		body := strings.Join(code, "\n")
		// Both ways this scan can pick the wrong `}` leave the guards green while they
		// read the wrong text, which is worse than a guard that fails: a heredoc closing
		// on its own line ends the body early and they check half a step, and swallowing
		// the next function makes them check a neighbour's code and call it this one's.
		if strings.Contains(body, "<<") {
			t.Fatalf("%s: %s now contains a heredoc — this extractor ends the body at the first `}` on "+
				"its own line and would read only part of the step; teach it the heredoc before adding one",
				devProvisionScript, opener)
		}
		if strings.Count(body, "() {") > 1 {
			t.Fatalf("%s: the body extracted for %s contains another function definition — the scan ran "+
				"past the end of the step, so these guards would be holding a neighbour's code",
				devProvisionScript, opener)
		}
		return body
	}
	t.Fatalf("%s: %s is not closed by a `}` on its own line", devProvisionScript, opener)
	return ""
}

// provisionScriptSource returns dev/provision.sh verbatim. Separate from the step
// extractor because one assertion is about the file rather than the function: whether
// the step is called at all.
func provisionScriptSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), devProvisionScript))
	if err != nil {
		t.Fatalf("read %s: %v", devProvisionScript, err)
	}
	return string(raw)
}

// communityRedisBinaryVar returns the name of the local holding the artifact's
// filename, so an assertion can anchor on the copy that writes it rather than on the
// name appearing anywhere in the step. Resolved, not assumed: renaming the local is
// allowed, renaming the artifact on one side only is not.
func communityRedisBinaryVar(t *testing.T, code string) string {
	t.Helper()
	assign := regexp.MustCompile(`local[[:space:]]+([A-Za-z_][A-Za-z0-9_]*)="` +
		regexp.QuoteMeta(communityRedisBinaryName) + `"`)
	m := assign.FindStringSubmatch(code)
	if m == nil {
		t.Fatalf("the community.redis step in %s no longer builds %q, which is the artifact this package "+
			"names — one of the two was renamed alone", devProvisionScript, communityRedisBinaryName)
	}
	return m[1]
}

// pluginDirReferences returns the paths the step reads out of the plugin's source
// directory, as `<file>` relative to it. It resolves the local variable holding
// that directory rather than assuming a name, and fails loudly on finding nothing:
// a guard that quietly matches zero paths is worse than no guard, because the tier
// stays green while checking nothing.
func pluginDirReferences(t *testing.T, code string) []string {
	t.Helper()
	// EXAMPLES is ${REPO_ROOT}/examples, so the shell literal is the tail of the
	// path this package names. Derived, so renaming the directory on one side only
	// is a failure here instead of a silent no-op.
	rel := strings.TrimPrefix(communityRedisPluginDir, "examples/")
	assign := regexp.MustCompile(`local[[:space:]]+([A-Za-z_][A-Za-z0-9_]*)="\$\{EXAMPLES\}/` + regexp.QuoteMeta(rel) + `"`)
	m := assign.FindStringSubmatch(code)
	if m == nil {
		t.Fatalf("the community.redis step in %s no longer takes its sources from ${EXAMPLES}/%s — "+
			"this package says that is where the plugin lives (%s)",
			devProvisionScript, rel, communityRedisPluginDir)
	}
	// Only literals can be stat'ed, and an indirect read is precisely the one this
	// guard would miss — NIM-516 was a read of a file that was not there.
	if strings.Contains(code, "${"+m[1]+"}/${") {
		t.Errorf("the community.redis step in %s builds a path under ${%s} out of another variable — "+
			"this guard can only check literal filenames, so inline it rather than leaving the read unheld",
			devProvisionScript, m[1])
	}
	uses := regexp.MustCompile(`\$\{` + m[1] + `\}/([A-Za-z0-9._/-]+)`)
	seen := map[string]bool{}
	for _, g := range uses.FindAllStringSubmatch(code, -1) {
		seen[g[1]] = true
	}
	if !seen[schema.SchemaFileName] {
		t.Errorf("the community.redis step in %s never reads %s — that document IS the module's contract "+
			"since NIM-377, and it is what gets stamped into the artifact and published beside it",
			devProvisionScript, schema.SchemaFileName)
	}
	if len(seen) == 0 {
		t.Fatalf("the community.redis step in %s reads no file out of ${%s} — either it stopped using the "+
			"plugin's sources or this guard stopped matching them", devProvisionScript, m[1])
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
