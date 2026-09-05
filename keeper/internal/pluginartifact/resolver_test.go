package pluginartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginsource"
)

const (
	testAlias = "redis"
	testRef   = "v1.4.0"
)

// stampedArtifact builds the bytes of a published plugin file: a tiny script with the
// canonical schema document in its trailer, exactly as `soul-mod stamp` leaves it. The
// script body differs per platform, which is the whole point — a release is several
// sets of bytes with several digests.
func stampedArtifact(t *testing.T, body string, doc schema.Document) []byte {
	t.Helper()
	payload, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return schema.AppendTrailer([]byte("#!/bin/sh\n"+body+"\n"), payload)
}

func moduleDoc() schema.Document {
	return schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name:   "acl",
			States: map[string]schema.State{"present": {Description: "the ACL user exists"}},
		}},
	}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// release is a fake publication host: a base URL serving a fixed set of files.
type release struct {
	server *httptest.Server
	files  map[string][]byte
}

func newRelease(t *testing.T, files map[string][]byte) *release {
	t.Helper()
	r := &release{files: files}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, ok := r.files[strings.TrimPrefix(req.URL.Path, "/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.server.Close)
	// The publication root is http, which the resolver refuses in production. The
	// dev/test flag is the same one the git resolver uses for file:// — one decision,
	// "this Keeper may reach insecure local sources", not one per transport.
	t.Setenv(allowInsecureEnv, "1")
	return r
}

func (r *release) baseURL() string { return r.server.URL }

// twoPlatformRelease is the fixture the epic's example describes: linux/amd64 and
// linux/arm64, different bytes, one disclosure.
func twoPlatformRelease(t *testing.T) (*release, []config.PluginArtifactEntry) {
	t.Helper()
	doc := moduleDoc()
	amd := stampedArtifact(t, "echo amd64", doc)
	arm := stampedArtifact(t, "echo arm64", doc)
	rel := newRelease(t, map[string][]byte{
		"redis_linux_amd64": amd,
		"redis_linux_arm64": arm,
	})
	return rel, []config.PluginArtifactEntry{
		{OS: "linux", Arch: "amd64", Path: "redis_linux_amd64", SHA256: digestOf(amd)},
		{OS: "linux", Arch: "arm64", Path: "redis_linux_arm64", SHA256: digestOf(arm)},
	}
}

func entryFor(rel *release, artifacts []config.PluginArtifactEntry) config.PluginCatalogEntry {
	return config.PluginCatalogEntry{
		Name:      testAlias,
		Kind:      sharedplugin.SourceKindArtifact,
		BaseURL:   rel.baseURL(),
		Ref:       testRef,
		Artifacts: artifacts,
	}
}

func newTestResolver(t *testing.T) (*Resolver, string) {
	t.Helper()
	cacheRoot := t.TempDir()
	// nil logger and nil client: the constructor substitutes its own defaults, and
	// the per-entry deadline comes from the context rather than from the client.
	return NewResolver(cacheRoot, 0, 0, nil, slog.New(slog.DiscardHandler)), cacheRoot
}

// The happy path: both files fetched, digests verified, one immutable slot, and the
// slot reads back as a two-artifact release with a single disclosure.
func TestResolve_TwoPlatformRelease(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	r, cacheRoot := newTestResolver(t)

	got, err := r.Resolve(context.Background(), entryFor(rel, artifacts))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got.Kind != sharedplugin.SourceKindArtifact {
		t.Errorf("Kind = %q, want artifact", got.Kind)
	}
	// The base URL is what the grant will be signed on: the catalog's two "where from"
	// keys collapse into the one field the signature covers.
	if got.Source != rel.baseURL() {
		t.Errorf("Source = %q, want the base_url", got.Source)
	}
	// No commit to pin. Filling this with the slot's directory name would put a value
	// that resolves to nothing into a field an operator reads as provenance.
	if got.Origin != "" {
		t.Errorf("Origin = %q, want empty for an artifact release", got.Origin)
	}
	if got.Contents == nil || len(got.Contents.Artifacts) != 2 {
		t.Fatalf("Contents = %+v, want two artifacts", got.Contents)
	}
	for i, want := range artifacts {
		a := got.Contents.Artifacts[i]
		if a.OS != want.OS || a.Arch != want.Arch || a.Path != want.Path || a.SHA256 != want.SHA256 {
			t.Errorf("artifact %d = %+v, want %+v", i, a, want)
		}
		if _, serr := os.Stat(a.BinaryPath); serr != nil {
			t.Errorf("artifact %d not materialized: %v", i, serr)
		}
	}

	// current -> the immutable slot, and the slot is reachable by alias the way
	// `plugin.allow` reaches it.
	byAlias, err := pluginhost.ReadSlot(cacheRoot, testAlias)
	if err != nil {
		t.Fatalf("ReadSlot: %v", err)
	}
	if len(byAlias.Artifacts) != 2 {
		t.Fatalf("ReadSlot artifacts = %d, want 2", len(byAlias.Artifacts))
	}
}

// A re-resolve of an unchanged release touches the network for nothing: the slot is
// named by a digest of its own descriptor, so an unchanged release is the same
// directory and the whole fetch is skipped.
func TestResolve_UnchangedReleaseIsSkipped(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	r, _ := newTestResolver(t)
	entry := entryFor(rel, artifacts)

	first, err := r.Resolve(context.Background(), entry)
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	// Take the source offline. A second resolve that reached for it would fail.
	rel.server.Close()

	second, err := r.Resolve(context.Background(), entry)
	if err != nil {
		t.Fatalf("Resolve #2 re-fetched an unchanged release: %v", err)
	}
	if first.SlotDir != second.SlotDir {
		t.Errorf("slot moved for an unchanged release: %q -> %q", first.SlotDir, second.SlotDir)
	}
}

// The source is untrusted by construction. A publication host serving other bytes than
// the operator declared gets a refused resolve — and leaves nothing behind, so a later
// `plugin.allow` cannot find a half-written slot to approve.
func TestResolve_DigestMismatchIsFailClosed(t *testing.T) {
	doc := moduleDoc()
	honest := stampedArtifact(t, "echo honest", doc)
	rel := newRelease(t, map[string][]byte{
		"redis_linux_amd64": stampedArtifact(t, "echo substituted", doc),
	})
	entry := entryFor(rel, []config.PluginArtifactEntry{
		{OS: "linux", Arch: "amd64", Path: "redis_linux_amd64", SHA256: digestOf(honest)},
	})
	r, cacheRoot := newTestResolver(t)

	_, err := r.Resolve(context.Background(), entry)
	if !errors.Is(err, pluginsource.ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
	if _, serr := os.Stat(filepath.Join(cacheRoot, testAlias, "current")); serr == nil {
		t.Error("a refused release left an active slot behind")
	}
}

// A source that answers 404 for a declared file is the same class of failure: the
// release the operator described is not there.
func TestResolve_MissingFileIsFailClosed(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	delete(rel.files, "redis_linux_arm64")
	r, cacheRoot := newTestResolver(t)

	if _, err := r.Resolve(context.Background(), entryFor(rel, artifacts)); err == nil {
		t.Fatal("Resolve accepted a release with a missing file")
	}
	if _, serr := os.Stat(filepath.Join(cacheRoot, testAlias, "current")); serr == nil {
		t.Error("a partially-fetched release was promoted to the active slot")
	}
}

// ★ The acceptance guard, in the form of real code: an entry that declares no row for
// a platform yields no artifact for it, so no grant can approve bytes there. The
// resolver's side of it is that the release simply does not contain that platform; the
// verify side is TestVerify_NoArtifactForPlatform in shared/pluginhost.
func TestResolve_ReleaseCoversOnlyDeclaredPlatforms(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	r, _ := newTestResolver(t)

	got, err := r.Resolve(context.Background(), entryFor(rel, artifacts[:1]))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel := sharedhost.SelectArtifact(got.Contents.SigilArtifacts(), "linux", "arm64"); sel != nil {
		t.Errorf("an undeclared platform selected %+v, want nil", sel)
	}
	if sel := sharedhost.SelectArtifact(got.Contents.SigilArtifacts(), "linux", "amd64"); sel == nil {
		t.Error("the declared platform selected nothing")
	}
}

// Entries that never reach the network. Each of them would otherwise produce a grant
// whose approved bytes depend on something other than the operator's decision.
func TestResolve_RefusesUnresolvableEntries(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	good := artifacts[0]

	cases := map[string]config.PluginCatalogEntry{
		"no artifacts": {
			Name: testAlias, Kind: sharedplugin.SourceKindArtifact,
			BaseURL: rel.baseURL(), Ref: testRef,
		},
		"duplicate platform": {
			Name: testAlias, Kind: sharedplugin.SourceKindArtifact,
			BaseURL: rel.baseURL(), Ref: testRef,
			Artifacts: []config.PluginArtifactEntry{good,
				{OS: "linux", Arch: "amd64", Path: "other", SHA256: good.SHA256}},
		},
		"unplatformed row": {
			Name: testAlias, Kind: sharedplugin.SourceKindArtifact,
			BaseURL: rel.baseURL(), Ref: testRef,
			Artifacts: []config.PluginArtifactEntry{{Path: "p", SHA256: good.SHA256}},
		},
		"path leaves the base url": {
			Name: testAlias, Kind: sharedplugin.SourceKindArtifact,
			BaseURL: rel.baseURL(), Ref: testRef,
			Artifacts: []config.PluginArtifactEntry{
				{OS: "linux", Arch: "amd64", Path: "../../etc/shadow", SHA256: good.SHA256}},
		},
		"path is another address": {
			Name: testAlias, Kind: sharedplugin.SourceKindArtifact,
			BaseURL: rel.baseURL(), Ref: testRef,
			Artifacts: []config.PluginArtifactEntry{
				{OS: "linux", Arch: "amd64", Path: "https://evil.example/x", SHA256: good.SHA256}},
		},
		"no ref": {
			Name: testAlias, Kind: sharedplugin.SourceKindArtifact,
			BaseURL: rel.baseURL(), Artifacts: []config.PluginArtifactEntry{good},
		},
		"no base_url": {
			Name: testAlias, Kind: sharedplugin.SourceKindArtifact,
			Ref: testRef, Artifacts: []config.PluginArtifactEntry{good},
		},
		"reserved alias": {
			Name: "core", Kind: sharedplugin.SourceKindArtifact,
			BaseURL: rel.baseURL(), Ref: testRef, Artifacts: []config.PluginArtifactEntry{good},
		},
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			r, cacheRoot := newTestResolver(t)
			if _, err := r.Resolve(context.Background(), entry); err == nil {
				t.Fatal("Resolve accepted an entry that cannot describe a release")
			}
			if entries, _ := os.ReadDir(cacheRoot); len(entries) != 0 {
				t.Errorf("a refused entry wrote into the cache: %v", entries)
			}
		})
	}
}

// A publication root reached over plain http is refused in production. The digest check
// is a second line here, not a substitute for a transport that cannot be rewritten in
// flight.
func TestResolve_RefusesInsecureBaseURLWithoutTheFlag(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	t.Setenv(allowInsecureEnv, "")
	r, _ := newTestResolver(t)

	if _, err := r.Resolve(context.Background(), entryFor(rel, artifacts)); err == nil {
		t.Fatal("Resolve accepted an http:// base_url without the dev flag")
	}
}

// A base URL that is not a plain publication root — credentials, a query, a scheme
// nothing implements — is refused before any request is made.
func TestValidateBaseURL(t *testing.T) {
	t.Setenv(allowInsecureEnv, "")
	bad := []string{
		"ftp://nexus.internal/plugins",
		"http://nexus.internal/plugins",
		"https://",
		"https://user:pass@nexus.internal/plugins",
		"https://nexus.internal/plugins?token=x",
		"https://nexus.internal/plugins#frag",
		// A BARE trailing marker. Both parse to an EMPTY RawQuery / Fragment — the `?`
		// marker lives in url.URL.ForceQuery — so a check on the parsed FIELDS passes
		// them. Then `base + "/" + path` puts the artifact's filename in the query or
		// the fragment, and every row of the release fetches the same address: the
		// "sign one address, fetch another" property the path rule exists to prevent,
		// arriving through the other half of the URL.
		//
		// It is also the shape that makes a grant permanently uninstallable — Keeper
		// signs it, the Soul-side check refuses it, and that branch has no fallback.
		"https://nexus.internal/plugins?",
		"https://nexus.internal/plugins#",
	}
	for _, u := range bad {
		if err := validateBaseURL(u); err == nil {
			t.Errorf("validateBaseURL(%q) accepted it", u)
		}
	}
	if err := validateBaseURL("https://nexus.internal/plugins/redis"); err != nil {
		t.Errorf("validateBaseURL rejected a plain https root: %v", err)
	}
}

// One release, one disclosure. A source whose platform builds carry DIFFERENT schema
// documents has nothing an Archon could approve with one signature, so the slot is
// refused rather than resolved to whichever document happened to be read first.
func TestResolve_RefusesDivergentDisclosures(t *testing.T) {
	amd := stampedArtifact(t, "echo amd64", moduleDoc())
	other := moduleDoc()
	other.Modules[0].Name = "config"
	arm := stampedArtifact(t, "echo arm64", other)

	rel := newRelease(t, map[string][]byte{
		"redis_linux_amd64": amd,
		"redis_linux_arm64": arm,
	})
	entry := entryFor(rel, []config.PluginArtifactEntry{
		{OS: "linux", Arch: "amd64", Path: "redis_linux_amd64", SHA256: digestOf(amd)},
		{OS: "linux", Arch: "arm64", Path: "redis_linux_arm64", SHA256: digestOf(arm)},
	})
	r, _ := newTestResolver(t)

	_, err := r.Resolve(context.Background(), entry)
	if !errors.Is(err, pluginhost.ErrReleaseUnreadable) {
		t.Fatalf("err = %v, want ErrReleaseUnreadable", err)
	}
}

// An artifact fetched over the size cap is refused and the slot is not created. The
// cap is applied to the bytes actually read, not to Content-Length: the header is the
// source's claim, and the source is the thing being bounded.
func TestResolve_RefusesOversizedArtifact(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	cacheRoot := t.TempDir()
	r := NewResolver(cacheRoot, 0, 16, nil, nil)

	_, err := r.Resolve(context.Background(), entryFor(rel, artifacts))
	if !errors.Is(err, pluginsource.ErrArtifactTooLarge) {
		t.Fatalf("err = %v, want ErrArtifactTooLarge", err)
	}
	if _, serr := os.Stat(filepath.Join(cacheRoot, testAlias, "current")); serr == nil {
		t.Error("an oversized release was promoted to the active slot")
	}
}

// A source-kind mismatch is refused rather than coerced: `source` and `base_url` are
// different assertions, and reading a git entry as a publication root would fetch an
// address the operator never wrote.
//
// The fixture is deliberately a git entry that WOULD otherwise resolve — it carries a
// live base_url and a valid artifact list, so nothing but the kind check can stop it.
// A version of this test with an empty base_url passes for the wrong reason: it would
// keep passing if the kind check were deleted.
func TestResolve_RefusesGitEntry(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	r, cacheRoot := newTestResolver(t)

	entry := entryFor(rel, artifacts)
	entry.Kind = sharedplugin.SourceKindGit

	_, err := r.Resolve(context.Background(), entry)
	if !errors.Is(err, pluginsource.ErrKindUnsupported) {
		t.Fatalf("err = %v, want ErrKindUnsupported", err)
	}
	if _, serr := os.Stat(filepath.Join(cacheRoot, testAlias)); serr == nil {
		t.Error("a git entry reached the cache through the artifact provider")
	}

	// An entry with NO `kind:` is the git default and must be refused the same way —
	// this is the case a real pre-NIM-793 catalog produces.
	entry.Kind = ""
	if _, err := r.Resolve(context.Background(), entry); !errors.Is(err, pluginsource.ErrKindUnsupported) {
		t.Fatalf("entry with no kind: err = %v, want ErrKindUnsupported", err)
	}
}

// The publication host does not get to redirect the Keeper somewhere else over an
// unprotected channel. A 302 to http:// is refused by the netguard CheckRedirect, so
// the scheme allow-list covers every hop and not only the first — without which
// `validateBaseURL` would be a control on the address the operator typed rather than
// on the address the bytes come from.
func TestResolve_RefusesDowngradeRedirect(t *testing.T) {
	doc := moduleDoc()
	amd := stampedArtifact(t, "echo amd64", doc)

	// A second host that would serve the bytes over plain http if the redirect were
	// followed. Its digest is the CORRECT one, so nothing but the redirect guard can
	// refuse this resolve.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(amd)
	}))
	t.Cleanup(origin.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, origin.URL+"/elsewhere", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	t.Setenv(allowInsecureEnv, "1")

	r, cacheRoot := newTestResolver(t)
	_, err := r.Resolve(context.Background(), config.PluginCatalogEntry{
		Name:    testAlias,
		Kind:    sharedplugin.SourceKindArtifact,
		BaseURL: redirector.URL,
		Ref:     testRef,
		Artifacts: []config.PluginArtifactEntry{
			{OS: "linux", Arch: "amd64", Path: "redis_linux_amd64", SHA256: digestOf(amd)},
		},
	})
	if !errors.Is(err, pluginsource.ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable (redirect refused)", err)
	}
	if !strings.Contains(err.Error(), "non-https") {
		t.Errorf("err = %v, want the netguard downgrade refusal", err)
	}
	if _, serr := os.Stat(filepath.Join(cacheRoot, testAlias, "current")); serr == nil {
		t.Error("a redirected release was promoted to the active slot")
	}
}

// A path that a URL parser reads differently from a path reader is refused BEFORE any
// egress. The signed grant records the path verbatim and the fetcher concatenates it,
// so `pkg#v2` would sign one address and fetch another.
func TestResolve_RefusesPathThatWouldSignADifferentAddress(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	for _, bad := range []string{
		"redis_linux_amd64#v2",
		"redis_linux_amd64?raw=1",
		"a%2e%2e%2fredis",
		"a/./redis",
		"..\\redis",
		"../redis",
		"/redis",
		"https://evil.example/redis",
	} {
		r, cacheRoot := newTestResolver(t)
		entry := entryFor(rel, artifacts)
		entry.Artifacts = []config.PluginArtifactEntry{
			{OS: "linux", Arch: "amd64", Path: bad, SHA256: artifacts[0].SHA256},
		}
		_, err := r.Resolve(context.Background(), entry)
		if !errors.Is(err, pluginsource.ErrEntryInvalid) {
			t.Errorf("path %q: err = %v, want ErrEntryInvalid", bad, err)
		}
		if _, serr := os.Stat(filepath.Join(cacheRoot, testAlias)); serr == nil {
			t.Errorf("path %q: reached the cache", bad)
		}
	}
}

// A slot directory that exists but no longer reads back as its release is REPLACED on
// the next resolve, not treated as permanent. The release_id is a digest of the
// descriptor, so nothing else can legitimately hold that name; leaving the scrap in
// place would make `os.Rename` fail forever and the entry unresolvable until an
// operator deleted the directory by hand.
func TestResolve_ReplacesAnUnreadableSlot(t *testing.T) {
	rel, artifacts := twoPlatformRelease(t)
	r, cacheRoot := newTestResolver(t)
	entry := entryFor(rel, artifacts)

	first, err := r.Resolve(context.Background(), entry)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	// Damage the slot the way a truncated write would: keep the directory and the
	// descriptor, replace one artifact's bytes.
	victim := first.Contents.Artifacts[0].BinaryPath
	if werr := os.WriteFile(victim, []byte("not the approved bytes"), 0o755); werr != nil {
		t.Fatalf("damage slot: %v", werr)
	}
	if _, rerr := pluginhost.ReadSlotDir(first.SlotDir); rerr == nil {
		t.Fatal("the damaged slot still reads back — the fixture did not damage anything")
	}

	second, err := r.Resolve(context.Background(), entry)
	if err != nil {
		t.Fatalf("re-resolve after damage: %v", err)
	}
	if second.SlotDir != first.SlotDir {
		t.Errorf("release_id moved: %q → %q", first.SlotDir, second.SlotDir)
	}
	if _, rerr := pluginhost.ReadSlotDir(second.SlotDir); rerr != nil {
		t.Errorf("the re-resolved slot still does not read back: %v", rerr)
	}
	_ = cacheRoot
}

func TestKind(t *testing.T) {
	r, _ := newTestResolver(t)
	if r.Kind() != sharedplugin.SourceKindArtifact {
		t.Errorf("Kind = %q, want artifact", r.Kind())
	}
}

// GUARD: a release that fails its read-back does NOT replace a working `current`.
//
// `current` is what every reader resolves through, so promoting before the read-back
// would mean a slot that turns out to be unreadable has already displaced a good one —
// and the error the resolve returns would arrive after the damage. Move
// updateCurrentSymlink back above ReadSlotDir and this reds.
//
// The second release is made unreadable in the one way that survives the fetch: its two
// platform builds disclose DIFFERENT schema documents, which only the read-back can
// see (each file matches its own declared digest, so nothing earlier objects).
func TestResolve_FailedReadBackDoesNotMoveCurrent(t *testing.T) {
	first, artifacts := twoPlatformRelease(t)
	r, cacheRoot := newTestResolver(t)

	good, err := r.Resolve(context.Background(), entryFor(first, artifacts))
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	current := filepath.Join(cacheRoot, testAlias, "current")
	before, err := os.Readlink(current)
	if err != nil {
		t.Fatalf("readlink after the good resolve: %v", err)
	}

	// A second release under the same alias whose builds disagree about the schema.
	otherDoc := moduleDoc()
	otherDoc.Modules[0].Name = "config"
	amd := stampedArtifact(t, "echo amd64 v2", moduleDoc())
	arm := stampedArtifact(t, "echo arm64 v2", otherDoc)
	divergent := newRelease(t, map[string][]byte{
		"v2_linux_amd64": amd,
		"v2_linux_arm64": arm,
	})
	entry := config.PluginCatalogEntry{
		Name:    testAlias,
		Kind:    sharedplugin.SourceKindArtifact,
		BaseURL: divergent.baseURL(),
		Ref:     "v2.0.0",
		Artifacts: []config.PluginArtifactEntry{
			{OS: "linux", Arch: "amd64", Path: "v2_linux_amd64", SHA256: digestOf(amd)},
			{OS: "linux", Arch: "arm64", Path: "v2_linux_arm64", SHA256: digestOf(arm)},
		},
	}
	if _, err := r.Resolve(context.Background(), entry); err == nil {
		t.Fatal("a release whose builds disclose different schemas was accepted")
	}

	after, err := os.Readlink(current)
	if err != nil {
		t.Fatalf("current is gone after the failed resolve: %v", err)
	}
	if after != before {
		t.Errorf("current moved to the failed release: %q → %q", before, after)
	}
	if _, rerr := pluginhost.ReadSlotDir(good.SlotDir); rerr != nil {
		t.Errorf("the previously good slot no longer reads back: %v", rerr)
	}
}
