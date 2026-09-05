package module_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// The release's layout on the source, and the platform the test host claims. The
// facts are fixed rather than read from the machine running the test: what is under
// test is that the HOST's own facts pick the row, and a test that agreed with
// runtime.GOARCH by construction would stop proving that on an arm64 runner.
const (
	basePath      = "/plugins/redis"
	linuxAMD64    = "redis_linux_amd64"
	linuxARM64    = "redis_linux_arm64"
	darwinARM64   = "redis_darwin_arm64"
	testOSFamily  = "debian"
	testHostArch  = "amd64"
	otherArtifact = "an artifact built for another platform\n"
)

// sourceFixture is the base fixture wired to a live artifact source: an https server
// serving the release, and a grant carrying the rows that name it.
type sourceFixture struct {
	*fixture
	// t is held so setRows can re-sign without every call site threading it.
	t    *testing.T
	srv  *httptest.Server
	hits []string
	// body / status are what the source answers with; the zero value serves the
	// fixture's artifact with 200.
	body   []byte
	status int
	// opts records what the module asked its client factory for.
	opts []util.HTTPClientOpts
}

func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()
	f := newFixture(t)
	sf := &sourceFixture{fixture: f, t: t}

	sf.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sf.hits = append(sf.hits, r.URL.Path)
		if sf.status != 0 && sf.status != http.StatusOK {
			w.WriteHeader(sf.status)
			return
		}
		body := sf.body
		if body == nil {
			body = f.binData
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(sf.srv.Close)

	f.rec.Source = sf.srv.URL + basePath
	f.rec.Kind = sharedplugin.SourceKindArtifact
	sf.setRows(
		sharedhost.SigilArtifact{OS: "linux", Arch: "amd64", Path: linuxAMD64, SHA256: f.binSHA},
		sharedhost.SigilArtifact{OS: "darwin", Arch: "arm64", Path: darwinARM64, SHA256: sha256Hex(otherArtifact)},
	)

	// The client seam points at the test server: its certificate is self-signed, so
	// the production factory's client cannot talk to it. The host facts are what row
	// selection reads.
	sf.mod.NewClient = func(opts util.HTTPClientOpts) util.HTTPDoer {
		sf.opts = append(sf.opts, opts)
		return sf.srv.Client()
	}
	sf.setHost(testOSFamily, testHostArch)
	return sf
}

// setRows replaces the grant's artifact rows AND re-signs it. The signed block covers
// the whole list since NIM-795, so rows set without a fresh signature would fail as
// bad_signature — and every test built on them would then pass for a reason that has
// nothing to do with what it names.
func (sf *sourceFixture) setRows(rows ...sharedhost.SigilArtifact) {
	sf.rec.Artifacts = rows
	sf.resign(sf.t)
}

// setSource replaces the grant's publication root AND re-signs it. Under NIM-795 the
// address is the grant's `source`, which the block covers — an edit without a fresh
// signature would fail as bad_signature and the test would stop testing what it names.
func (sf *sourceFixture) setSource(base string) {
	sf.rec.Source = base
	sf.resign(sf.t)
}

// signWithAnotherKey re-signs the grant with a key that is NOT a trust anchor, so
// verify fails while every earlier step still succeeds. It is how a test reaches the
// verify step deliberately now that the row digest IS the approved digest: bytes that
// satisfy the fetch necessarily satisfy the digest check.
func (sf *sourceFixture) signWithAnotherKey(t *testing.T) {
	t.Helper()
	_, foreign, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate foreign key: %v", err)
	}
	sf.priv = foreign
	sf.resign(t)
}

func (sf *sourceFixture) setHost(family, arch string) {
	sf.mod.SetHostFacts(util.HostFacts{OSFamily: family, Arch: arch})
}

func (sf *sourceFixture) url(path string) string { return sf.srv.URL + basePath + "/" + path }

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// --- the artifact grant installs from the source ---

func TestApplyArtifactGrantInstallsFromSource(t *testing.T) {
	sf := newSourceFixture(t)

	ev := sf.apply(t, map[string]any{"name": "redis"})
	if ev.GetFailed() {
		t.Fatalf("expected success, got failed: %q", ev.GetMessage())
	}
	if !ev.GetChanged() {
		t.Error("changed = false; installing a new module should give changed=true")
	}

	got, err := os.ReadFile(sf.binPath())
	if err != nil {
		t.Fatalf("binary not materialized: %v", err)
	}
	if string(got) != string(sf.binData) {
		t.Error("installed binary content did not match what the source served")
	}

	// The bytes did not ride the EventStream — that is the whole point of NIM-793.
	if sf.fetcher.calls != 0 {
		t.Errorf("FetchModule called %d time(s) for an artifact grant; the bytes must come from the source", sf.fetcher.calls)
	}
	if len(sf.hits) != 1 || sf.hits[0] != basePath+"/"+linuxAMD64 {
		t.Errorf("source requests = %v, want exactly [%s]", sf.hits, basePath+"/"+linuxAMD64)
	}

	out := ev.GetOutput().AsMap()
	if out["fetch_via"] != "source" {
		t.Errorf("fetch_via = %v, want %q — the run has to say which transport it used", out["fetch_via"], "source")
	}
	if out["fetch_url"] != sf.url(linuxAMD64) {
		t.Errorf("fetch_url = %v, want %q", out["fetch_url"], sf.url(linuxAMD64))
	}
	if _, ok := out["warnings"]; ok {
		t.Errorf("warnings = %v on a clean source pull; nothing was weakened", out["warnings"])
	}

	// The dial guard is lifted deliberately (an artifact repository inside the
	// perimeter answers on a private address); every other guard core.url ships with
	// stays on, and the flags are the ones core.url would build a client from.
	if len(sf.opts) == 0 {
		t.Fatal("the module never asked for a client")
	}
	want := util.HTTPClientOpts{AllowPrivate: true}
	if sf.opts[0] != want {
		t.Errorf("client opts = %+v, want %+v", sf.opts[0], want)
	}
}

// GUARD: the row is picked by the HOST's facts, not by the first row in the grant.
// A Linux distro family collapses to the operating system an artifact row names.
func TestApplyArtifactGrantPicksRowByHostFacts(t *testing.T) {
	sf := newSourceFixture(t)
	sf.setRows(
		sharedhost.SigilArtifact{OS: "linux", Arch: "amd64", Path: linuxAMD64, SHA256: sha256Hex(otherArtifact)},
		sharedhost.SigilArtifact{OS: "linux", Arch: "arm64", Path: linuxARM64, SHA256: sf.binSHA},
	)
	// alpine is a different family from the fixture default and still Linux; the arch
	// is what separates the two rows.
	sf.setHost("alpine", "arm64")

	ev := sf.apply(t, map[string]any{"name": "redis"})
	if ev.GetFailed() {
		t.Fatalf("expected success, got failed: %q", ev.GetMessage())
	}
	if len(sf.hits) != 1 || sf.hits[0] != basePath+"/"+linuxARM64 {
		t.Errorf("source requests = %v, want exactly [%s] — the host is linux/arm64", sf.hits, basePath+"/"+linuxARM64)
	}
}

// A factless host (push mode, or an unreadable /etc/os-release) still resolves to a
// platform: the binary that would run the artifact is the one asking. The grant here
// carries a row for the running platform and a decoy for another, so the assertion is
// on WHICH row was fetched, not merely that something was.
func TestApplyArtifactGrantWithoutFactsUsesTheRunningPlatform(t *testing.T) {
	sf := newSourceFixture(t)
	sf.setHost("", "")
	sf.setRows(
		sharedhost.SigilArtifact{OS: "plan9", Arch: "sparc", Path: "decoy", SHA256: sha256Hex(otherArtifact)},
		sharedhost.SigilArtifact{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: linuxAMD64, SHA256: sf.binSHA},
	)

	ev := sf.apply(t, map[string]any{"name": "redis"})
	if ev.GetFailed() {
		t.Fatalf("expected success, got failed: %q", ev.GetMessage())
	}
	if len(sf.hits) != 1 || sf.hits[0] != basePath+"/"+linuxAMD64 {
		t.Errorf("source requests = %v, want exactly [%s] — the row for %s/%s",
			sf.hits, basePath+"/"+linuxAMD64, runtime.GOOS, runtime.GOARCH)
	}
}

// GUARD: an artifact grant installs with NO EventStream session at all. This is the
// capability the change adds — before it, a run without a session could not install a
// module in any circumstance ("push mode is not supported").
func TestApplyArtifactGrantInstallsInPushMode(t *testing.T) {
	sf := newSourceFixture(t)

	ev := sf.applyCtx(t, context.Background(), map[string]any{"name": "redis"})
	if ev.GetFailed() {
		t.Fatalf("expected success with no session, got failed: %q", ev.GetMessage())
	}
	got, err := os.ReadFile(sf.binPath())
	if err != nil || string(got) != string(sf.binData) {
		t.Fatalf("the artifact was not installed without a session: err=%v", err)
	}
	if out := ev.GetOutput().AsMap(); out["fetch_via"] != "source" {
		t.Errorf("fetch_via = %v, want %q", out["fetch_via"], "source")
	}
}

// Idempotency is decided before either transport: a slot already holding the granted
// bytes touches neither the source nor Keeper.
func TestApplyArtifactGrantIdempotentSkipsBothTransports(t *testing.T) {
	sf := newSourceFixture(t)
	if err := os.MkdirAll(filepath.Dir(sf.binPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sf.binPath(), sf.binData, 0o755); err != nil {
		t.Fatal(err)
	}

	ev := sf.apply(t, map[string]any{"name": "redis"})
	if ev.GetFailed() {
		t.Fatalf("expected success, got failed: %q", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Error("changed = true; a slot already holding the granted bytes is a no-op")
	}
	if len(sf.hits) != 0 || sf.fetcher.calls != 0 {
		t.Errorf("source requests = %v, FetchModule calls = %d; neither transport should have been used",
			sf.hits, sf.fetcher.calls)
	}
	if _, ok := ev.GetOutput().AsMap()["fetch_via"]; ok {
		t.Error("fetch_via is reported for a run that fetched nothing")
	}
}

// --- no row for this host: a closed refusal, and no fallback ---

// The reason is `module_not_allowed`, not `module_fetch_failed`: nothing was ever
// approved for this platform, so it is an approval gap and not a transport problem.
// NIM-795 moved the row selection ahead of the fetch for exactly that reason — the
// refusal now happens where the grant is read, and it reads the same as the
// shared-verify reason `no_artifact_for_platform`. The operator's fix is to publish and
// re-approve a release that covers the platform; nothing about the network would help.
func TestApplyArtifactGrantRefusesUncoveredPlatform(t *testing.T) {
	sf := newSourceFixture(t)
	sf.setRows(
		sharedhost.SigilArtifact{OS: "linux", Arch: "arm64", Path: linuxARM64, SHA256: sha256Hex(otherArtifact)},
		sharedhost.SigilArtifact{OS: "darwin", Arch: "arm64", Path: darwinARM64, SHA256: sha256Hex(otherArtifact)},
	)
	sf.setHost("debian", "amd64")

	ev := sf.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_not_allowed")

	// The operator has to see what is missing without opening the catalog.
	msg := ev.GetMessage()
	if !strings.Contains(msg, "no artifact for linux/amd64") {
		t.Errorf("message = %q; it must name the platform the host is", msg)
	}
	if !strings.Contains(msg, "darwin/arm64, linux/arm64") {
		t.Errorf("message = %q; it must list the platforms the release does carry, sorted", msg)
	}

	if len(sf.hits) != 0 {
		t.Errorf("source requested %v; a platform the release does not cover is decided before the network", sf.hits)
	}
	// A session IS available here. Keeper holds no bytes for a platform the release
	// never built, so a fallback could only replace a precise answer with a vague one.
	if sf.fetcher.calls != 0 {
		t.Errorf("FetchModule called %d time(s); an uncovered platform is a refusal, not a fallback", sf.fetcher.calls)
	}
	if _, err := os.Stat(sf.binPath()); !os.IsNotExist(err) {
		t.Errorf("something materialized for a refused platform (stat err=%v)", err)
	}
}

// --- the source is down: the second legitimate transport takes over ---

func TestApplyArtifactGrantFallsBackToKeeper(t *testing.T) {
	sf := newSourceFixture(t)
	sf.status = http.StatusServiceUnavailable
	// The row's digest is what the fallback must ask Keeper by. Since NIM-795 there is
	// no second digest to confuse it with — the grant carries one row per platform and
	// nothing else — so the assertion is that the request names THIS host's row rather
	// than, say, the first row of the release.
	rowSHA := sf.binSHA
	sf.setRows(
		sharedhost.SigilArtifact{OS: "darwin", Arch: "arm64", Path: darwinARM64, SHA256: sha256Hex(otherArtifact)},
		sharedhost.SigilArtifact{OS: "linux", Arch: "amd64", Path: linuxAMD64, SHA256: rowSHA},
	)

	ev := sf.apply(t, map[string]any{"name": "redis"})
	if ev.GetFailed() {
		t.Fatalf("expected the fallback to carry the install, got failed: %q", ev.GetMessage())
	}
	if sf.fetcher.calls != 1 {
		t.Fatalf("FetchModule called %d time(s), want 1", sf.fetcher.calls)
	}
	if sf.fetcher.gotReq.GetBinarySha256() != rowSHA {
		t.Errorf("PluginFetchRequest.binary_sha256 = %q, want the selected row's %q — the fallback must ask for the bytes THIS host needs",
			sf.fetcher.gotReq.GetBinarySha256(), rowSHA)
	}
	got, err := os.ReadFile(sf.binPath())
	if err != nil || string(got) != string(sf.binData) {
		t.Fatalf("the fallback did not install the artifact: err=%v", err)
	}

	out := ev.GetOutput().AsMap()
	if out["fetch_via"] != "keeper" {
		t.Errorf("fetch_via = %v, want %q", out["fetch_via"], "keeper")
	}
	if u, ok := out["fetch_url"]; ok {
		t.Errorf("fetch_url = %v beside fetch_via=keeper; that names an address these bytes did not come from", u)
	}
	warnings, _ := out["warnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "source unreachable") {
		t.Errorf("warnings = %v; a run that silently changed transport is a run nobody can debug", out["warnings"])
	}
}

func TestApplyArtifactGrantSourceDownWithoutSession(t *testing.T) {
	sf := newSourceFixture(t)
	sf.status = http.StatusServiceUnavailable

	ev := sf.applyCtx(t, context.Background(), map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_fetch_failed")
	if !strings.Contains(ev.GetMessage(), "no EventStream session") {
		t.Errorf("message = %q; it must say the fallback was not available either", ev.GetMessage())
	}
	if _, err := os.Stat(sf.binPath()); !os.IsNotExist(err) {
		t.Errorf("something materialized with no transport that worked (stat err=%v)", err)
	}
}

// --- what the source served is not what the grant promised ---

// GUARD: a source serving the wrong bytes is answered, not routed around. Keeper can
// stand in for a source that is down; standing in for one that is tampered with would
// turn the only signal of it into a warning line under a green run.
func TestApplyArtifactGrantWrongBytesDoNotFallBack(t *testing.T) {
	sf := newSourceFixture(t)
	sf.body = []byte("malicious payload")

	ev := sf.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_fetch_failed")
	if !strings.Contains(ev.GetMessage(), "sha256 mismatch") {
		t.Errorf("message = %q; it must name the mismatch", ev.GetMessage())
	}
	if sf.fetcher.calls != 0 {
		t.Errorf("FetchModule called %d time(s) after the source served the wrong bytes", sf.fetcher.calls)
	}
	if _, err := os.Stat(sf.binPath()); !os.IsNotExist(err) {
		t.Errorf("unpromised bytes materialized (stat err=%v)", err)
	}
}

// GUARD: the path is joined, never steered. Each of these would send the fetch
// somewhere the catalog entry does not name — the reason the catalog form carries no
// substitutions at all (NIM-793).
func TestArtifactPathIsJoinedNotSteered(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"absolute path", "/etc/shadow"},
		{"a URL of its own", "https://evil.example/payload"},
		{"parent segment", "../../other-plugin/redis_linux_amd64"},
		{"current segment", "./redis_linux_amd64"},
		{"query", "redis_linux_amd64?as=other"},
		{"fragment", "redis_linux_amd64#frag"},
		{"empty", ""},
		// The server is what resolves the path: a traversal spelled in percent-
		// encoding passes the literal check and arrives decoded.
		{"encoded parent segment", "%2e%2e%2fother-plugin/redis_linux_amd64"},
		{"encoded parent segment, upper case", "%2E%2E%2Fother-plugin/redis_linux_amd64"},
		{"half-encoded separator", "..%2f..%2fredis_linux_amd64"},
		{"encoded absolute path", "%2fetc%2fshadow"},
		{"invalid encoding", "redis_linux_amd64%zz"},
		// Not a separator to Go, but one to some servers.
		{"backslash traversal", `..\..\redis_linux_amd64`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sf := newSourceFixture(t)
			sf.setRows(sharedhost.SigilArtifact{OS: "linux", Arch: "amd64", Path: tc.path, SHA256: sf.binSHA})

			ev := sf.apply(t, map[string]any{"name": "redis"})
			wantFailedReason(t, ev, "module_fetch_failed")
			if len(sf.hits) != 0 {
				t.Errorf("source requested %v; the path was refused, nothing should have been sent", sf.hits)
			}
			// An address this host cannot use is a configuration answer, not a
			// transport one: Keeper cannot make the catalog row right.
			if sf.fetcher.calls != 0 {
				t.Errorf("FetchModule called %d time(s) for an unusable address", sf.fetcher.calls)
			}
		})
	}
}

func TestApplyArtifactGrantWithoutBaseURL(t *testing.T) {
	sf := newSourceFixture(t)
	sf.setSource("")

	ev := sf.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_fetch_failed")
	if !strings.Contains(ev.GetMessage(), "no base_url") {
		t.Errorf("message = %q; it must say which half of the grant is missing", ev.GetMessage())
	}
	if sf.fetcher.calls != 0 {
		t.Errorf("FetchModule called %d time(s) for a grant with no address", sf.fetcher.calls)
	}
}

// An `@` or a `?` in a PATH segment is not userinfo and not a query, and a base_url
// carrying one is perfectly ordinary (npm-style scopes, for one). Refusing it would
// make the guard against credentials a guard against a legal address.
func TestApplyArtifactGrantAcceptsAtSignInBaseURLPath(t *testing.T) {
	sf := newSourceFixture(t)
	sf.setSource(sf.srv.URL + "/@souls/plugins")

	ev := sf.apply(t, map[string]any{"name": "redis"})
	if ev.GetFailed() {
		t.Fatalf("a scoped path was refused: %q", ev.GetMessage())
	}
	if len(sf.hits) != 1 || sf.hits[0] != "/@souls/plugins/"+linuxAMD64 {
		t.Errorf("source requests = %v, want exactly [/@souls/plugins/%s]", sf.hits, linuxAMD64)
	}
}

// GUARD: base_url forms that cannot address a file under themselves. Each is refused
// before the network, and none falls back to Keeper — a catalog field Keeper cannot
// fix must not be answered by quietly using the other transport forever.
func TestApplyArtifactGrantRefusesUnusableBaseURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		// The join would put the row's filename inside the query string, so every
		// row would fetch the base itself.
		{"query", "https://repo.internal/plugins?token=abc", "query or a fragment"},
		{"fragment", "https://repo.internal/plugins#frag", "query or a fragment"},
		// Source authentication is deferred by NIM-793, and a password in base_url
		// would travel into fetch_url, i.e. into RunResult and OTel.
		{"credentials", "https://svc:s3cr3t@repo.internal/plugins", "must not carry credentials"},
		// One slash: parses, passes the scheme check, and would die inside the client
		// with a transport-shaped error that the fallback would then route around.
		{"no host", "https:/repo.internal/plugins", "names no host"},
		// https-only, the same rule core.url applies, and here with no param to lift it.
		{"plaintext http", "http://repo.internal/plugins", "artifact source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sf := newSourceFixture(t)
			sf.setSource(tc.base)

			ev := sf.apply(t, map[string]any{"name": "redis"})
			wantFailedReason(t, ev, "module_fetch_failed")
			if !strings.Contains(ev.GetMessage(), tc.want) {
				t.Errorf("message = %q; want it to contain %q", ev.GetMessage(), tc.want)
			}
			if len(sf.hits) != 0 {
				t.Errorf("source requested %v; the address was refused before the network", sf.hits)
			}
			if sf.fetcher.calls != 0 {
				t.Errorf("FetchModule called %d time(s) for an address Keeper cannot fix", sf.fetcher.calls)
			}
			if _, err := os.Stat(sf.binPath()); !os.IsNotExist(err) {
				t.Errorf("something materialized behind a refused address (stat err=%v)", err)
			}
		})
	}
}

// GUARD: "the source is down" and "the source answered no" are different facts, and
// only the first is Keeper's to stand in for. A 404 is a catalog row pointing at a
// file that is not there — answered by the fallback, the typo would sit behind a green
// run forever.
func TestApplyArtifactGrantStatusDecidesTheFallback(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantFallback bool
	}{
		{"not found", http.StatusNotFound, false},
		{"forbidden", http.StatusForbidden, false},
		{"unauthorized", http.StatusUnauthorized, false},
		{"too many requests", http.StatusTooManyRequests, true},
		{"request timeout", http.StatusRequestTimeout, true},
		{"server error", http.StatusInternalServerError, true},
		{"bad gateway", http.StatusBadGateway, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sf := newSourceFixture(t)
			sf.status = tc.status

			ev := sf.apply(t, map[string]any{"name": "redis"})
			if tc.wantFallback {
				if ev.GetFailed() {
					t.Fatalf("status %d is the source being unavailable; the fallback should have carried it: %q",
						tc.status, ev.GetMessage())
				}
				if sf.fetcher.calls != 1 {
					t.Errorf("FetchModule called %d time(s), want 1", sf.fetcher.calls)
				}
				return
			}
			wantFailedReason(t, ev, "module_fetch_failed")
			if sf.fetcher.calls != 0 {
				t.Errorf("FetchModule called %d time(s); status %d is an answer about this request, not an outage",
					sf.fetcher.calls, tc.status)
			}
		})
	}
}

// --- THE ordering guard ---

// GUARD: Sigil verify runs BEFORE materialization on the source path too.
//
// The source here serves exactly what its catalog row promises — so the fetch step is
// satisfied — and the grant is signed by a key that is NOT a trust anchor, so verify
// refuses. Reaching verify this way is forced by NIM-795: the row digest IS the
// approved digest, so bytes that pass the fetch cannot fail the digest comparison, and
// the signature is what is left to fail. The install must stop with the slot exactly
// as it was: the previous artifact and its digest sidecar both intact, since
// installSlot removes the sidecar first and would otherwise leave a freshly installed
// binary sealed against a stale digest.
//
// Move the VerifyArtifactBytes call after installSlot and this test goes red on both
// of its slot assertions (the refusal itself still arrives, which is exactly why
// asserting the refusal alone would not have caught the reordering). That is what it
// is for: "rights before the network, signature before the disk" is the whole reason
// an untrusted source is safe to pull from, and no other test covers that order on
// this path.
func TestApplyArtifactSourceVerifyRunsBeforeInstall(t *testing.T) {
	sf := newSourceFixture(t)
	served := []byte("bytes the source vouches for but the grant does not\n")
	sf.body = served
	sf.setRows(sharedhost.SigilArtifact{
		OS: "linux", Arch: "amd64", Path: linuxAMD64, SHA256: sha256Hex(string(served)),
	})
	sf.signWithAnotherKey(t)

	// A slot that already holds a working install: this is what a failed verify must
	// not disturb.
	slotDir := filepath.Dir(sf.binPath())
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := []byte("#!/bin/sh\necho the artifact that was already installed\n")
	if err := os.WriteFile(sf.binPath(), previous, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(slotDir, sharedhost.DigestSidecarName)
	if err := os.WriteFile(sidecar, []byte(sha256Hex(string(previous))), 0o400); err != nil {
		t.Fatal(err)
	}

	ev := sf.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_verify_failed")

	got, err := os.ReadFile(sf.binPath())
	if err != nil {
		t.Fatalf("the previously installed artifact is gone: %v", err)
	}
	if string(got) != string(previous) {
		t.Error("the slot holds the unverified bytes: verify no longer runs before materialization")
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("the digest sidecar was cleared for an install that never passed verify: %v", err)
	}
}
