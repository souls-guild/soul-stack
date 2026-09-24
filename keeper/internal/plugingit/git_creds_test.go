package plugingit

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
	credSSHURL  = "ssh://git@gitlab.example.com/org/plugin.git"
	credHTTPURL = "https://gitlab.example.com/org/plugin.git"
)

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

// TestAuthFor_ConfiguredCredentialWins — a `kind: git` plugin source on a
// configured host takes the configured credential, for both schemes, without
// consulting the agent (SSH_AUTH_SOCK is blanked).
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

// TestAuthFor_NoMatchingCredentialKeepsAgentFallback — the same guard the
// artifact package carries, and the reason it is carried twice: the two
// authFor implementations are symmetric by hand, so a change to one that
// forgets the other has to fail somewhere.
func TestAuthFor_NoMatchingCredentialKeepsAgentFallback(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	store := gitCredStore(t)

	auth, err := authFor("https://other.example.com/org/plugin.git", store)
	if err != nil {
		t.Fatalf("authFor(https): %v", err)
	}
	if auth != nil {
		t.Fatalf("https on an unconfigured host resolved to %v, want no auth", auth)
	}

	if _, err := authFor("ssh://git@other.example.com/org/plugin.git", store); err == nil {
		t.Fatal("ssh resolved without an agent — the agent fallback is gone")
	} else if !strings.Contains(err.Error(), "SSH-agent auth") {
		t.Fatalf("ssh failed somewhere other than the agent: %v", err)
	}

	if _, err := authFor(credHTTPURL, nil); err != nil {
		t.Fatalf("authFor with no store at all: %v", err)
	}
}
