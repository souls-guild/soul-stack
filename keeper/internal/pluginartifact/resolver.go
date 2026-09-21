// Package pluginartifact — the artifact-kind plugin source (NIM-793): a published,
// already-built release fetched over https and pinned by per-file digests.
//
// It is the second implementation of [pluginsource.Provider], beside
// [plugingit.Resolver], and the reason that interface exists.
//
// # What it does and, more to the point, what it is FOR
//
// This package is the Keeper half of "a plugin arrives from its source": it makes the
// signed grant carry a per-platform address and digest, which is what a Soul needs in
// order to fetch the bytes itself (the Soul half is `soul/internal/coremod/module`,
// NIM-796).
//
// Keeper resolves and signs, so it fetches the release itself: it must check the
// digests the operator declared against what the source actually serves, and it must
// read the disclosure out of the artifact before an Archon can approve it. A grant
// asserting a digest nobody re-computed would be an approval of a claim rather than of
// bytes.
//
// So the traffic here is a resolve, once per release, and never a delivery path. The
// delivery path is the Soul fetching from the same source with the signed digest in
// hand, which is what takes artifact bytes off the EventStream they used to share with
// run dispatch.
//
// # The source is untrusted by construction
//
// Nothing here treats the publication host as authoritative. A compromised Nexus can
// serve other bytes; it gets a refused resolve, because the digest it has to match is
// the one the operator wrote in the catalog and not one the response carries. That is
// also why there is no path template: the file list is enumerated, so the set of
// addresses this package will fetch is fixed by the catalog rather than computed from
// it.
//
// egress — HIGH security risk, same class as the git resolver's. Bounded four ways: by
// scheme ([validateBaseURL] — https, with http only behind a dev flag — and
// [guardedClient]'s redirect guard, which is what extends that rule past the first
// hop), by SHAPE (the file list is enumerated, so the set of addresses this package
// will fetch is fixed by the catalog rather than computed from it), by time (the
// entry's context deadline), and by volume (`plugins.max_artifact_size_mb` per file,
// enforced with a LimitReader and not with Content-Length, which the source controls).
//
// It is NOT bounded by the netguard SSRF dial guard, deliberately and symmetrically
// with the Soul half — see [guardedClient]. A fetched artifact is NOT executed and NOT
// marked trusted: trust is given separately by `plugin.allow` + Sigil.
package pluginartifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/netguard"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginsource"
)

// bytesPerMiB — multiplier MiB→bytes, local copy of the unexported one in
// shared/config, to default the limit in bytes without an import workaround.
const bytesPerMiB = 1024 * 1024

// Resolver is the artifact-kind provider. cacheRoot is the root of the slot cache;
// unlike the git resolver there is no work root, because there is no checkout — a
// staging directory beside the slot is the whole of the intermediate state.
type Resolver struct {
	cacheRoot       string
	client          *http.Client
	timeout         time.Duration
	maxArtifactSize int64
	logger          *slog.Logger
}

// maxArtifactRedirects — the redirect budget of one artifact fetch. A publication
// root normally redirects zero or once (a CDN edge); three leaves room for that and
// stops a chain from becoming a crawl.
const maxArtifactRedirects = 3

// NewResolver constructs the provider. timeout ≤ 0 → [config.DefaultPluginFetchTimeout];
// maxArtifactSize ≤ 0 → [config.DefaultPluginMaxArtifactSizeMB]; client nil →
// [guardedClient] (the timeout is applied per entry through the context, so the client
// carries none of its own and a slow release does not get a second, different budget).
func NewResolver(cacheRoot string, timeout time.Duration, maxArtifactSize int64, client *http.Client, logger *slog.Logger) *Resolver {
	if timeout <= 0 {
		timeout = config.DefaultPluginFetchTimeout
	}
	if maxArtifactSize <= 0 {
		maxArtifactSize = int64(config.DefaultPluginMaxArtifactSizeMB) * bytesPerMiB
	}
	if client == nil {
		client = guardedClient()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{
		cacheRoot:       cacheRoot,
		client:          client,
		timeout:         timeout,
		maxArtifactSize: maxArtifactSize,
		logger:          logger,
	}
}

// guardedClient builds the HTTP client this package fetches executable bytes with.
//
// # The redirect guard, always on
//
// [netguard.NewCheckRedirect] refuses a hop to anything but https and caps the chain.
// Without it the scheme allow-list in [validateBaseURL] would apply to the FIRST hop
// only, and a publication host answering `302 → http://…` would deliver the artifact
// over a channel that can be rewritten in flight. The digest check would still catch a
// substitution, but a control that only notices afterwards is not the control this one
// is. Nothing about any deployment needs a downgrade hop, so there is no flag for it.
//
// # The SSRF dial guard, deliberately NOT on
//
// [netguard.GuardedDialContext] refuses loopback / RFC1918 / link-local addresses, and
// an internal artifact repository is exactly an RFC1918 address. Installing it here
// would make the Keeper refuse to resolve the address this whole path exists to reach:
// `https://nexus.internal/plugins/redis` would fail at dial, no slot would be written,
// and `plugin.allow` would answer "not in cache" forever.
//
// The reasoning is the Soul half's, and it holds identically here (see
// `soul/internal/coremod/module`.sourceClientOpts): a base_url is CATALOG
// CONFIGURATION an Archon wrote into keeper.yml, not run input a task supplied. The
// guard exists to stop an untrusted VALUE from steering an outbound request; this
// value is the operator's own. Keeper and Soul fetch the same URL, so a guard on one
// side and not the other would not be a stronger system — it would be a broken one.
//
// What still bounds this path: https only, the redirect guard above, the enumerated
// file list (no computed addresses), the per-file size cap, the entry deadline, and —
// the one that actually decides anything — the digest each file must match before it
// is written, and the Sigil signature before it is ever run.
//
// The residual is a source that redirects to an internal HTTPS address — a blind,
// GET-only, credential-free request of at most [maxArtifactRedirects] hops whose body
// never reaches the source that steered it (it is streamed to staging, hashed, and the
// staging directory is removed on a digest mismatch). Two things bound it beyond that,
// and the second is the one carrying the weight: the operator chose the source, and the
// hop must present a certificate the SYSTEM TRUST STORE accepts for the name it
// redirected to — the transport is not `InsecureSkipVerify` anywhere on this path.
// Cloud metadata services are HTTP-only, so the classic target stays closed by the
// scheme rule rather than by any address check. Closing the rest would mean refusing
// the internal registries this feature exists for.
func guardedClient() *http.Client {
	return &http.Client{
		CheckRedirect: netguard.NewCheckRedirect(maxArtifactRedirects),
		Transport:     http.DefaultTransport.(*http.Transport).Clone(),
	}
}

// Kind implements [pluginsource.Provider].
func (r *Resolver) Kind() string { return sharedplugin.SourceKindArtifact }

// Resolve fetches one published release into an immutable slot.
//
// Flow:
//
//  1. the entry is checked against the artifact shape: a base URL on an allowed
//     scheme, a ref, and a file list that can be signed over
//     ([sharedhost.CanonicalArtifacts] — this is where a duplicate `(os, arch)` and a
//     malformed digest are refused, before any egress);
//  2. release_id := sha256 of the canonical descriptor. One release, one immutable
//     slot directory — the role commit_sha plays for the git kind, derived rather than
//     taken from the ref, because a tag can be moved and a slot named by a movable
//     label is not immutable;
//  3. if `<cacheRoot>/<alias>/<release_id>/` already reads back as this release, the
//     download is skipped entirely;
//  4. otherwise every file is fetched into a staging directory on the same filesystem,
//     each verified against its declared digest as it streams, then the descriptor is
//     written and the staging directory is atomically renamed into the slot;
//  5. `current` is atomically swapped to the slot;
//  6. the slot is read back with the SAME code `plugin.allow` reads it with — which is
//     what re-derives the digests from disk and enforces one disclosure per release.
//
// Every failure leaves the cache as it was: a partial release is removed with its
// staging directory rather than promoted.
func (r *Resolver) Resolve(ctx context.Context, e config.PluginCatalogEntry) (pluginsource.Resolved, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	alias := e.Name
	if err := pluginsource.ValidateAlias(alias); err != nil {
		return pluginsource.Resolved{}, err
	}
	// An entry of another kind must not be fetched over https just because it also
	// carries a name and a ref: `source` and `base_url` are different assertions, and
	// an entry declaring one must not be resolved through the other. Symmetric with
	// the git resolver's guard, and stated rather than left to the field checks below
	// — those would refuse an entry with no base_url for the wrong reason and accept a
	// `kind: git` entry that happened to carry one.
	if k := e.ResolvedKind(); k != sharedplugin.SourceKindArtifact {
		return pluginsource.Resolved{}, fmt.Errorf("%w: %q is not an artifact source", pluginsource.ErrKindUnsupported, k)
	}
	base, err := r.entryBaseURL(e)
	if err != nil {
		return pluginsource.Resolved{}, err
	}
	if e.Ref == "" {
		return pluginsource.Resolved{}, fmt.Errorf("%w: empty ref", pluginsource.ErrEntryInvalid)
	}

	release, err := releaseOf(e, base)
	if err != nil {
		return pluginsource.Resolved{}, err
	}
	releaseID, err := pluginhost.ReleaseID(release)
	if err != nil {
		return pluginsource.Resolved{}, fmt.Errorf("%w: %v", pluginsource.ErrEntryInvalid, err)
	}

	pluginDir := filepath.Join(r.cacheRoot, alias)
	dst := filepath.Join(pluginDir, releaseID)
	if !slotHolds(dst, release) {
		if err := r.materialize(ctx, pluginDir, dst, base, alias, release); err != nil {
			return pluginsource.Resolved{}, err
		}
	}

	// Read the slot back BEFORE promoting it. `current` is what every reader resolves
	// through, so swapping it first would mean a slot that fails its read-back — a
	// flipped byte, two platforms disclosing different documents — has already replaced
	// a previously good release, and the error this returns arrives after the damage.
	// Promotion is the last step, so a failed resolve leaves the cache as it was.
	contents, err := pluginhost.ReadSlotDir(dst)
	if err != nil {
		return pluginsource.Resolved{}, err
	}
	if err := updateCurrentSymlink(pluginDir, releaseID); err != nil {
		return pluginsource.Resolved{}, err
	}

	return pluginsource.Resolved{
		Alias:  alias,
		Kind:   sharedplugin.SourceKindArtifact,
		Source: base,
		Ref:    e.Ref,
		// No Origin: this kind has no commit to pin. Its provenance is the signed
		// (source, ref) plus a digest per file, all of which the grant carries — a
		// second, unsigned marker would only be a claim.
		Origin:   "",
		SlotDir:  dst,
		Contents: contents,
	}, nil
}

// entryBaseURL validates the entry's `base_url` and returns it without a trailing
// slash, so joining a path is one rule and not two.
func (r *Resolver) entryBaseURL(e config.PluginCatalogEntry) (string, error) {
	if e.BaseURL == "" {
		return "", fmt.Errorf("%w: empty base_url", pluginsource.ErrEntryInvalid)
	}
	if err := validateBaseURL(e.BaseURL); err != nil {
		return "", err
	}
	return strings.TrimRight(e.BaseURL, "/"), nil
}

// releaseOf turns a catalog entry's rows into a slot descriptor, canonically ordered
// and validated. This is the fail-closed gate the acceptance criteria name: an entry
// with no row at all, with two rows for one platform, or with a path that would leave
// the base URL, never reaches the network and never becomes a grant.
func releaseOf(e config.PluginCatalogEntry, base string) (pluginhost.Release, error) {
	if len(e.Artifacts) == 0 {
		return pluginhost.Release{}, fmt.Errorf("%w: no artifacts declared", pluginsource.ErrEntryInvalid)
	}
	rows := make([]sharedhost.SigilArtifact, 0, len(e.Artifacts))
	for _, a := range e.Artifacts {
		if !config.ValidPluginArtifactPath(a.Path) {
			return pluginhost.Release{}, fmt.Errorf(
				"%w: artifact (os=%q arch=%q) path %q must name a file under base_url",
				pluginsource.ErrEntryInvalid, a.OS, a.Arch, a.Path)
		}
		rows = append(rows, sharedhost.SigilArtifact{
			OS: a.OS, Arch: a.Arch, Path: a.Path, SHA256: strings.ToLower(a.SHA256),
		})
	}
	canon, err := sharedhost.CanonicalArtifacts(rows)
	if err != nil {
		return pluginhost.Release{}, fmt.Errorf("%w: %v", pluginsource.ErrEntryInvalid, err)
	}
	for _, a := range canon {
		if !a.Platformed() {
			// An unplatformed row is the git kind's shape. Accepting it here would
			// produce a release that answers for every platform out of one published
			// file, which is not what a published release is.
			return pluginhost.Release{}, fmt.Errorf(
				"%w: an artifact entry must state os and arch on every row", pluginsource.ErrEntryInvalid)
		}
	}
	return pluginhost.Release{
		Kind: sharedplugin.SourceKindArtifact,
		// The address the fetch will actually go to, normalized the way the fetch
		// composes it. `plugin.allow` compares the operator's asserted `source`
		// against this before signing, so an approval cannot name an origin the
		// Keeper never reached.
		Source:    base,
		Artifacts: pluginhost.ReleaseArtifactsOf(canon),
	}, nil
}

// slotHolds reports whether dst already holds exactly this release. The release_id is
// derived from the descriptor, so a directory bearing that name and reading back
// cleanly IS this release — but it is read back rather than assumed, which is what
// turns a half-written or tampered slot into a re-fetch instead of an approval.
func slotHolds(dst string, release pluginhost.Release) bool {
	contents, err := pluginhost.ReadSlotDir(dst)
	if err != nil || contents == nil || contents.Kind != release.Kind {
		return false
	}
	if len(contents.Artifacts) != len(release.Artifacts) {
		return false
	}
	for i, want := range release.Artifacts {
		got := contents.Artifacts[i]
		if got.OS != want.OS || got.Arch != want.Arch || got.Path != want.Path || got.SHA256 != want.SHA256 {
			return false
		}
	}
	return true
}

// materialize builds the whole release in a staging directory and promotes it with one
// rename. Nothing partially-fetched is ever reachable through the slot: the rename is
// the only moment the release becomes visible, and a failure before it removes the
// staging directory.
func (r *Resolver) materialize(ctx context.Context, pluginDir, dst, base, alias string, release pluginhost.Release) error {
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return fmt.Errorf("pluginartifact: mkdir plugin dir %q: %w", pluginDir, err)
	}
	staging := filepath.Join(pluginDir, ".staging-"+randSuffix())
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("pluginartifact: mkdir staging %q: %w", staging, err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }

	for _, a := range release.Artifacts {
		platformDir := filepath.Join(staging, a.Dir())
		if err := os.MkdirAll(platformDir, 0o755); err != nil {
			cleanup()
			return fmt.Errorf("pluginartifact: mkdir %q: %w", platformDir, err)
		}
		// The artifact takes the alias as its filename in the slot — it has no name
		// of its own, and the readers find it by "the one executable here" anyway.
		if err := r.fetchArtifact(ctx, base+"/"+a.Path, filepath.Join(platformDir, alias), a.SHA256); err != nil {
			cleanup()
			return err
		}
	}
	if err := pluginhost.WriteRelease(staging, release); err != nil {
		cleanup()
		return err
	}

	if err := os.Rename(staging, dst); err != nil {
		// Race of two resolvers on the same release_id: the winner already created
		// dst — not an error, the slot is immutable and the content is identical.
		if slotHolds(dst, release) {
			cleanup()
			return nil
		}
		// dst exists and is NOT this release: a half-written slot from a resolve that
		// died between rename and fsync, or one whose bytes have since been damaged.
		// The release_id is a digest of the descriptor, so nothing else can legitimately
		// hold this name — the directory is scrap, and removing it is what keeps the
		// entry from being permanently unresolvable (rename into a non-empty directory
		// fails forever, and every later resolve would repeat the same failure).
		if rmErr := os.RemoveAll(dst); rmErr != nil {
			cleanup()
			return fmt.Errorf("pluginartifact: remove unreadable slot %q: %w", dst, rmErr)
		}
		if err := os.Rename(staging, dst); err != nil {
			cleanup()
			return fmt.Errorf("pluginartifact: atomic rename staging→slot %q: %w", dst, err)
		}
	}
	return nil
}

// fetchArtifact downloads one file to dst and refuses it unless its bytes hash to
// wantSHA.
//
// The digest is computed WHILE streaming, so the file is never read twice and a
// mismatch is known before the caller can do anything with it. The size cap comes from
// a LimitReader rather than from Content-Length: the header is the source's claim, and
// the source is the thing being bounded.
func (r *Resolver) fetchArtifact(ctx context.Context, rawURL, dst, wantSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("%w: build request for %s: %v", pluginsource.ErrSourceUnavailable, rawURL, err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: GET %s: %v", pluginsource.ErrSourceUnavailable, rawURL, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: GET %s: HTTP %d", pluginsource.ErrSourceUnavailable, rawURL, resp.StatusCode)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("pluginartifact: create %q: %w", dst, err)
	}
	h := sha256.New()
	// +1 byte: reading exactly maxArtifactSize+1 means the source is over the cap.
	written, cerr := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, r.maxArtifactSize+1))
	if cerr != nil {
		_ = out.Close()
		return fmt.Errorf("%w: read %s: %v", pluginsource.ErrSourceUnavailable, rawURL, cerr)
	}
	if written > r.maxArtifactSize {
		_ = out.Close()
		return fmt.Errorf("%w: %s > limit %d bytes", pluginsource.ErrArtifactTooLarge, rawURL, r.maxArtifactSize)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("pluginartifact: fsync %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("pluginartifact: close %q: %w", dst, err)
	}
	// OpenFile's mode is subject to umask — set it explicitly.
	if err := os.Chmod(dst, 0o755); err != nil {
		return fmt.Errorf("pluginartifact: chmod %q: %w", dst, err)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("%w: %s served %s, the catalog declares %s",
			pluginsource.ErrSourceUnavailable, rawURL, got, wantSHA)
	}
	return nil
}

// updateCurrentSymlink atomically swaps `<pluginDir>/current` → releaseID: it creates a
// temp symlink beside it and renames it over `current` (a symlink rename is atomic
// within a directory). The target is relative, so the slot moves with its plugin dir.
//
// Duplicated from the git resolver rather than shared: the two write the same link for
// their own reasons, and a shared helper would put the cache layout of both kinds
// behind one function that neither package owns.
func updateCurrentSymlink(pluginDir, releaseID string) error {
	tmp := filepath.Join(pluginDir, ".current-"+randSuffix())
	if err := os.Symlink(releaseID, tmp); err != nil {
		return fmt.Errorf("pluginartifact: create temp symlink in %q: %w", pluginDir, err)
	}
	if err := os.Rename(tmp, filepath.Join(pluginDir, pluginhost.CurrentLink)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pluginartifact: atomic swap current symlink in %q: %w", pluginDir, err)
	}
	return nil
}

// randSuffix — short random suffix for staging/temp names, so two resolvers do not
// pick the same one. crypto/rand is not for strength here, only for collision-freedom.
func randSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// allowInsecureEnv — the dev/test escape hatch permitting an `http://` base URL, and
// nothing else. It does NOT govern reaching private addresses: an internal registry on
// an RFC1918 address is the normal production case here, so those are always reachable
// (see [guardedClient]).
//
// The SAME flag the git resolver uses for `file://`, deliberately not a second one: it
// means "this Keeper may reach a source over an unprotected transport", and an operator
// who decided that once should not decide it again per transport.
//
// ⚠ The Soul side has NO equivalent: `util.ValidateURL` there is https-only. So a grant
// signed on an http source under this flag resolves on the Keeper and is refused by
// every Soul. That is a dev-stand-only asymmetry and it is the safe direction, but it
// is why this flag belongs on a dev stand and nowhere else.
const allowInsecureEnv = "SOUL_STACK_ALLOW_FILE_REPOS"

// insecureSourcesAllowed reports whether the dev/test escape hatch is on.
func insecureSourcesAllowed() bool { return os.Getenv(allowInsecureEnv) == "1" }

// validateBaseURL checks the publication root's scheme and shape.
//
// `https://` in production, full stop: these are executable bytes and the digest check
// is a second line, not a substitute for a transport that cannot be rewritten in
// flight. `http://` passes only under [allowInsecureEnv]. A URL with a query, a
// fragment or credentials is refused — the address is meant to be a directory the
// files hang off, and anything else there is a signal the operator is describing
// something this resolver will not do.
func validateBaseURL(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("%w: base_url %q: %v", pluginsource.ErrEntryInvalid, base, err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !insecureSourcesAllowed() {
			return fmt.Errorf("%w: http:// base_url is forbidden in prod (set %s=1 for dev/test): %q",
				pluginsource.ErrEntryInvalid, allowInsecureEnv, base)
		}
	default:
		return fmt.Errorf("%w: unsupported base_url scheme %q (allowed: https://)",
			pluginsource.ErrEntryInvalid, base)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: base_url %q has no host", pluginsource.ErrEntryInvalid, base)
	}
	if u.User != nil {
		return fmt.Errorf("%w: base_url %q must not carry credentials", pluginsource.ErrEntryInvalid, base)
	}
	// Refuse `?` and `#` on the RAW string, not through the parsed fields. A base
	// ending in a bare `?` parses to an EMPTY RawQuery (the marker lives in
	// url.URL.ForceQuery), and a bare `#` to an empty Fragment — so a field check
	// passes both, and then `base + "/" + path` puts the artifact's FILENAME in the
	// query or the fragment. Every row of the release would fetch the same address:
	// exactly the "sign one address, fetch another" property the path rule exists to
	// prevent, arriving through the other half of the URL.
	//
	// It is also the shape that makes a grant permanently uninstallable — Keeper signs
	// it, the Soul-side check refuses it, and that branch has no fallback.
	if strings.ContainsAny(base, "?#") {
		return fmt.Errorf("%w: base_url %q must be a plain publication root (no query or fragment)",
			pluginsource.ErrEntryInvalid, base)
	}
	return nil
}
