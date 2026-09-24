package gitauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestMain clears go-git's ssh_config reader for the whole package.
//
// [hostPort] reads it, and its default is the RUNNING USER's `~/.ssh/config`:
// a developer with a `Host gitlab.example.com` stanza would otherwise see
// TestHostPort and TestLookup_ClientConfigPinsTheHostKeyAlgorithm fail on
// their machine and nowhere else. The one test that is ABOUT that reader swaps
// its own in.
func TestMain(m *testing.M) {
	gitssh.DefaultSSHConfig = nil
	os.Exit(m.Run())
}

// fakeKV is a KVReader over a literal map: path → fields.
type fakeKV struct {
	secrets map[string]map[string]any
	err     error
}

func (f *fakeKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	kv, ok := f.secrets[path]
	if !ok {
		return nil, errors.New("fakeKV: no secret at " + path)
	}
	return kv, nil
}

// keyPair is a generated ed25519 identity plus the two serialized forms the
// credential block deals in.
type keyPair struct {
	privPEM    string
	pub        ssh.PublicKey
	knownHosts string
}

func newKeyPair(t *testing.T, host string) keyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh public key: %v", err)
	}
	return keyPair{
		privPEM:    string(pem.EncodeToMemory(block)),
		pub:        sshPub,
		knownHosts: knownhosts.Line([]string{host}, sshPub) + "\n",
	}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

const (
	testHost    = "gitlab.example.com"
	testSSHURL  = "ssh://git@gitlab.example.com/org/repo.git"
	testSCPURL  = "git@gitlab.example.com:org/repo.git"
	testHTTPURL = "https://gitlab.example.com/org/repo.git"
)

func mustLoad(t *testing.T, vc KVReader, creds ...config.KeeperGitCredential) *Store {
	t.Helper()
	s, err := Load(context.Background(), vc, &config.KeeperGit{Credentials: creds})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func TestLoad_AbsentBlockYieldsNilStore(t *testing.T) {
	for name, block := range map[string]*config.KeeperGit{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Load(context.Background(), nil, block)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if s != nil {
				t.Fatalf("Store = %+v, want nil", s)
			}
		})
	}
}

// TestLookup_SSHKeyFromVault and TestLookup_SSHKeyFromFile are the two halves
// of the acceptance criterion "cloned with a key from Vault and with a key from
// a file": the same entry, the same result, two sources.
func TestLookup_SSHKeyFromVault(t *testing.T) {
	kp := newKeyPair(t, testHost)
	vc := &fakeKV{secrets: map[string]map[string]any{
		"secret/keeper/git": {"ssh_key": kp.privPEM, "known_hosts": kp.knownHosts},
	}}
	s := mustLoad(t, vc, config.KeeperGitCredential{
		Host:          testHost,
		KeyRef:        "vault:secret/keeper/git#ssh_key",
		KnownHostsRef: "vault:secret/keeper/git#known_hosts",
	})

	auth, matched := s.Lookup(testSSHURL)
	if !matched {
		t.Fatal("Lookup did not match the configured host")
	}
	pk, ok := auth.(*gitssh.PublicKeys)
	if !ok {
		t.Fatalf("auth is %T, want *gitssh.PublicKeys", auth)
	}
	if pk.User != "git" {
		t.Errorf("User = %q, want git", pk.User)
	}
	if !bytes.Equal(pk.Signer.PublicKey().Marshal(), kp.pub.Marshal()) {
		t.Error("the signer does not carry the configured key")
	}
	if pk.HostKeyCallback == nil {
		t.Error("HostKeyCallback is nil — go-git would fall back to the keeper's own ~/.ssh")
	}
}

func TestLookup_SSHKeyFromFile(t *testing.T) {
	kp := newKeyPair(t, testHost)
	s := mustLoad(t, nil, config.KeeperGitCredential{
		Host:           testHost,
		KeyFile:        writeFile(t, "id_ed25519", kp.privPEM),
		KnownHostsFile: writeFile(t, "known_hosts", kp.knownHosts),
	})

	auth, matched := s.Lookup(testSSHURL)
	if !matched {
		t.Fatal("Lookup did not match the configured host")
	}
	pk := auth.(*gitssh.PublicKeys)
	if !bytes.Equal(pk.Signer.PublicKey().Marshal(), kp.pub.Marshal()) {
		t.Error("the signer does not carry the key read from key_file")
	}
	if pk.HostKeyCallback == nil {
		t.Error("HostKeyCallback is nil")
	}
}

// TestLoad_RefWinsOverFile is the acceptance criterion for the priority: two
// DIFFERENT credentials are configured in the two sources, and the one that
// must come out is the Vault one. A test with the same material in both could
// not tell.
//
// It covers known_hosts as well as the key, because those resolve through
// separate code: the key is read once and the hosts are parsed by go-git from
// a PATH, so the file source is reachable by a second route and the priority
// can hold for the key while silently inverting for the hosts it is checked
// against.
func TestLoad_RefWinsOverFile(t *testing.T) {
	fromVault := newKeyPair(t, testHost)
	fromFile := newKeyPair(t, testHost)
	vc := &fakeKV{secrets: map[string]map[string]any{
		"secret/keeper/git": {"ssh_key": fromVault.privPEM, "known_hosts": fromVault.knownHosts},
	}}
	s := mustLoad(t, vc, config.KeeperGitCredential{
		Host:           testHost,
		KeyRef:         "vault:secret/keeper/git#ssh_key",
		KeyFile:        writeFile(t, "id_ed25519", fromFile.privPEM),
		KnownHostsRef:  "vault:secret/keeper/git#known_hosts",
		KnownHostsFile: writeFile(t, "known_hosts", fromFile.knownHosts),
	})

	auth, _ := s.Lookup(testSSHURL)
	pk := auth.(*gitssh.PublicKeys)
	if !bytes.Equal(pk.Signer.PublicKey().Marshal(), fromVault.pub.Marshal()) {
		t.Error("key_file won over key_ref")
	}
	if bytes.Equal(pk.Signer.PublicKey().Marshal(), fromFile.pub.Marshal()) {
		t.Error("the file key was used — the two sources are not distinguishable in this test")
	}

	addr := &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 22}
	if err := pk.HostKeyCallback(testHost+":22", addr, fromVault.pub); err != nil {
		t.Errorf("the host key pinned by known_hosts_ref was rejected: %v", err)
	}
	if err := pk.HostKeyCallback(testHost+":22", addr, fromFile.pub); err == nil {
		t.Error("known_hosts_file won over known_hosts_ref")
	}
}

// TestLookup_HostKeyCallbackPinsTheConfiguredKey — the known_hosts source has
// to end up ENFORCING something. A callback built from an entry that pins one
// key must accept that key on that host and refuse any other; a callback that
// accepted everything would pass every other test here unnoticed.
func TestLookup_HostKeyCallbackPinsTheConfiguredKey(t *testing.T) {
	pinned := newKeyPair(t, testHost)
	other := newKeyPair(t, testHost)
	vc := &fakeKV{secrets: map[string]map[string]any{
		"secret/keeper/git": {"ssh_key": pinned.privPEM, "known_hosts": pinned.knownHosts},
	}}
	s := mustLoad(t, vc, config.KeeperGitCredential{
		Host:          testHost,
		KeyRef:        "vault:secret/keeper/git#ssh_key",
		KnownHostsRef: "vault:secret/keeper/git#known_hosts",
	})
	auth, _ := s.Lookup(testSSHURL)
	cb := auth.(*gitssh.PublicKeys).HostKeyCallback

	addr := &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 22}
	if err := cb(testHost+":22", addr, pinned.pub); err != nil {
		t.Errorf("the pinned host key was rejected: %v", err)
	}
	if err := cb(testHost+":22", addr, other.pub); err == nil {
		t.Error("an unpinned host key was accepted — host verification is not happening")
	}
	if err := cb("elsewhere.example.com:22", addr, pinned.pub); err == nil {
		t.Error("the key was accepted for a host it is not pinned to")
	}
}

// TestLookup_ClientConfigPinsTheHostKeyAlgorithm goes one level past the
// callback, to the *ssh.ClientConfig go-git actually dials with — the level at
// which the previous revision of this package was broken and every
// callback-level test stayed green.
//
// go-git derives HostKeyAlgorithms from known_hosts ONLY when it installs its
// own callback; with a custom one it leaves the field to the caller
// (plumbing/transport/ssh/common.go). Left empty, x/crypto advertises its
// default order, in which ed25519 is LAST — so a host pinned by its ed25519
// key alone negotiates the server's ECDSA key instead and the pinned callback
// reports `knownhosts: key mismatch`, which reads like a man-in-the-middle.
func TestLookup_ClientConfigPinsTheHostKeyAlgorithm(t *testing.T) {
	kp := newKeyPair(t, testHost)
	s := mustLoad(t, nil, config.KeeperGitCredential{
		Host:           testHost,
		KeyFile:        writeFile(t, "id_ed25519", kp.privPEM),
		KnownHostsFile: writeFile(t, "known_hosts", kp.knownHosts),
	})
	auth, matched := s.Lookup(testSSHURL)
	if !matched {
		t.Fatal("Lookup did not match")
	}
	cfg, err := auth.(*gitssh.PublicKeys).ClientConfig()
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	if len(cfg.HostKeyAlgorithms) == 0 {
		t.Fatal("HostKeyAlgorithms is empty — x/crypto would advertise its defaults, " +
			"in which ed25519 ranks last, and a host pinned only by its ed25519 key " +
			"would fail the handshake as a key mismatch")
	}
	if cfg.HostKeyAlgorithms[0] != ssh.KeyAlgoED25519 {
		t.Errorf("HostKeyAlgorithms = %v, want the pinned %s first",
			cfg.HostKeyAlgorithms, ssh.KeyAlgoED25519)
	}
	if cfg.HostKeyCallback == nil {
		t.Error("HostKeyCallback is nil in the dialed config")
	}
}

// TestLookup_HTTPSToken covers the acceptance criterion for https from both
// sources, and the reason the https half exists at all: the token travels in
// the auth method, never in the URL the registry and the audit payload keep.
func TestLookup_HTTPSToken(t *testing.T) {
	const token = "glpat-xxxxxxxxxxxxxxxxxxxx"
	cases := map[string]config.KeeperGitCredential{
		"from vault": {Host: testHost, TokenRef: "vault:secret/keeper/git#token"},
		"from file":  {Host: testHost, TokenFile: writeFile(t, "token", token+"\n")},
	}
	vc := &fakeKV{secrets: map[string]map[string]any{
		"secret/keeper/git": {"token": token},
	}}
	for name, cred := range cases {
		t.Run(name, func(t *testing.T) {
			s := mustLoad(t, vc, cred)
			auth, matched := s.Lookup(testHTTPURL)
			if !matched {
				t.Fatal("Lookup did not match the configured host")
			}
			ba, ok := auth.(*githttp.BasicAuth)
			if !ok {
				t.Fatalf("auth is %T, want *githttp.BasicAuth", auth)
			}
			if ba.Password != token {
				t.Errorf("Password = %q, want the configured token", ba.Password)
			}
			if ba.Username != tokenUser {
				t.Errorf("Username = %q, want %q", ba.Username, tokenUser)
			}
			// The token must not be recoverable from the auth method's own
			// rendering: go-git logs it on some paths.
			if strings.Contains(ba.String(), token) {
				t.Errorf("the token leaked into String(): %q", ba.String())
			}
		})
	}
}

// TestHTTPSTokenTravelsAsAHeaderNotInTheURL is the guard behind acceptance
// criterion 2, taken to the wire: go-git is pointed at a local server with the
// auth method Lookup produced, and the assertions are on what the SERVER
// received.
//
// The point is where the credential ends up. `services.git` is stored in the
// registry, returned by `GET /v1/services` and copied verbatim into the
// `service.register` audit payload, so the token must reach the remote in the
// Authorization header and appear nowhere in the request line. Asserting on
// the local URL variable instead would prove nothing — it is a copy, and no
// implementation of Lookup could change it; the two assertions here are the
// two the server can actually contradict.
func TestHTTPSTokenTravelsAsAHeaderNotInTheURL(t *testing.T) {
	const token = "glpat-xxxxxxxxxxxxxxxxxxxx"

	var (
		gotAuthorization string
		gotRequestURI    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotRequestURI = r.RequestURI
		// 401 ends the exchange without implementing the git protocol: the
		// subject is the request the client sent, not the response.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", srv.URL, err)
	}
	s := mustLoad(t, nil, config.KeeperGitCredential{
		Host:      host,
		TokenFile: writeFile(t, "token", token),
	})

	gitURL := srv.URL + "/org/repo.git"
	auth, matched := s.Lookup(gitURL)
	if !matched {
		t.Fatalf("Lookup did not match %q", gitURL)
	}
	remote := git.NewRemote(nil, &gitconfig.RemoteConfig{Name: "origin", URLs: []string{gitURL}})
	// The listing is expected to fail on the 401; what it carried is the test.
	_, _ = remote.ListContext(context.Background(), &git.ListOptions{Auth: auth})

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(tokenUser+":"+token))
	if gotAuthorization != want {
		t.Errorf("Authorization = %q, want the token as basic-auth password", gotAuthorization)
	}
	if strings.Contains(gotRequestURI, token) {
		t.Errorf("the token reached the request line: %q", gotRequestURI)
	}
}

// TestLookup_URLUsernameWinsOverTheDefault — an operator may record a
// non-secret account name in services.git; the token stays in the config.
func TestLookup_URLUsernameWinsOverTheDefault(t *testing.T) {
	s := mustLoad(t, nil, config.KeeperGitCredential{
		Host:      testHost,
		TokenFile: writeFile(t, "token", "t"),
	})
	auth, matched := s.Lookup("https://ci-bot@gitlab.example.com/org/repo.git")
	if !matched {
		t.Fatal("Lookup did not match")
	}
	if got := auth.(*githttp.BasicAuth).Username; got != "ci-bot" {
		t.Errorf("Username = %q, want ci-bot", got)
	}
}

// TestLookup_NoMatchLeavesTheCallersFallback is THE guard on the feature
// staying optional. Every case here must report matched=false, which is what
// makes the caller do exactly what it did before NIM-898 — the ssh-agent for
// ssh, nil for https. Without it the block would silently become mandatory for
// sources it does not name.
func TestLookup_NoMatchLeavesTheCallersFallback(t *testing.T) {
	kp := newKeyPair(t, testHost)
	keyOnly := config.KeeperGitCredential{
		Host:           testHost,
		KeyFile:        writeFile(t, "id_ed25519", kp.privPEM),
		KnownHostsFile: writeFile(t, "known_hosts", kp.knownHosts),
	}
	tokenOnly := config.KeeperGitCredential{
		Host:      testHost,
		TokenFile: writeFile(t, "token", "t"),
	}

	cases := []struct {
		name  string
		store *Store
		url   string
	}{
		{"nil store", nil, testSSHURL},
		{"nil store, https", nil, testHTTPURL},
		{"unknown host, ssh", mustLoad(t, nil, keyOnly), "ssh://git@other.example.com/o/r.git"},
		{"unknown host, https", mustLoad(t, nil, tokenOnly), "https://other.example.com/o/r.git"},
		{"ssh url on a token-only host", mustLoad(t, nil, tokenOnly), testSSHURL},
		{"https url on a key-only host", mustLoad(t, nil, keyOnly), testHTTPURL},
		{"url with no host at all", mustLoad(t, nil, keyOnly), "file:///srv/repos/r.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth, matched := c.store.Lookup(c.url)
			if matched {
				t.Fatalf("Lookup claimed a credential for %q: %v", c.url, auth)
			}
			if auth != nil {
				t.Fatalf("auth = %v with matched=false", auth)
			}
		})
	}
}

// TestLookup_SCPFormIsSSH — `git@host:org/repo.git` carries no scheme and is
// the form most operators write. It must take the ssh half of the entry, not
// the https one.
func TestLookup_SCPFormIsSSH(t *testing.T) {
	kp := newKeyPair(t, testHost)
	s := mustLoad(t, nil, config.KeeperGitCredential{
		Host:           testHost,
		KeyFile:        writeFile(t, "id_ed25519", kp.privPEM),
		KnownHostsFile: writeFile(t, "known_hosts", kp.knownHosts),
		TokenFile:      writeFile(t, "token", "t"),
	})
	auth, matched := s.Lookup(testSCPURL)
	if !matched {
		t.Fatal("Lookup did not match the scp form")
	}
	if _, ok := auth.(*gitssh.PublicKeys); !ok {
		t.Fatalf("auth is %T, want *gitssh.PublicKeys", auth)
	}
}

// TestLookup_HostKeyNormalization — the configured host and the URL's host go
// through two different code paths to reach the same map key, so the forms an
// operator may legitimately write have to be pinned. `[fd00::1]` is the one
// that bites: `url.Hostname()` strips the brackets and a key that kept them
// would match nothing.
func TestLookup_HostKeyNormalization(t *testing.T) {
	cases := map[string]string{
		"GitLab.Example.COM": "https://gitlab.example.com/org/repo.git",
		"gitlab.example.com": "https://GitLab.Example.COM/org/repo.git",
		"[fd00::1]":          "https://[fd00::1]/org/repo.git",
		"fd00::1":            "https://[FD00::1]/org/repo.git",
		// One address, two spellings. Comparing the strings would leave this
		// entry validating and never matching.
		"fd00:0:0:0:0:0:0:1": "https://[fd00::1]/org/repo.git",
		"10.1.2.3":           "https://10.1.2.3/org/repo.git",
	}
	for configured, gitURL := range cases {
		t.Run(configured, func(t *testing.T) {
			s := mustLoad(t, nil, config.KeeperGitCredential{
				Host:      configured,
				TokenFile: writeFile(t, "token", "t"),
			})
			if _, matched := s.Lookup(gitURL); !matched {
				t.Fatalf("host %q did not match %q", configured, gitURL)
			}
		})
	}
}

func TestLoad_VaultFieldOverride(t *testing.T) {
	kp := newKeyPair(t, testHost)
	vc := &fakeKV{secrets: map[string]map[string]any{
		"secret/keeper/git": {"deploy_key": kp.privPEM, "hosts": kp.knownHosts},
	}}
	s := mustLoad(t, vc, config.KeeperGitCredential{
		Host:          testHost,
		KeyRef:        "vault:secret/keeper/git#deploy_key",
		KnownHostsRef: "vault:secret/keeper/git#hosts",
	})
	auth, matched := s.Lookup(testSSHURL)
	if !matched {
		t.Fatal("Lookup did not match")
	}
	if !bytes.Equal(auth.(*gitssh.PublicKeys).Signer.PublicKey().Marshal(), kp.pub.Marshal()) {
		t.Error("the #field override did not select the right Vault field")
	}
}

func TestLoad_DefaultVaultFields(t *testing.T) {
	const token = "glpat-default-field"
	vc := &fakeKV{secrets: map[string]map[string]any{
		"secret/keeper/git": {defaultTokenField: token},
	}}
	s := mustLoad(t, vc, config.KeeperGitCredential{
		Host:     testHost,
		TokenRef: "vault:secret/keeper/git",
	})
	auth, matched := s.Lookup(testHTTPURL)
	if !matched {
		t.Fatal("Lookup did not match")
	}
	if auth.(*githttp.BasicAuth).Password != token {
		t.Error("a ref without a #field did not fall back to the default field")
	}
}

func TestLoad_Failures(t *testing.T) {
	kp := newKeyPair(t, testHost)
	goodVC := &fakeKV{secrets: map[string]map[string]any{
		"secret/keeper/git": {"ssh_key": kp.privPEM, "known_hosts": kp.knownHosts, "empty": ""},
	}}

	cases := []struct {
		name string
		vc   KVReader
		cred config.KeeperGitCredential
		want string
	}{
		{
			name: "ssh key with no known_hosts source",
			vc:   goodVC,
			cred: config.KeeperGitCredential{Host: testHost, KeyRef: "vault:secret/keeper/git#ssh_key"},
			want: "known_hosts",
		},
		{
			name: "a ref with no vault client",
			vc:   nil,
			cred: config.KeeperGitCredential{Host: testHost, TokenRef: "vault:secret/keeper/git#token"},
			want: "vault client",
		},
		{
			name: "an empty vault field",
			vc:   goodVC,
			cred: config.KeeperGitCredential{Host: testHost, TokenRef: "vault:secret/keeper/git#empty"},
			want: "empty",
		},
		{
			name: "a missing vault field",
			vc:   goodVC,
			cred: config.KeeperGitCredential{Host: testHost, TokenRef: "vault:secret/keeper/git#absent"},
			want: "has no",
		},
		{
			name: "an unreadable key file",
			vc:   nil,
			cred: config.KeeperGitCredential{Host: testHost, KeyFile: "/nonexistent/id_ed25519", KnownHostsFile: "/nonexistent/known_hosts"},
			want: "key_file",
		},
		{
			name: "a whitespace-only token",
			vc:   nil,
			cred: config.KeeperGitCredential{Host: testHost, TokenFile: writeFile(t, "token", "\n\n")},
			want: "whitespace",
		},
		{
			name: "a malformed known_hosts",
			vc:   nil,
			cred: config.KeeperGitCredential{Host: testHost, KeyFile: writeFile(t, "id_ed25519", kp.privPEM), KnownHostsFile: writeFile(t, "known_hosts", "not a known_hosts line\n")},
			want: "known_hosts",
		},
		{
			// The reader skips blank and `#` lines without complaint and hands
			// back a db that refuses every host, so this has to be caught here
			// or it surfaces as a key error on the first clone instead.
			name: "a known_hosts holding only comments",
			vc:   nil,
			cred: config.KeeperGitCredential{Host: testHost, KeyFile: writeFile(t, "id_ed25519", kp.privPEM), KnownHostsFile: writeFile(t, "known_hosts", "# managed by ansible\n\n")},
			want: "no known_hosts entry",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(context.Background(), c.vc, &config.KeeperGit{Credentials: []config.KeeperGitCredential{c.cred}})
			if err == nil {
				t.Fatal("Load succeeded, want a startup failure")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
			if !strings.Contains(err.Error(), testHost) {
				t.Errorf("error %q does not name the host it came from", err)
			}
		})
	}
}

// TestLoad_MalformedKeyDoesNotQuoteTheKey — a startup error is printed and
// logged. It must name the field, not the material.
func TestLoad_MalformedKeyDoesNotQuoteTheKey(t *testing.T) {
	const bogus = "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-actually-a-key\n-----END OPENSSH PRIVATE KEY-----\n"
	kp := newKeyPair(t, testHost)
	_, err := Load(context.Background(), nil, &config.KeeperGit{Credentials: []config.KeeperGitCredential{{
		Host:           testHost,
		KeyFile:        writeFile(t, "id_ed25519", bogus),
		KnownHostsFile: writeFile(t, "known_hosts", kp.knownHosts),
	}}})
	if err == nil {
		t.Fatal("Load accepted a malformed private key")
	}
	if strings.Contains(err.Error(), "not-actually-a-key") {
		t.Fatalf("the key material leaked into the error: %v", err)
	}
}

// TestLoad_KnownHostsShapesTheReaderAccepts pins the boundary of the
// zero-entry check: it must refuse ONLY a file with nothing to read, and
// accept every shape the reader that consumes the file accepts.
//
// The trailing-comment case is the one that bit. `ssh.ParseKnownHosts` caps an
// entry at five whitespace fields; the reader stops at the key blob and ignores
// the rest of the line. `<host> ssh-ed25519 AAAA... deploy key for gitlab` is
// six fields — an ordinary line `ssh` is fine with — and checking it with the
// stricter parser refused to start the daemon over a comment.
func TestLoad_KnownHostsShapesTheReaderAccepts(t *testing.T) {
	kp := newKeyPair(t, testHost)
	line := strings.TrimRight(kp.knownHosts, "\n")

	cases := map[string]string{
		"bare":                       line + "\n",
		"trailing one-word note":     line + " deploy\n",
		"trailing multi-word note":   line + " deploy key for gitlab\n",
		"leading comment and blanks": "# managed by ansible\n\n" + line + "\n",
		"no trailing newline":        line,
		"CRLF line endings":          strings.ReplaceAll(line+"\n", "\n", "\r\n"),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			s := mustLoad(t, nil, config.KeeperGitCredential{
				Host:           testHost,
				KeyFile:        writeFile(t, "id_ed25519", kp.privPEM),
				KnownHostsFile: writeFile(t, "known_hosts", content),
			})
			auth, matched := s.Lookup(testSSHURL)
			if !matched {
				t.Fatal("Lookup did not match")
			}
			addr := &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 22}
			if err := auth.(*gitssh.PublicKeys).HostKeyCallback(testHost+":22", addr, kp.pub); err != nil {
				t.Fatalf("the pinned host key was rejected: %v", err)
			}
		})
	}
}

func TestLoad_TokenFileTrailingNewlineStripped(t *testing.T) {
	const token = "glpat-xxxxxxxxxxxxxxxxxxxx"
	s := mustLoad(t, nil, config.KeeperGitCredential{
		Host:      testHost,
		TokenFile: writeFile(t, "token", token+"\r\n"),
	})
	auth, _ := s.Lookup(testHTTPURL)
	if got := auth.(*githttp.BasicAuth).Password; got != token {
		t.Errorf("Password = %q, want the token without its trailing newline", got)
	}
}
