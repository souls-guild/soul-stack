package harness

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The mirror's own guards, docker-free and network-free: the catalog here is
// synthetic and its "tarballs" are a few bytes each, because what is under test
// is the cache-and-serve mechanism, not the real releases.

func fakeArtifact(t *testing.T, dir, prefix, version, body string) upstreamArtifact {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	a := upstreamArtifact{
		varPrefix:    prefix,
		upstreamBase: "https://example.invalid/" + prefix + "/releases/download",
		version:      version,
		file:         prefix + "-" + version + ".tar.gz",
		sha256:       hex.EncodeToString(sum[:]),
	}
	if dir != "" {
		dst := filepath.Join(dir, filepath.FromSlash(a.cacheRel()))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
	}
	return a
}

func getMirror(t *testing.T, m *artifactMirror, rel string) (int, string) {
	t.Helper()
	resp, err := m.client(10 * time.Second).Get(m.baseURL + "/" + rel)
	if err != nil {
		t.Fatalf("GET %s/%s: %v", m.baseURL, rel, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestArtifactMirrorServesTheCacheUnderTheUpstreamLayout — the destiny builds
// `<base_url>/v<version>/<file>` and the mirror answers at exactly that path with
// exactly those bytes.
//
// The path shape is the whole trick: the fixture only has to override base_url
// because everything after it is the upstream layout, verbatim. If the mirror
// served a flattened or re-rooted tree the override would 404 and the failure
// would surface inside the container, twenty minutes in.
func TestArtifactMirrorServesTheCacheUnderTheUpstreamLayout(t *testing.T) {
	dir := t.TempDir()
	cat := []upstreamArtifact{
		fakeArtifact(t, dir, "node_exporter", "1.8.2", "node-exporter-bytes"),
		fakeArtifact(t, dir, "vector", "0.40.0", "vector-bytes"),
	}

	m, err := startArtifactMirror(dir, "127.0.0.1")
	if err != nil {
		t.Fatalf("start mirror: %v", err)
	}
	t.Cleanup(m.close)

	for i, a := range cat {
		// The URL the destiny actually builds, assembled the way the destiny
		// assembles it, from the base_url the overlay hands it.
		base := m.baseURL + "/" + a.varPrefix
		rel := strings.TrimPrefix(base+"/v"+a.version+"/"+a.file, m.baseURL+"/")

		code, body := getMirror(t, m, rel)
		if code != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200 (the destiny's URL shape and the cache layout disagree)", rel, code)
		}
		if want := []string{"node-exporter-bytes", "vector-bytes"}[i]; body != want {
			t.Errorf("GET %s served %q, want %q", rel, body, want)
		}
		if got := m.hitCount(a.cacheRel()); got != 1 {
			t.Errorf("hitCount(%s) = %d after one request, want 1", a.cacheRel(), got)
		}
	}
}

// TestMissingArtifactsNamesWhatWasNeverFetched — the counter that makes the
// override falsifiable.
//
// This is the guard the whole hermetization leans on. Everything else asserts
// that the fixture WROTE a redirect; only this one notices when the redirect was
// written and then ignored — the overlay out-sorted, silenced by a `_stack.yaml`,
// or reading a var name the scenario no longer uses. In every one of those the
// create still passes, from github, and the gate is quietly back to depending on
// a network outside its slice.
func TestMissingArtifactsNamesWhatWasNeverFetched(t *testing.T) {
	dir := t.TempDir()
	cat := []upstreamArtifact{
		fakeArtifact(t, dir, "node_exporter", "1.8.2", "a"),
		fakeArtifact(t, dir, "redis_exporter", "1.62.0", "b"),
		fakeArtifact(t, dir, "vector", "0.40.0", "c"),
	}

	m, err := startArtifactMirror(dir, "127.0.0.1")
	if err != nil {
		t.Fatalf("start mirror: %v", err)
	}
	t.Cleanup(m.close)

	// Nothing fetched yet: all three named, and the message says why it matters.
	msg := missingArtifacts(m, cat)
	for _, a := range cat {
		if !strings.Contains(msg, a.cacheRel()) {
			t.Errorf("nothing served, and the report does not name %s:\n%s", a.cacheRel(), msg)
		}
	}
	if !strings.Contains(msg, "github.com") {
		t.Errorf("the report does not say where the run fetched from instead:\n%s", msg)
	}

	// Two of three: the accusation must name the third and only the third. What
	// WAS served is printed too, further down, and deliberately — "nothing was
	// served" and "something else was served" want different reactions — but the
	// two must not be confusable, or the reader goes looking at the wrong var.
	getMirror(t, m, cat[0].cacheRel())
	getMirror(t, m, cat[2].cacheRel())
	msg = missingArtifacts(m, cat)
	accusation, evidence, ok := strings.Cut(msg, "It served:")
	if !ok {
		t.Fatalf("the report no longer separates what is missing from what was served:\n%s", msg)
	}
	if !strings.Contains(accusation, cat[1].cacheRel()) {
		t.Errorf("redis_exporter was never served and the report does not accuse it:\n%s", msg)
	}
	if strings.Contains(accusation, cat[0].cacheRel()) || strings.Contains(accusation, cat[2].cacheRel()) {
		t.Errorf("the report accuses artifacts that WERE served:\n%s", msg)
	}
	if !strings.Contains(evidence, cat[0].cacheRel()) || !strings.Contains(evidence, cat[2].cacheRel()) {
		t.Errorf("the report does not show what the mirror DID serve:\n%s", msg)
	}

	// All three: silence.
	getMirror(t, m, cat[1].cacheRel())
	if msg := missingArtifacts(m, cat); msg != "" {
		t.Errorf("everything was served and the report still complains:\n%s", msg)
	}
}

// TestArtifactMirrorCountsRequestsItCouldNotServe — a 404 counts as a hit.
//
// Deliberate, and the deliberate part is worth stating: the counter answers "did
// the product ask US", not "did we have it". Counting only successes would turn a
// version-drift 404 into the same report as an ignored overlay, and those want
// opposite reactions — one is a stale catalog, the other is a gate back on the
// public internet.
func TestArtifactMirrorCountsRequestsItCouldNotServe(t *testing.T) {
	dir := t.TempDir()
	a := fakeArtifact(t, "", "vector", "0.40.0", "unused") // not written to dir

	m, err := startArtifactMirror(dir, "127.0.0.1")
	if err != nil {
		t.Fatalf("start mirror: %v", err)
	}
	t.Cleanup(m.close)

	if code, _ := getMirror(t, m, a.cacheRel()); code != http.StatusNotFound {
		t.Fatalf("GET a path with no file: status %d, want 404", code)
	}
	if got := m.hitCount(a.cacheRel()); got != 1 {
		t.Errorf("hitCount after a 404 = %d, want 1: the product DID ask the mirror, and a "+
			"missing cache entry must read as version drift, not as an ignored override", got)
	}
	if msg := missingArtifacts(m, []upstreamArtifact{a}); msg != "" {
		t.Errorf("the artifact was requested (and 404'd); missingArtifacts should stay quiet:\n%s", msg)
	}
}

// TestArtifactCachedRejectsAndRemovesWrongBytes — a corrupt entry is not reused.
//
// An interrupted download leaves a short file, and a short tarball fails INSIDE
// the container, on a create, printed as `--- FAIL` against the product — as a
// checksum error for the two destinies that declare one, as a broken unpack for
// node-exporter, which deliberately declares none. Removing it here means the
// next run re-fetches instead of failing the same way forever for a reason it
// cannot see.
func TestArtifactCachedRejectsAndRemovesWrongBytes(t *testing.T) {
	dir := t.TempDir()
	a := fakeArtifact(t, dir, "vector", "0.40.0", "the-right-bytes")
	path := filepath.Join(dir, filepath.FromSlash(a.cacheRel()))

	switch ok, err := artifactCached(path, a.sha256); {
	case err != nil:
		t.Fatalf("artifactCached on good bytes: %v", err)
	case !ok:
		t.Fatalf("artifactCached on good bytes = false")
	}

	if err := os.WriteFile(path, []byte("truncated"), 0o644); err != nil {
		t.Fatalf("corrupt the entry: %v", err)
	}
	switch ok, err := artifactCached(path, a.sha256); {
	case err != nil:
		t.Fatalf("artifactCached on bad bytes: %v", err)
	case ok:
		t.Fatalf("artifactCached accepted bytes with the wrong digest")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the corrupt entry survived (%v); the next run would fail on it again", err)
	}

	// Absent is not an error — it is the ordinary cold-cache case.
	switch ok, err := artifactCached(filepath.Join(dir, "nope.tar.gz"), a.sha256); {
	case err != nil:
		t.Errorf("artifactCached on a missing file returned an error: %v", err)
	case ok:
		t.Errorf("artifactCached on a missing file = true")
	}
}

// TestEnsureArtifactCacheOfflineNamesTheWayOut — with the cache cold and fetching
// forbidden, the failure says what to run.
//
// SOUL_STACK_E2E_ARTIFACT_OFFLINE=1 is how "this gate needs nothing from
// github.com" is demonstrated rather than asserted, so its failure mode is part
// of the feature: a cold cache under it must not read as a broken fixture.
func TestEnsureArtifactCacheOfflineNamesTheWayOut(t *testing.T) {
	dir := t.TempDir()
	a := fakeArtifact(t, "", "vector", "0.40.0", "unused")

	t.Setenv(artifactOfflineEnv, "1")
	err := ensureArtifactCache(dir, []upstreamArtifact{a})
	if err == nil {
		t.Fatalf("cold cache with fetching forbidden: no error")
	}
	for _, want := range []string{a.cacheRel(), artifactOfflineEnv, "make e2e-live-artifacts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the offline failure does not mention %q:\n%v", want, err)
		}
	}

	// Warm cache, still offline: no error and no fetch. This is the state a
	// hermetic gate run is actually in.
	warm := fakeArtifact(t, dir, "vector", "0.40.0", "warm-bytes")
	if err := ensureArtifactCache(dir, []upstreamArtifact{warm}); err != nil {
		t.Errorf("warm cache under %s=1 still failed: %v", artifactOfflineEnv, err)
	}
}

// TestArtifactCacheDirIsStableAndOutsideTheRepo — the cache survives the things
// that routinely wipe a working tree.
func TestArtifactCacheDirIsStableAndOutsideTheRepo(t *testing.T) {
	t.Setenv(artifactCacheEnv, "/var/cache/soul-stack-e2e")
	if got, err := artifactCacheDir(); err != nil || got != "/var/cache/soul-stack-e2e" {
		t.Fatalf("artifactCacheDir() = %q, %v; want the env override", got, err)
	}

	t.Setenv(artifactCacheEnv, "")
	t.Setenv("XDG_CACHE_HOME", "/home/somebody/.cache")
	got, err := artifactCacheDir()
	if err != nil {
		t.Fatalf("artifactCacheDir(): %v", err)
	}
	if want := filepath.Join("/home/somebody/.cache", "soul-stack", "e2e-live", "artifacts"); got != want {
		t.Errorf("artifactCacheDir() = %q, want %q", got, want)
	}
	// Not under the repository and not under $TMPDIR: `git clean -xfd` between
	// runs, or a per-run temp dir, would turn "downloads once per machine" back
	// into "downloads every run" — the dependency this ticket removes.
	if strings.HasPrefix(got, repoRoot(t)+string(filepath.Separator)) {
		t.Errorf("the cache lives inside the repository (%s); `git clean -xfd` would empty it", got)
	}
	if tmp := os.TempDir(); tmp != "" && strings.HasPrefix(got, tmp+string(filepath.Separator)) {
		t.Errorf("the cache lives under %s and would not survive a reboot", tmp)
	}
}

// TestArtifactOfflineReadsTheUsualSpellings — the env var is set by hand, in
// shells, by people who write `true`.
func TestArtifactOfflineReadsTheUsualSpellings(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {" 1 ", true},
		{"", false}, {"0", false}, {"false", false}, {"no", false},
	} {
		t.Setenv(artifactOfflineEnv, tc.value)
		if got := artifactOffline(); got != tc.want {
			t.Errorf("artifactOffline() with %s=%q = %v, want %v", artifactOfflineEnv, tc.value, got, tc.want)
		}
	}
}

// TestArtifactMirrorAdvertisesTheHostTheContainerCanReach — the base_url handed
// to the soul is not the harness's own view of the socket.
//
// The mirror listens on the host; the fetch happens inside the soul container.
// `127.0.0.1` there is the container, and a mirror advertised that way is a
// connection refused that reads like a fixture bug. keeperEndpointHost() is the
// address the container already dials for keeper, and it is the same answer here.
func TestArtifactMirrorAdvertisesTheHostTheContainerCanReach(t *testing.T) {
	m, err := startArtifactMirror(t.TempDir(), "host.docker.internal")
	if err != nil {
		t.Fatalf("start mirror: %v", err)
	}
	t.Cleanup(m.close)

	if !strings.HasPrefix(m.baseURL, "https://host.docker.internal:") {
		t.Fatalf("baseURL = %q, want it advertised at the host the container reaches", m.baseURL)
	}
	if strings.HasSuffix(m.baseURL, ":0") {
		t.Fatalf("baseURL = %q: the listener's port was never resolved", m.baseURL)
	}
	if strings.HasSuffix(m.baseURL, "/") {
		t.Fatalf("baseURL = %q ends in a slash; the overlay appends its own", m.baseURL)
	}
}

// TestTwoArtifactMirrorsCoexist — two gate runs at once do not fight over a port.
//
// The Makefile says this repo is worked in several worktrees at a time, and a
// fixed port would make the second gate fail on bind — or, worse, silently serve
// the first one's cache.
func TestTwoArtifactMirrorsCoexist(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	a := fakeArtifact(t, dirA, "vector", "0.40.0", "from-A")
	fakeArtifact(t, dirB, "vector", "0.40.0", "from-B")

	mA, err := startArtifactMirror(dirA, "127.0.0.1")
	if err != nil {
		t.Fatalf("start mirror A: %v", err)
	}
	t.Cleanup(mA.close)
	mB, err := startArtifactMirror(dirB, "127.0.0.1")
	if err != nil {
		t.Fatalf("start mirror B: %v (a second concurrent gate run cannot start)", err)
	}
	t.Cleanup(mB.close)

	if mA.baseURL == mB.baseURL {
		t.Fatalf("both mirrors advertise %s", mA.baseURL)
	}
	if _, body := getMirror(t, mA, a.cacheRel()); body != "from-A" {
		t.Errorf("mirror A served %q", body)
	}
	if _, body := getMirror(t, mB, a.cacheRel()); body != "from-B" {
		t.Errorf("mirror B served %q, want its own cache", body)
	}
}

// TestProbeUpstreamArtifactsNamesTheEnvironment — an unreachable upstream is
// reported as the environment, by host, before anything of the product runs.
//
// This is what makes the real-path test's failure legible (NIM-542 acceptance
// (c)). The probe runs BEFORE the stand deliberately: once an apply has failed,
// "github went away" and "the code broke" arrive through the same channel and the
// reader has to guess. Here nothing has run yet, so the answer is not a judgement
// call — and the message has to say so out loud, or a reader who sees SKIP will
// still go looking for a bug.
//
// No network: "upstream" is a local mirror for the reachable case and a closed
// port for the unreachable one.
func TestProbeUpstreamArtifactsNamesTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	up, err := startArtifactMirror(dir, "127.0.0.1")
	if err != nil {
		t.Fatalf("start stand-in upstream: %v", err)
	}
	t.Cleanup(up.close)

	live := fakeArtifact(t, dir, "vector", "0.40.0", "bytes")
	live.upstreamBase = up.baseURL + "/vector"
	probe := up.client(10 * time.Second)
	if msg := probeUpstreamArtifactsWith(probe, []upstreamArtifact{live}); msg != "" {
		t.Fatalf("a reachable upstream was reported as unreachable:\n%s", msg)
	}

	// A file the "upstream" does not have: reachable host, missing release. Still
	// not something to run a stand against.
	absent := fakeArtifact(t, "", "vector", "9.9.9", "")
	absent.upstreamBase = up.baseURL + "/vector"
	if msg := probeUpstreamArtifactsWith(probe, []upstreamArtifact{absent}); !strings.Contains(msg, "status 404") {
		t.Errorf("a 404 upstream was not reported as unreachable:\n%s", msg)
	}

	// Nothing listening: the ordinary "no outbound access" case.
	dead := fakeArtifact(t, "", "vector", "0.40.0", "")
	dead.upstreamBase = "https://127.0.0.1:1/vector"
	msg := probeUpstreamArtifactsWith(probe, []upstreamArtifact{dead})
	if msg == "" {
		t.Fatalf("a closed port was reported as reachable")
	}
	for _, want := range []string{
		dead.upstreamURL(),                   // which host, exactly
		"environment, not a defect",          // how to read it
		"nothing of this repository has run", // why that reading is safe
		"does not depend on it",              // and why the release is not blocked
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the unreachable-upstream report does not say %q:\n%s", want, msg)
		}
	}
}

// TestArtifactMirrorCARootLandsWhereTheStoreLooks — the container's trust store
// will actually read the file the fixture drops into it.
//
// `update-ca-certificates` scans /usr/local/share/ca-certificates and takes only
// names ending in `.crt`. Everything else it ignores — exit code 0, "0 added",
// not a word — so container.go's exit-code check cannot catch this: the command
// succeeds at doing nothing. The name is PEM content, which makes `.pem` the
// natural edit, and the whole cost of making it lands twenty minutes into a live
// run as `x509: certificate signed by unknown authority` inside the container,
// against the product, on a machine where nothing is wrong.
//
// Which is the failure shape this ticket exists to remove, so it gets a guard
// rather than a comment.
func TestArtifactMirrorCARootLandsWhereTheStoreLooks(t *testing.T) {
	if !strings.HasSuffix(artifactMirrorCAFile, ".crt") {
		t.Errorf("artifactMirrorCAFile = %q. update-ca-certificates reads only *.crt and skips the rest\n"+
			"  silently, exit code 0 — so the container's store stays without this root and the first\n"+
			"  core.url fetch dies on an unknown authority, twenty minutes in, blamed on the product.",
			artifactMirrorCAFile)
	}
	if artifactMirrorCADir != "/usr/local/share/ca-certificates" {
		t.Errorf("artifactMirrorCADir = %q. That is the one directory update-ca-certificates scans;\n"+
			"  /etc/ssl/certs holds the GENERATED bundle and a file dropped there is overwritten by\n"+
			"  the next regeneration.", artifactMirrorCADir)
	}
}

// TestArtifactMirrorCAPEMIsTheRootAndNotTheLeaf — what gets dropped into the
// store is a CA, and the served certificate chains to it.
//
// A separate failure from the one above and a quieter one. Both certificates are
// minted in the same function and both marshal to PEM, so handing caPEM the leaf
// is a one-line slip; so is losing `IsCA` on the root. The harness's own guards
// would not notice either — m.client() pins whatever bytes it is given, and a
// pool containing the leaf verifies that leaf perfectly well. Debian's store does
// not work that way: chain building wants a CA-flagged issuer, and a leaf sitting
// in /usr/local/share/ca-certificates is inert.
//
// So this checks the property the CONTAINER depends on, not the one the harness
// happens to satisfy: caPEM holds exactly one certificate, it is a CA, and it is
// the issuer that actually signed what the mirror presents.
func TestArtifactMirrorCAPEMIsTheRootAndNotTheLeaf(t *testing.T) {
	m, err := startArtifactMirror(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatalf("start mirror: %v", err)
	}
	t.Cleanup(m.close)

	block, rest := pem.Decode(m.caPEM)
	if block == nil {
		t.Fatalf("caPEM does not decode as PEM; update-ca-certificates would skip it")
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		t.Errorf("caPEM carries more than one PEM block; the store expects the root alone")
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("caPEM does not parse as a certificate: %v", err)
	}
	if !root.IsCA || !root.BasicConstraintsValid {
		t.Errorf("caPEM holds IsCA=%v BasicConstraintsValid=%v. A certificate without CA basic\n"+
			"  constraints is ignored when the container builds a chain — installed, trusted by\n"+
			"  nothing.", root.IsCA, root.BasicConstraintsValid)
	}
	if root.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("caPEM's root does not carry KeyUsageCertSign, so it cannot be the issuer of anything")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(m.caPEM) {
		t.Fatalf("caPEM was rejected by a cert pool")
	}
	conn, err := tls.Dial("tcp", strings.TrimPrefix(m.baseURL, "https://"),
		&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("the mirror's own root does not verify the mirror's own certificate: %v", err)
	}
	defer conn.Close()

	leaf := conn.ConnectionState().PeerCertificates[0]
	if leaf.Equal(root) {
		t.Fatalf("the mirror serves its CA as the leaf; the container would be handed the signer\n" +
			"  as the server certificate")
	}
	if err := leaf.CheckSignatureFrom(root); err != nil {
		t.Errorf("the served certificate was not signed by what caPEM carries (%v).\n"+
			"  The store would trust a root unrelated to the handshake and the fetch would fail\n"+
			"  inside the container.", err)
	}
}

// TestArtifactMirrorServesNothingOutsideItsCache — a path traversal out of the
// cache root gets nothing.
//
// Low stakes (the server is bound for the length of one test) but the mirror is
// reachable from a container running a real service, and "the fixture handed the
// product an arbitrary host file" is not a sentence anyone wants to write later.
func TestArtifactMirrorServesNothingOutsideItsCache(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), fmt.Sprintf("outside-%s.txt", filepath.Base(dir)))
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write %s: %v", outside, err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	m, err := startArtifactMirror(dir, "127.0.0.1")
	if err != nil {
		t.Fatalf("start mirror: %v", err)
	}
	t.Cleanup(m.close)

	code, body := getMirror(t, m, "../"+filepath.Base(outside))
	if code == http.StatusOK && strings.Contains(body, "secret") {
		t.Fatalf("the mirror served a file outside its cache root")
	}
}

// TestArtifactMirrorHealthProbeIsNotCountedAsAFetch — the spawn-time reachability
// probe must not be able to satisfy the guard that a create used the mirror.
//
// SpawnSoulContainer asks the mirror one question before soul starts ("can this
// container reach you, and does it trust you?"), so that an unreachable or
// untrusted mirror is named at spawn time instead of twenty minutes later as a
// fetch error. That probe runs on EVERY live test — which is exactly what makes
// it dangerous. missingArtifacts is the one guard that notices when the vars
// overlay was written and then ignored (out-sorted, silenced by a `_stack.yaml`,
// reading a var name the scenario dropped), and it notices it by asking whether
// the mirror was used at all. A probe that counted would answer "yes, always",
// including on the runs where every tarball came from github, and the strongest
// guard in this change would be structurally unable to fail.
//
// So: the probe is served, and it leaves no trace in the accounting.
func TestArtifactMirrorHealthProbeIsNotCountedAsAFetch(t *testing.T) {
	dir := t.TempDir()
	cat := []upstreamArtifact{
		fakeArtifact(t, dir, "node_exporter", "1.8.2", "a"),
		fakeArtifact(t, dir, "vector", "0.40.0", "c"),
	}

	m, err := startArtifactMirror(dir, "127.0.0.1")
	if err != nil {
		t.Fatalf("start mirror: %v", err)
	}
	t.Cleanup(m.close)

	// The probe answers — it has to, or SpawnSoulContainer fails every live test
	// on a mirror that is working fine.
	code, body := getMirror(t, m, strings.TrimPrefix(artifactMirrorHealthPath, "/"))
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200; SpawnSoulContainer's pre-flight would fail against a healthy mirror",
			artifactMirrorHealthPath, code)
	}
	if !strings.Contains(body, artifactMirrorHealthBody) {
		t.Errorf("the health path answers %q, which does not identify what answered.\n"+
			"  SpawnSoulContainer's pre-flight asserts these bytes came back over the wire, and\n"+
			"  that assertion is the runtime half of the collision guard below: if the health path\n"+
			"  ever names a catalogued tarball, this is what the product receives instead of it.",
			body)
	}

	// And it is invisible to the accounting. `served()` empty, not merely "does
	// not contain a cacheRel": the failure being guarded is a pre-flight that
	// counts, and it counts under whatever path it used.
	if got := m.served(); len(got) != 0 {
		t.Errorf("the health probe was recorded as a fetch: served() = %v.\n"+
			"  missingArtifacts would then be satisfied by the pre-flight alone, on every run,\n"+
			"  including the runs where the overlay contributed nothing and the tarballs came\n"+
			"  from github — the guard would be incapable of going red.", got)
	}
	if msg := missingArtifacts(m, cat); msg == "" {
		t.Fatalf("after the health probe and nothing else, missingArtifacts is quiet; it must still\n" +
			"  accuse every catalogued tarball")
	}

	// The other way to break the same property, and it does not look like a bug
	// while you are making it: point the probe at a path the catalog also uses.
	// The handler answers the health path from a literal and returns before the
	// file server sees the request, so that tarball would be shadowed — the
	// product gets a 200 carrying one sentence of English, unpacking fails, and
	// missingArtifacts simultaneously accuses the same artifact of never having
	// been fetched. Two contradictory findings, both pointed at the product.
	//
	// The catalog to ask is the PRODUCTION one. Asking `cat` — the two artifacts
	// this test invented four lines up — would compare the health path against
	// paths the test chose itself, which no edit to artifactMirrorHealthPath can
	// ever collide with. That loop was here and it could not go red.
	probeRel := strings.TrimPrefix(artifactMirrorHealthPath, "/")
	for _, a := range artifactCatalog() {
		if a.cacheRel() == probeRel {
			t.Errorf("the health path %q is also the cache path of catalogued artifact %s.\n"+
				"  The handler answers it before the file server is reached, so the real tarball is\n"+
				"  shadowed by %q: a 200 the product cannot unpack, and an accusation from\n"+
				"  missingArtifacts that %s was never fetched at all.",
				artifactMirrorHealthPath, a.varPrefix, artifactMirrorHealthBody, a.varPrefix)
		}
	}

	// A real fetch still lands, so the emptiness above is the probe being excluded
	// and not the counter being broken.
	getMirror(t, m, cat[0].cacheRel())
	if got := m.hitCount(cat[0].cacheRel()); got != 1 {
		t.Errorf("hitCount(%s) = %d after a real request, want 1: the counter itself stopped working,\n"+
			"  which would make every assertion above vacuous", cat[0].cacheRel(), got)
	}
}

// TestArtifactMirrorRefusesAnAdvertiseHostThatWouldComeOutBracketed — a host the
// subject cannot be handed is rejected here, where the message can say why.
//
// `net.JoinHostPort` brackets any host containing a colon, and all three destinies
// declare base_url as `^https://[A-Za-z0-9._/:-]+$` — a character class with no
// brackets in it (asserted against the real destiny files in
// TestArtifactMirrorURLIsOneTheDestinyWillAccept). So a mirror advertised on
// `::1` produces a URL that fails input validation INSIDE the container, on a
// create, twenty minutes in, wearing the costume of a product defect — the exact
// failure shape this ticket exists to remove. E2E_KEEPER_HOST is set by hand and
// by CI, and on a v6-first host it is a plausible thing to set.
func TestArtifactMirrorRefusesAnAdvertiseHostThatWouldComeOutBracketed(t *testing.T) {
	// The last three are the reason this test is not named after IPv6. An
	// IPv4-mapped literal has a To4() and a zone-ID literal does not parse as an IP
	// at all, so an address-family test admits both — and JoinHostPort brackets
	// both, because it brackets on a colon in the host. The invariant is about the
	// authority the mirror advertises, so the cases have to include the ones a
	// family test gets wrong.
	for _, host := range []string{
		"::1", "fd00::1", "2001:db8::dead:beef",
		"::ffff:127.0.0.1", "::ffff:172.27.122.166", "fe80::1%eth0",
	} {
		m, err := startArtifactMirror(t.TempDir(), host)
		if err == nil {
			if m != nil {
				t.Errorf("startArtifactMirror(%q) succeeded and advertises %q; that URL is one the destiny\n"+
					"  pattern rejects, and the rejection would arrive as a failed create", host, m.baseURL)
				m.close()
				continue
			}
			t.Errorf("startArtifactMirror(%q) succeeded; it would advertise a bracketed URL the destiny\n"+
				"  rejects, and the rejection would arrive as a failed create", host)
			continue
		}
		if m != nil {
			m.close()
			t.Errorf("startArtifactMirror(%q) refused but still returned a mirror; its listener leaks", host)
		}
		for _, want := range []string{host, "E2E_KEEPER_HOST", "bracket"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal for %q does not mention %q, so the reader is not told what to set\n"+
					"  instead:\n  %v", host, want, err)
			}
		}
	}

	// The shapes that must keep working — refusing too much would be the same
	// twenty-minute failure with the blame moved.
	for _, host := range []string{"127.0.0.1", "172.27.122.166", "host.docker.internal", "localhost"} {
		m, err := startArtifactMirror(t.TempDir(), host)
		if err != nil {
			t.Errorf("startArtifactMirror(%q) was refused: %v", host, err)
			continue
		}
		t.Cleanup(m.close)
		if strings.Contains(m.baseURL, "[") {
			t.Errorf("baseURL for %q is %q; a bracketed authority does not match the destiny pattern",
				host, m.baseURL)
		}
	}
}

// The count in `update-ca-certificates`' output is the only signal it gives:
// the command exits 0 whether it absorbed the CA or ignored the file entirely,
// so SpawnSoulContainer reads the number. This pins the reader against the
// shapes that make the obvious substring tests wrong — and those two are here as
// cases, not as prose, because both look correct until a count reaches double
// digits, which is exactly when nobody is looking.
func TestCAAddedCountReadsTheNumberAndNotASubstring(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want int
	}{
		{"the real shape", "Updating certificates in /etc/ssl/certs...\n1 added, 0 removed; done.\n", 1},
		{"nothing absorbed", "Updating certificates in /etc/ssl/certs...\n0 added, 0 removed; done.\n", 0},

		// `strings.Contains(out, "1 added")` says yes to this one and no to
		// "2 added": it false-greens a store that took more than we shipped and
		// false-reds one that took several.
		{"twenty-one, which contains `1 added`", "21 added, 0 removed; done.\n", 21},
		{"two, which does not contain `1 added`", "2 added, 0 removed; done.\n", 2},

		// And the repair-by-inversion, `!Contains(out, "0 added")`, calls this one
		// a failure.
		{"ten, which contains `0 added`", "10 added, 0 removed; done.\n", 10},

		// No line at all is not zero. Zero means the command looked and took
		// nothing; -1 means it never told us, and the two want different messages.
		{"no count line", "Updating certificates in /etc/ssl/certs...\n", -1},
		{"empty output", "", -1},

		// The number has to come from the count line, not from a path that happens
		// to carry digits before it.
		{"digits earlier in the output", "cert 9 added to /usr/local/share/ca-certificates\n7 added, 0 removed; done.\n", 7},
	} {
		if got := caAddedCount(tc.out); got != tc.want {
			t.Errorf("%s: caAddedCount(%q) = %d, want %d", tc.name, tc.out, got, tc.want)
		}
	}
}
