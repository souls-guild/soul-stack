package harness

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The local stand-in for github.com's release downloads: a cache on disk plus an
// HTTPS server in the harness process that serves it under the upstream layout.
//
// NIM-542, and the reason it is a mirror rather than a stub: what the product
// fetches here is the REAL tarball, byte for byte, so the checksums a service
// pins still verify, `core.archive.extracted` still
// unpacks a real tree and the exporters that come out still run. Hermetic and
// fake are different properties; this buys the first without the second.
//
// Untagged on purpose. A mirror that only compiles with docker present cannot be
// exercised by the docker-free guards, and those guards are the ones that run in
// `make e2e-live-gate`'s first step, before twenty minutes are spent.

// artifactCacheEnv — where the cache lives, when the default is wrong (CI with a
// warm shared volume, or a machine whose $HOME is not writable).
const artifactCacheEnv = "SOUL_STACK_E2E_ARTIFACT_CACHE"

// artifactOfflineEnv — set to 1/true to forbid the primer from reaching upstream.
//
// This is how the ticket's acceptance is DEMONSTRATED rather than asserted: with
// a warm cache and this set, a gate run cannot touch github.com even by accident,
// and a missing entry fails by name instead of quietly restoring the dependency.
const artifactOfflineEnv = "SOUL_STACK_E2E_ARTIFACT_OFFLINE"

// artifactFetchTimeout — per-artifact budget for the one-time priming fetch.
// Generous: this runs at most once per machine per version bump, and a partial
// download that got written into the cache would be worse than a slow one.
const artifactFetchTimeout = 10 * time.Minute

// upstreamProbeTimeout — budget for the reachability check the real-path test
// runs before it builds a stand. Short: it is answering "is the public internet
// there", not "is it fast".
const upstreamProbeTimeout = 20 * time.Second

// probeUpstreamArtifacts asks upstream for the first byte of every tarball and
// reports what could not be had, or "" when they all could.
//
// The whole point of running it BEFORE the stand. When github is unreachable the
// question "was this the environment or the code?" has a clean answer only while
// nothing of the product has run yet — afterwards the same outage arrives as a
// failed apply, indistinguishable at a glance from a real regression. This is why
// the non-gate real-path test skips instead of failing: an unreachable upstream is
// a fact about the machine, and a fact about the machine must not be filed as a
// finding about the code (NIM-406, NIM-542).
//
// A ranged GET rather than HEAD: release CDNs answer HEAD inconsistently, and one
// byte costs nothing.
func probeUpstreamArtifacts(cat []upstreamArtifact, timeout time.Duration) string {
	return probeUpstreamArtifactsWith(&http.Client{Timeout: timeout}, cat)
}

// probeUpstreamArtifactsWith is probeUpstreamArtifacts with the client injected,
// so the guards can point it at a local stand-in instead of the real internet —
// a reachability check whose own test needs the network would prove nothing.
func probeUpstreamArtifactsWith(client *http.Client, cat []upstreamArtifact) string {
	var bad []string
	for _, a := range cat {
		req, err := http.NewRequest(http.MethodGet, a.upstreamURL(), nil)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", a.upstreamURL(), err))
			continue
		}
		req.Header.Set("Range", "bytes=0-0")
		resp, err := client.Do(req)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", a.upstreamURL(), err))
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			bad = append(bad, fmt.Sprintf("%s: status %s", a.upstreamURL(), resp.Status))
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return "upstream release downloads are not reachable from this machine:\n  " +
		strings.Join(bad, "\n  ") +
		"\n  This is the environment, not a defect: nothing of this repository has run yet." +
		"\n  The blocking gate does not depend on it — `make e2e-live-gate` serves these" +
		"\n  tarballs from a local mirror (NIM-542). This test covers the real path and is" +
		"\n  deliberately outside that gate, so it steps aside instead of going red."
}

// RequireUpstreamArtifacts skips the test unless every catalogued tarball can be
// fetched from its real upstream. For the non-gate test that keeps the public
// github path covered.
func RequireUpstreamArtifacts(t *testing.T) {
	t.Helper()
	if msg := probeUpstreamArtifacts(artifactCatalog(), upstreamProbeTimeout); msg != "" {
		t.Skipf("%s", msg)
	}
}

// ReportUpstreamIfItWentAway prints a decisive line when a real-path test has
// failed and upstream is no longer reachable.
//
// The pre-flight above covers the ordinary case — upstream down before anything
// starts. This covers the other one: reachable at the start, gone twenty minutes
// later, with the failure arriving as a failed apply that looks exactly like a
// regression. It cannot un-fail the test, and does not try to: this test is
// outside the blocking gate precisely so that its red costs a reader's attention
// rather than a release. What it must do is tell that reader which of the two
// they are looking at.
func ReportUpstreamIfItWentAway(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}
	if msg := probeUpstreamArtifacts(artifactCatalog(), upstreamProbeTimeout); msg != "" {
		t.Logf("READ THE FAILURE ABOVE AS ENVIRONMENT, NOT AS A DEFECT.\n%s\n"+
			"  Upstream answered before this test started and does not answer now, so the\n"+
			"  apply failure above is the network dropping out mid-run.", msg)
		return
	}
	t.Logf("Upstream is still reachable, so the failure above is NOT the network: read it as a finding.")
}

// artifactMirror — a read-only HTTPS server over the cache directory, plus a
// count of what it actually served.
//
// The counter is the point. `vars/99-…yaml` overriding three keys is a claim, and
// a claim about a YAML file three layers away from where it is read is exactly
// the kind that holds until it doesn't: rename the file, add a `_stack.yaml` to
// the example, rename a var, and the overlay contributes nothing at all — the
// create still passes, from github, and the gate goes back to depending on the
// network with nobody the wiser. missingArtifacts turns that silence into a red
// test.
type artifactMirror struct {
	baseURL string
	dir     string
	srv     *http.Server

	// caPEM — the CA that signed the mirror's server certificate. Every soul
	// container is handed this and runs update-ca-certificates before it is
	// started (SpawnSoulContainer, step 4b): the mirror speaks https, and nothing
	// signed it but us.
	caPEM []byte

	mu       sync.Mutex
	hits     map[string]int
	serveErr error
}

// artifactMirrorHealthPath — the one path the container may ask for that does
// NOT count as a hit.
//
// SpawnSoulContainer fetches it before soul starts, to turn "the container
// cannot reach or cannot trust the mirror" into a named failure at spawn time
// instead of a fetch error twenty minutes into a create. It must stay outside
// the accounting: `missingArtifacts` is the guard that a create really went
// through the mirror, and a pre-flight that counted would satisfy that guard by
// itself — the mirror would be reported as used on every run, including the runs
// where the overlay contributed nothing and the tarballs came from github.
const artifactMirrorHealthPath = "/_e2e-live/mirror-alive"

// artifactMirrorHealthBody — what the health path answers with.
//
// One constant, three readers: the handler writes it, the unit test asserts the
// handler wrote it, and SpawnSoulContainer's pre-flight asserts the bytes that
// came back over the wire are these. That last one is the runtime half of the
// collision guard in artifactmirror_test.go — if artifactMirrorHealthPath ever
// names a catalogued tarball, the handler answers it from this string and
// returns before the file server is reached, so the product would get a 200
// carrying this sentence where it expected a tarball. The static guard catches
// that without docker; this catches it if the static one is ever weakened.
const artifactMirrorHealthBody = "soul-stack e2e-live artifact mirror"

// caAddedCount reads the count out of what `update-ca-certificates` printed, or
// -1 if it printed no such line — which is itself a finding, because printing
// one is the command's whole contract with us (its exit code is 0 either way).
//
// A substring test cannot do this job, and the obvious ones are wrong in
// opposite directions: `strings.Contains(out, "1 added")` is true for "21 added"
// and false for "2 added", and the repair-by-inversion, `!Contains(out,
// "0 added")`, is false for "10 added". Read the number.
//
// It lives in this file rather than next to its caller because container.go is
// behind //go:build e2e_live: there it could only be exercised by a live run,
// and a parser is exactly the kind of thing that should be pinned by a table of
// strings in the docker-free step.
func caAddedCount(out string) int {
	m := caAddedCountRe.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

// caAddedCountRe — `update-ca-certificates` ends with a line of the form
// "N added, M removed; done." (its line 199). Anchored to the start of a line so
// a path or a hostname containing digits earlier in the output cannot supply the
// number.
var caAddedCountRe = regexp.MustCompile(`(?m)^(\d+) added`)

// artifactCacheDir — the cache root: $SOUL_STACK_E2E_ARTIFACT_CACHE, else
// $XDG_CACHE_HOME/soul-stack/e2e-live/artifacts, else ~/.cache/… . Outside the
// repository and outside $TMPDIR both: it must survive `go clean`, `git clean`
// and a reboot, or "hermetic" would mean "downloads once per run" instead of
// "downloads once".
func artifactCacheDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(artifactCacheEnv)); dir != "" {
		return dir, nil
	}
	base := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("artifact cache: no %s and no home directory: %w", artifactCacheEnv, err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "soul-stack", "e2e-live", "artifacts"), nil
}

// PrimeArtifactCache fetches every catalogued artifact that is not already in the
// cache, and reports where the cache is. This is what `make e2e-live-artifacts`
// runs and what the live gate runs as its first step.
//
// NewStack primes the same cache on its own, so this is never REQUIRED — it exists
// so that the one network-dependent moment of an otherwise hermetic tier happens
// up front, named, in half a minute, instead of twenty minutes in as a failed
// fetch inside a container. It is also how a machine gets primed deliberately
// before it goes offline.
func PrimeArtifactCache() (dir string, err error) {
	dir, err = artifactCacheDir()
	if err != nil {
		return "", err
	}
	return dir, ensureArtifactCache(dir, artifactCatalog())
}

// artifactOffline reports whether the primer is forbidden to reach upstream.
func artifactOffline() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(artifactOfflineEnv)))
	return v == "1" || v == "true" || v == "yes"
}

// ensureArtifactCache makes sure every artifact in cat is in dir with the right
// bytes, fetching the ones that are not.
//
// Verification is by digest on the way IN, and a file whose digest is wrong is
// removed rather than left. It has to be ours: vector and redis-exporter hand
// `checksum:` to core.url and would reject bad bytes inside the container, but
// node-exporter's install task declares no checksum by design, so a truncated
// entry there sails past the fetch and lands as a failed unpack. Either way it
// arrives as `--- FAIL` on a create — a finding about the code, for a truncated
// download.
func ensureArtifactCache(dir string, cat []upstreamArtifact) error {
	for _, a := range cat {
		dst := filepath.Join(dir, filepath.FromSlash(a.cacheRel()))
		switch ok, err := artifactCached(dst, a.sha256); {
		case err != nil:
			return err
		case ok:
			continue
		}
		if artifactOffline() {
			return fmt.Errorf("artifact cache: %s is missing from %s and %s forbids fetching it.\n"+
				"  Prime the cache on a machine that can reach %s:\n"+
				"      make e2e-live-artifacts",
				a.cacheRel(), dir, artifactOfflineEnv, a.upstreamBase)
		}
		if err := fetchArtifact(a, dst); err != nil {
			return err
		}
	}
	return nil
}

// artifactCached — is the file there with the expected digest? A file that is
// there with the WRONG digest is deleted and reported as absent: leaving it would
// make the next run fail the same way for a reason it cannot see.
func artifactCached(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("artifact cache: open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("artifact cache: read %s: %w", path, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		if err := os.Remove(path); err != nil {
			return false, fmt.Errorf("artifact cache: %s has digest %s, want %s, and could not be removed: %w",
				path, got, want, err)
		}
		return false, nil
	}
	return true, nil
}

// fetchArtifact downloads one artifact into the cache, digest-checked, through a
// temp file so an interrupted run cannot leave a half tarball behind under the
// name of a whole one.
func fetchArtifact(a upstreamArtifact, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("artifact cache: mkdir %s: %w", filepath.Dir(dst), err)
	}

	client := &http.Client{Timeout: artifactFetchTimeout}
	resp, err := client.Get(a.upstreamURL())
	if err != nil {
		return fmt.Errorf("artifact cache: fetch %s: %w", a.upstreamURL(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("artifact cache: fetch %s: status %s", a.upstreamURL(), resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".partial-*")
	if err != nil {
		return fmt.Errorf("artifact cache: temp file beside %s: %w", dst, err)
	}
	defer os.Remove(tmp.Name())

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("artifact cache: download %s: %w", a.upstreamURL(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("artifact cache: close %s: %w", tmp.Name(), err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.sha256 {
		return fmt.Errorf("artifact cache: %s has digest %s, the catalog pins %s.\n"+
			"  Either the release was re-cut upstream or the catalog is stale — do not\n"+
			"  relax this, fix tests/e2e-live/harness/artifactcatalog.go and its guard.",
			a.upstreamURL(), got, a.sha256)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("artifact cache: install %s: %w", dst, err)
	}
	return nil
}

// startArtifactMirror serves dir over HTTPS on an ephemeral port and advertises
// itself at advertiseHost, which is the address the SOUL CONTAINER must use — not
// the harness's own view of the socket.
//
// HTTPS, not HTTP, and this is not decoration. The three destinies declare
// `base_url` with `pattern: "^https://…"` and `core.url` refuses plain http
// without an explicit opt-out — a transport-security property of the artifacts
// under test. A mirror speaking http would have to be paid for by relaxing that
// pattern in examples/destiny/*, i.e. by weakening the subject to fit its own
// test (NIM-211). Serving TLS instead costs one generated CA and keeps the
// product's fetch path — scheme check, TLS handshake, chain validation —
// exercised exactly as it is against github.
//
// The port is ephemeral rather than fixed, and that costs nothing here: the
// keeper's service snapshot cache lives under the per-test `t.TempDir()`
// (stack.go: KEEPER_SERVICE_CACHE_DIR), so it is cold every test anyway and the
// per-run URL inside the vars layer changes a commit SHA nothing reuses. A fixed
// port would instead collide the moment two gates run at once, which the Makefile
// already says happens — this repo is worked in several worktrees.
func startArtifactMirror(dir, advertiseHost string) (*artifactMirror, error) {
	// A bracketed authority is one every destiny this fixture points at the mirror
	// rejects: they all declare `pattern: "^https://[A-Za-z0-9._/:-]+$"` for
	// base_url, a character class with no brackets in it. The rejection would
	// arrive inside the container, twenty minutes in, as an input-validation
	// failure worn as a product defect. Refuse it here, where the message can say
	// why.
	//
	// The test is on the authority this mirror would advertise, not on the address
	// family, because the family is only a proxy for it and a lossy one: an
	// IPv4-mapped literal (`::ffff:127.0.0.1`) has a To4() and a zone-ID literal
	// (`fe80::1%eth0`) does not parse as an IP at all, so a family test waves both
	// through — and JoinHostPort brackets both, since it brackets on a colon in the
	// host. Ask JoinHostPort what it will produce and judge that.
	if authority := net.JoinHostPort(advertiseHost, "<port>"); strings.HasPrefix(authority, "[") {
		return nil, fmt.Errorf("artifact mirror: advertise host %q has to be bracketed in a URL, and the "+
			"destiny input pattern for base_url accepts no brackets: the generated https://%s would be "+
			"rejected by the subject, not by us. Set E2E_KEEPER_HOST to an IPv4 address or a name reachable "+
			"from the container", advertiseHost, authority)
	}

	caPEM, cert, err := generateArtifactMirrorTLS(advertiseHost)
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, fmt.Errorf("artifact mirror: listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	m := &artifactMirror{
		baseURL: fmt.Sprintf("https://%s", net.JoinHostPort(advertiseHost, fmt.Sprint(port))),
		dir:     dir,
		caPEM:   caPEM,
		hits:    map[string]int{},
	}

	files := http.FileServer(http.Dir(dir))
	m.srv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Clean once, and judge the cleaned path in both places. `record`
			// cleans; the exclusion used to compare the raw one, so `/_e2e-live/./
			// mirror-alive` missed the exclusion and got counted under the cleaned
			// name. Nothing reads that name today, which is precisely why it would
			// have kept working until something did.
			reqPath := path.Clean("/" + r.URL.Path)
			if reqPath == artifactMirrorHealthPath {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, artifactMirrorHealthBody+"\n")
				return // deliberately not recorded, see artifactMirrorHealthPath
			}
			m.record(reqPath)
			files.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	go func() {
		// Keep the reason the server stopped. http.ErrServerClosed is close()
		// doing its job; anything else means the mirror died under the run, and
		// the only symptom the container would otherwise show is a connection
		// refused with no cause attached.
		if err := m.srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.mu.Lock()
			m.serveErr = err
			m.mu.Unlock()
		}
	}()
	return m, nil
}

// serveError — why the mirror's server stopped, or nil while it is serving.
func (m *artifactMirror) serveError() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.serveErr
}

// artifactMirrorCADir / artifactMirrorCAFile — where the mirror's root lands in
// the soul container. Debian's update-ca-certificates only picks up files under
// this directory, and only ones ending in `.crt`; a `.pem` here is ignored
// without a word.
const (
	artifactMirrorCADir  = "/usr/local/share/ca-certificates"
	artifactMirrorCAFile = "soul-stack-e2e-artifact-mirror.crt"
)

// artifactMirrorTLSValidity — how long the mirror's certificate is good for. A
// live test is minutes; a day of slack costs nothing and keeps a laptop that
// slept mid-suite from failing on clock skew.
const artifactMirrorTLSValidity = 24 * time.Hour

// generateArtifactMirrorTLS makes a throwaway CA and a leaf for advertiseHost.
//
// advertiseHost is whatever the container will put in the URL — an IP on WSL2
// (E2E_KEEPER_HOST), the name `host.docker.internal` on native Linux — so it goes
// into the SAN as an IP or a DNS name depending on what it parses as. Getting
// this wrong does not fail loudly at startup; it fails inside the container,
// twenty minutes in, as a certificate error against the product.
func generateArtifactMirrorTLS(advertiseHost string) (caPEM []byte, cert tls.Certificate, err error) {
	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := time.Now().Add(artifactMirrorTLSValidity)

	serial := func() (*big.Int, error) {
		return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("artifact mirror: CA key: %w", err)
	}
	caSerial, err := serial()
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("artifact mirror: CA serial: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "soul-stack-e2e-artifact-mirror-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("artifact mirror: create CA cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("artifact mirror: parse CA cert: %w", err)
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("artifact mirror: server key: %w", err)
	}
	srvSerial, err := serial()
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("artifact mirror: server serial: %w", err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: srvSerial,
		Subject:      pkix.Name{CommonName: "soul-stack-e2e-artifact-mirror"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// localhost/127.0.0.1 are for the harness's own tests of this file; the
		// container's address is appended below. No reference to container.go's
		// defaultKeeperHost — that file is behind //go:build e2e_live and this one
		// is deliberately not.
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	if ip := net.ParseIP(advertiseHost); ip != nil {
		srvTmpl.IPAddresses = append(srvTmpl.IPAddresses, ip)
	} else if advertiseHost != "" && advertiseHost != "localhost" {
		srvTmpl.DNSNames = append(srvTmpl.DNSNames, advertiseHost)
	}

	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("artifact mirror: create server cert: %w", err)
	}
	srvKeyDER, err := x509.MarshalPKCS8PrivateKey(srvKey)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("artifact mirror: marshal server key: %w", err)
	}

	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	cert, err = tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: srvKeyDER}),
	)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("artifact mirror: assemble keypair: %w", err)
	}
	return caPEM, cert, nil
}

// client — an http.Client whose root pool holds this mirror's CA and nothing
// else, not even the system roots. For the harness's own guards, and the empty
// starting pool is deliberate: a guard that also trusted the public roots would
// still pass against a mirror accidentally pointed at something real. Everything
// the SOUL container does goes through the system trust store instead
// (SpawnSoulContainer step 4b), and that path is the one the live tests exercise.
func (m *artifactMirror) client(timeout time.Duration) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(m.caPEM)
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}

// record counts one request against its cleaned path.
func (m *artifactMirror) record(urlPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hits[strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+urlPath)), "/")]++
}

// hitCount — how many times the mirror was asked for that path (as returned by
// upstreamArtifact.cacheRel).
func (m *artifactMirror) hitCount(rel string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[rel]
}

// served — every path the mirror was asked for, sorted. For the failure message:
// "nothing was served" and "something else was served" want different reactions,
// and a bare count cannot tell them apart.
func (m *artifactMirror) served() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.hits))
	for k := range m.hits {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// close stops the server. Idempotent enough for a t.Cleanup chain.
func (m *artifactMirror) close() {
	if m == nil || m.srv == nil {
		return
	}
	_ = m.srv.Close()
}

// missingArtifacts — the catalog entries the mirror never served, as a message,
// or "" when it served them all. Split out from the assertion so it can be tested
// without a *testing.T that must fail.
func missingArtifacts(m *artifactMirror, cat []upstreamArtifact) string {
	var missing []string
	for _, a := range cat {
		if m.hitCount(a.cacheRel()) == 0 {
			missing = append(missing, a.cacheRel())
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"the artifact mirror was never asked for %s.\n"+
			"  It served: %v\n"+
			"  The run fetched those tarballs from SOMEWHERE — if not from here, then from\n"+
			"  github.com, and this gate is back to being decided by a network outside its\n"+
			"  slice (NIM-542). The vars layer is what redirects them: check that\n"+
			"  %s still sorts last in the service's vars/ directory, that the example has\n"+
			"  not grown a vars/_stack.yaml (an unlisted layer contributes NOTHING under a\n"+
			"  stack — keeper/internal/servicevars/pipeline.go), and that the scenario still\n"+
			"  reads these var names.",
		strings.Join(missing, ", "), m.served(), artifactMirrorVarsFile)
}
