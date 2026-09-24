package artifact

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/souls-guild/soul-stack/keeper/internal/gitauth"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	credHost    = "gitlab.example.com"
	credSSHURL  = "ssh://git@gitlab.example.com/org/repo.git"
	credHTTPURL = "https://gitlab.example.com/org/repo.git"
)

// gitCredStore builds a store holding one entry for credHost: an ssh key with
// its known_hosts, and an https token.
func gitCredStore(t *testing.T) *gitauth.Store {
	t.Helper()
	dir := t.TempDir()
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
	keyFile := filepath.Join(dir, "id_ed25519")
	khFile := filepath.Join(dir, "known_hosts")
	tokenFile := filepath.Join(dir, "token")
	for path, content := range map[string]string{
		keyFile:   string(pem.EncodeToMemory(block)),
		khFile:    knownhosts.Line([]string{credHost}, sshPub) + "\n",
		tokenFile: "glpat-xxxxxxxxxxxxxxxxxxxx",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	store, err := gitauth.Load(context.Background(), nil, &config.KeeperGit{
		Credentials: []config.KeeperGitCredential{{
			Host:           credHost,
			KeyFile:        keyFile,
			KnownHostsFile: khFile,
			TokenFile:      tokenFile,
		}},
	})
	if err != nil {
		t.Fatalf("gitauth.Load: %v", err)
	}
	return store
}

// TestAuthFor_ConfiguredCredentialWins — a matching entry supplies the auth
// method for both schemes, and for ssh it does so WITHOUT consulting the agent:
// SSH_AUTH_SOCK is blanked, so a fall-through would surface as an error.
func TestAuthFor_ConfiguredCredentialWins(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	store := gitCredStore(t)

	sshAuth, err := authFor(credSSHURL, store)
	if err != nil {
		t.Fatalf("authFor(ssh): %v", err)
	}
	if _, ok := sshAuth.(*gitssh.PublicKeys); !ok {
		t.Errorf("ssh auth is %T, want *gitssh.PublicKeys", sshAuth)
	}

	httpAuth, err := authFor(credHTTPURL, store)
	if err != nil {
		t.Fatalf("authFor(https): %v", err)
	}
	if _, ok := httpAuth.(*githttp.BasicAuth); !ok {
		t.Errorf("https auth is %T, want *githttp.BasicAuth", httpAuth)
	}
}

// TestAuthFor_NoMatchingCredentialKeepsAgentFallback is the guard that keeps
// NIM-898 additive. With no store at all, and with a store that names a
// different host, resolution must be what it was before the feature existed:
//
//   - https → no auth at all (nil, nil), the pre-existing MVP behaviour;
//   - ssh → the ssh-agent, which with SSH_AUTH_SOCK blanked fails AS THE AGENT
//     and so proves the agent is still the one being asked.
//
// Delete the fallback in authFor and this test reddens; delete the credential
// branch and it stays green while TestAuthFor_ConfiguredCredentialWins reddens.
func TestAuthFor_NoMatchingCredentialKeepsAgentFallback(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	elsewhere := gitCredStore(t)

	for name, store := range map[string]*gitauth.Store{
		"no store":       nil,
		"unrelated host": elsewhere,
	} {
		t.Run(name, func(t *testing.T) {
			httpsURL := "https://other.example.com/org/repo.git"
			if name == "no store" {
				httpsURL = credHTTPURL
			}
			auth, err := authFor(httpsURL, store)
			if err != nil {
				t.Fatalf("authFor(https): %v", err)
			}
			if auth != nil {
				t.Fatalf("https resolved to %v, want no auth", auth)
			}

			sshURL := "ssh://git@other.example.com/org/repo.git"
			if name == "no store" {
				sshURL = credSSHURL
			}
			if _, err := authFor(sshURL, store); err == nil {
				t.Fatal("ssh resolved without an agent — the agent fallback is gone")
			} else if !strings.Contains(err.Error(), "SSH-agent auth") {
				t.Fatalf("ssh failed somewhere other than the agent: %v", err)
			}
		})
	}
}

// TestAuthFor_FileURLNeedsNoAuth — `file://` is the dev/test source the test
// corpus clones from; it must keep resolving to no auth whatever is configured.
func TestAuthFor_FileURLNeedsNoAuth(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	auth, err := authFor("file:///srv/repos/repo.git", gitCredStore(t))
	if err != nil {
		t.Fatalf("authFor(file): %v", err)
	}
	if auth != nil {
		t.Fatalf("file:// resolved to %v, want no auth", auth)
	}
}
