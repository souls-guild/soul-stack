package module

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/shared/config"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// How this run got its bytes, reported in the final event so the choice of
// transport is readable after the fact and not re-derived from the grant.
const (
	fetchViaSource = "source"
	fetchViaKeeper = "keeper"
)

// maxArtifactBytes caps a source pull. The bytes are read into memory (verify runs
// before anything touches the disk), so the cap is what keeps an untrusted source
// from deciding how much of this host's memory to take.
//
// It is Keeper's DEFAULT ceiling, not a shared one. Keeper's own is configurable
// (`plugins.max_artifact_size_mb`), it is enforced by the sender, and Soul has no
// config of its own to read here — so a cluster that raised its ceiling has artifacts
// this path refuses and FetchModule would serve. Refusing is the safe direction for a
// number that bounds an allocation an untrusted source controls; a Soul-side knob is
// the fix if that ever bites, not a wider default.
const maxArtifactBytes = int64(config.DefaultPluginMaxArtifactSizeMB) * 1024 * 1024

// sourceClientOpts — the client for a source pull.
//
// AllowPrivate is the one core.url default that cannot hold here, and the reason is
// where the URL comes from. A core.url task's URL is run input; a base_url is catalog
// configuration an Archon wrote and the grant carries, and the deployment this exists
// for is an artifact repository inside the perimeter, answering on an RFC1918 address
// the dial guard would refuse. The guard that actually protects the install is
// untouched: the bytes are Sigil-verified before they reach the disk, so a source that
// serves anything else earns a refusal rather than an exec.
var sourceClientOpts = util.HTTPClientOpts{AllowPrivate: true}

// errSourceUnusable marks a source failure Keeper cannot stand in for: what arrived
// was not what the grant promised, or the grant's address is not one this host can
// fetch from at all. Both are answered rather than routed around — see [Module.fetch].
var errSourceUnusable = errors.New("source is unusable for this grant")

// fetchResult — the bytes plus how they were obtained, for the final event.
type fetchResult struct {
	data     []byte
	via      string
	url      string
	warnings []string
}

// fetch performs step (3): the artifact's bytes, by whichever transport the grant
// calls for. It never writes anything — verify still runs on the return value.
//
// The rule, in the order it is applied:
//   - the grant carries no artifact rows → Keeper (FetchModule), exactly as before;
//   - it carries rows, one of them matches this host → the source;
//   - it carries rows and none matches this host → refusal, no fallback. A platform
//     the release does not cover is not a transport problem: Keeper holds no bytes
//     for it either, so falling back could only turn a clear answer into a vague one;
//   - the source did not ANSWER and a session exists → Keeper, with the source's
//     error kept as a warning. This is the host that lost its egress, not a mode
//     change;
//   - the source answered with something other than what the grant promised, or the
//     grant's own address is unusable → refusal ([errSourceUnusable]). Keeper can
//     stand in for a source that is down; it cannot stand in for one that is serving
//     the wrong bytes, and a fallback there would turn the one signal that a source
//     was tampered with into a warning line under a green run.
func (m *Module) fetch(ctx context.Context, alias string, rec *sharedhost.SigilRecord) (*fetchResult, error) {
	if len(rec.Artifacts) == 0 {
		data, err := fetchViaStream(ctx, alias, rec.BinarySHA256hex)
		if err != nil {
			return nil, err
		}
		return &fetchResult{data: data, via: fetchViaKeeper}, nil
	}

	osName, arch := m.hostPlatform()
	art, err := selectArtifact(rec.Artifacts, osName, arch)
	if err != nil {
		return nil, err
	}

	data, rawURL, serr := m.fetchFromSource(ctx, rec.BaseURL, art)
	if serr == nil {
		return &fetchResult{data: data, via: fetchViaSource, url: rawURL}, nil
	}
	if errors.Is(serr, errSourceUnusable) {
		return nil, serr
	}
	if _, ok := fetcherFrom(ctx); !ok {
		return nil, fmt.Errorf("%w (no EventStream session to fall back to)", serr)
	}
	data, kerr := fetchViaStream(ctx, alias, art.SHA256hex)
	if kerr != nil {
		return nil, fmt.Errorf("source: %w; keeper: %w", serr, kerr)
	}
	// No fetch_url on this one: the bytes came from Keeper, and a URL beside
	// fetch_via=keeper would name an address these bytes did not come from. The
	// source that was tried is in the warning, with the reason it was not used.
	return &fetchResult{
		data:     data,
		via:      fetchViaKeeper,
		warnings: []string{fmt.Sprintf("source unreachable (%v); fetched from Keeper instead", serr)},
	}, nil
}

// fetchViaStream pulls the artifact over the current EventStream session. Without a
// session there is no transport at all: push-mode `soul apply` carries no stream, and
// a grant that names no source leaves nothing else to try.
func fetchViaStream(ctx context.Context, alias, sha string) ([]byte, error) {
	fetcher, ok := fetcherFrom(ctx)
	if !ok {
		return nil, errors.New("FetchModule is unavailable in this run (no EventStream session; push mode is not supported)")
	}
	return fetchAll(ctx, fetcher, alias, sha)
}

// fetchFromSource downloads one artifact row and checks it against THAT row's sha256.
// Returns the bytes and the URL they came from (the URL is returned even on failure,
// so a diagnostic can name the address that did not answer).
//
// This digest check is not the Sigil verify — that one still runs afterwards, over
// the signed block. It is the cheaper half done at the point of arrival: bytes that
// are not what the catalog row promised never reach the verify step, let alone disk.
func (m *Module) fetchFromSource(ctx context.Context, base string, art sharedhost.SigilArtifact) ([]byte, string, error) {
	rawURL, err := artifactURL(base, art.Path)
	if err != nil {
		return nil, "", err
	}
	if verr := util.ValidateURL(rawURL); verr != nil {
		return nil, rawURL, fmt.Errorf("%w: artifact source %s: %w", errSourceUnusable, rawURL, verr)
	}

	reqCtx, cancel := context.WithTimeout(ctx, util.DefaultFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, rawURL, fmt.Errorf("%w: build request for %s: %w", errSourceUnusable, rawURL, err)
	}

	resp, err := m.NewClient(sourceClientOpts).Do(req)
	if err != nil {
		return nil, rawURL, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, rawURL, statusError(rawURL, resp.StatusCode)
	}

	// One byte past the cap: reading exactly the cap cannot tell a file at the limit
	// from one truncated at it, and a truncated artifact would fail verify with a
	// digest mismatch — the diagnostic for tampering, on a file that was merely large.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err != nil {
		return nil, rawURL, fmt.Errorf("download %s: %w", rawURL, err)
	}
	if int64(len(data)) > maxArtifactBytes {
		return nil, rawURL, fmt.Errorf("%w: artifact at %s exceeds the %d MiB limit",
			errSourceUnusable, rawURL, config.DefaultPluginMaxArtifactSizeMB)
	}

	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, art.SHA256hex) {
		return nil, rawURL, fmt.Errorf("%w: sha256 mismatch for %s: grant row %s/%s wants %s, source served %s",
			errSourceUnusable, rawURL, art.OS, art.Arch, art.SHA256hex, got)
	}
	return data, rawURL, nil
}

// statusError classifies a non-2xx answer, because "the source did not answer" and
// "the source answered no" are different facts and only the first is Keeper's to
// stand in for.
//
// A 4xx is an answer about THIS request: the row's path is not there (404), or the
// repository will not serve it anonymously (401/403, and v1 has no credentials to
// offer — source authentication is deferred by NIM-793). Keeper cannot make a catalog
// row right, so falling back would hide the typo behind a green run forever. The two
// exceptions are the 4xx codes that mean "ask again": a timed-out request and a rate
// limit are load, not a verdict. A 5xx is the source being down, which is the case
// the fallback exists for.
func statusError(rawURL string, code int) error {
	retryable := code == http.StatusRequestTimeout || code == http.StatusTooManyRequests
	if code >= 400 && code < 500 && !retryable {
		return fmt.Errorf("%w: %s answered %d", errSourceUnusable, rawURL, code)
	}
	return fmt.Errorf("fetch %s: unexpected status %d", rawURL, code)
}

// selectArtifact picks the row for this host's platform. The refusal names what the
// release does carry: the operator's next move is either a host of a platform the
// release covers or a release that covers this one, and neither is decidable from
// "no artifact for linux/arm64" alone.
func selectArtifact(arts []sharedhost.SigilArtifact, osName, arch string) (sharedhost.SigilArtifact, error) {
	for _, a := range arts {
		if strings.EqualFold(a.OS, osName) && strings.EqualFold(a.Arch, arch) {
			return a, nil
		}
	}
	return sharedhost.SigilArtifact{}, fmt.Errorf(
		"grant carries no artifact for %s/%s; the release covers %s", osName, arch, platformList(arts))
}

// platformList renders the platforms a grant carries, sorted: a diagnostic that
// reorders itself between two runs over the same grant reads like a change.
func platformList(arts []sharedhost.SigilArtifact) string {
	if len(arts) == 0 {
		return "no platform"
	}
	out := make([]string, 0, len(arts))
	for _, a := range arts {
		out = append(out, a.OS+"/"+a.Arch)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// artifactURL joins the grant's base_url with an artifact row's path.
//
// The path is joined, never interpolated, and one that could steer the address
// elsewhere is refused rather than normalized. The catalog form has no substitutions
// for that same reason (NIM-793): a URL is where executable bytes come from, and an
// expressive template there turns an address into a program. Refused: an absolute
// path or a URL of its own, a `.` or `..` segment, a query or a fragment — and the
// same, percent-encoded, since the server is what resolves the path and `%2e%2e%2f`
// arrives there as the traversal the literal check just refused.
func artifactURL(base, path string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("%w: it carries artifacts but no base_url to fetch them from", errSourceUnusable)
	}
	// Parsed rather than pattern-matched: `@` and `?` mean one thing in an authority
	// and another in a path, and a check that cannot tell them apart refuses
	// https://registry.internal/@souls/plugins for carrying credentials it does not.
	u, err := url.Parse(base)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: base_url %q is not a URL", errSourceUnusable, base)
	case u.Host == "":
		// `https:/repo.internal/plugins` — one slash — parses, and the scheme check
		// passes it. Left alone it reaches the client, which fails with "no Host in
		// request URL": a transport-shaped error for a typo, and therefore a run that
		// falls back to Keeper and hides the typo for as long as the catalog says so.
		return "", fmt.Errorf("%w: base_url %q names no host", errSourceUnusable, base)
	case u.User != nil:
		// Credentials are refused, not carried: source authentication is deferred by
		// NIM-793 (v1 reads an anonymous repository, since a credential means a secret
		// on every host and that is its own decision). Carrying them would also put a
		// password into fetch_url, i.e. into RunResult and OTel.
		return "", fmt.Errorf("%w: base_url must not carry credentials (source authentication is not supported yet)", errSourceUnusable)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		// Concatenating onto a query would put the row's filename inside the query
		// string and fetch the base instead — every row, silently, the same wrong file.
		return "", fmt.Errorf("%w: base_url %q must not carry a query or a fragment", errSourceUnusable, base)
	case path == "":
		return "", fmt.Errorf("%w: artifact row carries an empty path", errSourceUnusable)
	}

	decoded, err := url.PathUnescape(path)
	if err != nil {
		return "", fmt.Errorf("%w: artifact path %q is not valid percent-encoding", errSourceUnusable, path)
	}
	for _, form := range []string{path, decoded} {
		if perr := checkArtifactPath(form, path); perr != nil {
			return "", perr
		}
	}
	return strings.TrimRight(base, "/") + "/" + path, nil
}

// checkArtifactPath refuses a path that would address anything but a file under
// base_url. form is the spelling under test (the path as written, then percent-decoded);
// path is what the diagnostic quotes, so both refusals name what the operator wrote.
func checkArtifactPath(form, path string) error {
	switch {
	case strings.Contains(form, "://") || strings.HasPrefix(form, "/"):
		return fmt.Errorf("%w: artifact path %q must be relative to base_url", errSourceUnusable, path)
	case strings.ContainsAny(form, "?#"):
		return fmt.Errorf("%w: artifact path %q must not carry a query or a fragment", errSourceUnusable, path)
	case strings.Contains(form, `\`):
		// Go does not treat a backslash as a separator, so `..\..\x` would pass the
		// segment scan below and reach a server that does treat it as one. No release
		// artifact is named with a backslash; refusing the character is cheaper than
		// reasoning about which server normalizes it.
		return fmt.Errorf("%w: artifact path %q must not contain a backslash", errSourceUnusable, path)
	}
	for _, seg := range strings.Split(form, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("%w: artifact path %q must not contain a %q segment", errSourceUnusable, path, seg)
		}
	}
	return nil
}

// hostPlatform returns the (os, arch) an artifact row must match to run here: the
// host's OWN Soulprint facts (ADR-018), with the running binary as the fallback for a
// factless host — the primary→fallback shape util.ResolvePkgMgr already uses.
//
// os.family is a DISTRO family on Linux (debian / rhel / alpine / arch) where a row
// names an operating system (linux / darwin / windows), so the Linux families collapse
// to one value. Any other family is already a GOOS: Soulprint fills family from
// runtime.GOOS on every system with no /etc/os-release to read.
func (m *Module) hostPlatform() (osName, arch string) {
	osName = runtime.GOOS
	switch m.facts.OSFamily {
	case "":
		// No fact — an unreadable os-release, or push mode with no collector.
	case "debian", "rhel", "alpine", "arch":
		osName = "linux"
	default:
		osName = m.facts.OSFamily
	}

	arch = m.facts.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	return osName, arch
}
