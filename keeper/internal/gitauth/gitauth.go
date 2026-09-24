// Package gitauth resolves the keeper.yml `git.credentials[]` block into the
// go-git auth methods the artifact loader and the plugin resolver hand to
// clone / fetch / ls-remote (NIM-898).
//
// Before it, `authFor` knew exactly one method — the ssh-agent — so a private
// https remote could be reached only by writing the token into the URL, and
// that URL is `services.git`: kept in the registry, returned by
// `GET /v1/services`, rendered in the UI, and copied verbatim into the
// `service.register` audit payload, which is append-only.
//
// The block is additive. A source whose host matches no entry resolves exactly
// as it did before this package existed, and so does an entry that carries no
// credential for the URL's scheme — [Store.Lookup] reports `matched=false` and
// the caller keeps its own fallback. Nothing here can make the feature
// mandatory by accident.
//
// Everything is resolved ONCE, at daemon start, symmetric with
// `push.LoadHostCAs`: a bad key, an unreadable file or a malformed known_hosts
// aborts startup rather than surfacing on the first clone of some service
// hours later. A SIGHUP does not re-read the block.
package gitauth

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Default Vault KV field names for the `*_ref` sources. Each is overridable
// per-ref with the `#<field>` suffix (`vault:secret/keeper/git#deploy_key`),
// the same override `redis.password_ref` accepts — one Vault secret usually
// holds all three fields of one host.
const (
	defaultKeyField        = "ssh_key"
	defaultKnownHostsField = "known_hosts"
	defaultTokenField      = "token"
)

// tokenUser is the basic-auth username used for an https token when the URL
// names none. Both GitLab and GitHub authenticate a personal access token sent
// as the PASSWORD and ignore the username, but an empty username is not
// universally accepted, so a value has to be chosen; `oauth2` is GitLab's
// documented spelling. A URL that does carry a username wins — that lets an
// operator record a non-secret account name in `services.git` while the token
// stays here.
const tokenUser = "oauth2"

// KVReader is the narrow Vault KV read this package needs, satisfied by
// *keepervault.Client. Factored out for tests that stand up no Vault.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// ErrVaultClientRequired means a `*_ref` source was configured with no Vault
// client to resolve it through.
var ErrVaultClientRequired = errors.New("gitauth: vault client is required to resolve a *_ref source")

// credential is one host's resolved material. The ssh key is parsed at load
// time into a Signer, and the known_hosts db is built at load time, so a
// malformed key or an unparseable known_hosts fails the START rather than the
// first clone. What that does NOT establish is that the db is usable — see
// [knownHostsDB].
//
// The db is kept rather than just its callback because the host-key ALGORITHM
// list is derived from it per target address — see [Store.Lookup].
type credential struct {
	signer     ssh.Signer
	knownHosts hostKeyDB
	token      string
}

// hostKeyDB is the part of go-git's known_hosts database this package uses.
// Named as an interface so the concrete type — which reaches us through
// `gitssh.NewKnownHostsDb` and belongs to an INDIRECT dependency
// (`github.com/skeema/knownhosts`) — is not written down here: spelling it
// would promote that module to a direct require in keeper/go.mod, which is a
// dependency-graph change this has no reason to make. Two methods is also the
// whole of what is used.
type hostKeyDB interface {
	HostKeyCallback() ssh.HostKeyCallback
	HostKeyAlgorithms(hostWithPort string) []string
}

// Store maps a host to its credentials. Read-only after [Load]; a nil *Store
// is valid and matches nothing, which is how every caller with no `git:` block
// keeps its previous behaviour without a nil check of its own.
type Store struct {
	byHost map[string]credential
}

// Load resolves every entry of the `git:` block. A nil or empty block yields a
// nil Store and no error.
//
// Failure is fail-fast with the host named: the caller (daemon startup) aborts,
// because a keeper that came up with half its git credentials would fail later
// at a clone, with an error pointing at the repository rather than at the
// config line that is actually wrong.
func Load(ctx context.Context, vc KVReader, g *config.KeeperGit) (*Store, error) {
	if g == nil || len(g.Credentials) == 0 {
		return nil, nil
	}
	byHost := make(map[string]credential, len(g.Credentials))
	for _, c := range g.Credentials {
		resolved, err := loadCredential(ctx, vc, c)
		if err != nil {
			return nil, fmt.Errorf("gitauth: git.credentials[%s]: %w", c.Host, err)
		}
		byHost[CanonicalHost(c.Host)] = resolved
	}
	return &Store{byHost: byHost}, nil
}

func loadCredential(ctx context.Context, vc KVReader, c config.KeeperGitCredential) (credential, error) {
	var out credential

	key, err := resolveSecret(ctx, vc, c.KeyRef, c.KeyFile, defaultKeyField, "key")
	if err != nil {
		return credential{}, err
	}
	if key != nil {
		signer, perr := ssh.ParsePrivateKey(key)
		if perr != nil {
			// The key bytes must not reach the message: a startup error is
			// printed and travels into logs.
			return credential{}, fmt.Errorf("parsing the ssh private key: %w", perr)
		}
		out.signer = signer

		knownHosts, kerr := resolveSecret(ctx, vc, c.KnownHostsRef, c.KnownHostsFile, defaultKnownHostsField, "known_hosts")
		if kerr != nil {
			return credential{}, kerr
		}
		if knownHosts == nil {
			// Unreachable through the schema phase (`git_known_hosts_required`);
			// kept because this package is also reachable from a caller that
			// built the config struct directly, and a nil callback here would
			// silently mean "go-git searches the keeper's own ~/.ssh".
			return credential{}, errors.New("an ssh key without a known_hosts source")
		}
		// The path is handed on ONLY when it is the source resolveSecret
		// actually read. With both sources set the ref wins, and passing the
		// file anyway would build the db from the losing one — the priority
		// would hold for the key and silently invert for the hosts it is
		// checked against.
		khFile := ""
		if c.KnownHostsRef == "" {
			khFile = c.KnownHostsFile
		}
		db, cerr := knownHostsDB(khFile, knownHosts)
		if cerr != nil {
			return credential{}, cerr
		}
		out.knownHosts = db
	}

	token, err := resolveSecret(ctx, vc, c.TokenRef, c.TokenFile, defaultTokenField, "token")
	if err != nil {
		return credential{}, err
	}
	if token != nil {
		// A file written by `echo` ends in a newline and a token must not.
		// Trimming can empty a whitespace-only source, and that has to fail
		// here: past this point an empty token is indistinguishable from an
		// unconfigured one, and would quietly fall through to no auth.
		out.token = strings.TrimSpace(string(token))
		if out.token == "" {
			return credential{}, errors.New("the token source holds only whitespace")
		}
	}

	return out, nil
}

// resolveSecret reads one secret from its two sources in priority order: the
// vault-ref first, the local file second. Both empty → (nil, nil), meaning the
// entry configures nothing for this secret. `what` names the field in errors.
//
// An empty resolved value is an ERROR, not an absent one: an empty Vault field
// or a truncated file is always a mistake, and for known_hosts it is the
// dangerous kind — it parses into a callback that rejects every host with a
// message about the host key rather than about the config.
func resolveSecret(ctx context.Context, vc KVReader, ref, file, defaultField, what string) ([]byte, error) {
	switch {
	case ref != "":
		val, err := readVaultField(ctx, vc, ref, defaultField)
		if err != nil {
			return nil, fmt.Errorf("%s_ref: %w", what, err)
		}
		if val == "" {
			return nil, fmt.Errorf("%s_ref resolved to an empty value", what)
		}
		return []byte(val), nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s_file: %w", what, err)
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("%s_file %q is empty", what, file)
		}
		return b, nil
	default:
		return nil, nil
	}
}

// readVaultField resolves `vault:<mount>/<path>[#<field>]` to the field's
// string value, defaulting the field when the ref carries no `#` override.
func readVaultField(ctx context.Context, vc KVReader, ref, defaultField string) (string, error) {
	if vc == nil {
		return "", ErrVaultClientRequired
	}
	refPath, field := ref, defaultField
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		refPath, field = ref[:i], ref[i+1:]
	}
	path, err := keepervault.ParseRef(refPath)
	if err != nil {
		return "", err
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return "", fmt.Errorf("read vault %q: %w", path, err)
	}
	raw, ok := kv[field]
	if !ok {
		return "", fmt.Errorf("vault secret %q has no %q field", path, field)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("vault field %q has unsupported type %T (want string)", field, raw)
	}
	return s, nil
}

// knownHostsDB builds the host-key db over go-git's known_hosts reader, which
// is also x/crypto's and OpenSSH's — hashed entries, wildcards,
// `@cert-authority` lines and port forms all behave the way `ssh` does, which
// a hand-written matcher would only approximate.
//
// That reader takes FILE PATHS, so content resolved from Vault is written to a
// temp file and removed again in the same call: the parse is eager (the db is
// fully built before the constructor returns), so nothing reads the path
// afterwards and no plaintext outlives the call. known_hosts is public
// material — host public keys — but leaving files behind per restart is not a
// thing to do either.
//
// Content with no line to read is refused — see [requireKnownHostsEntry] for
// the exact rule and why it is no stricter. That is a floor, not a guarantee
// the db ends up usable: a file whose only entries are `@revoked` lines, or
// that pins hosts other than the one this credential is for, parses fine and
// still refuses every connection. Catching those would mean judging content
// against a host, and the entry's host is not the address go-git dials.
func knownHostsDB(file string, content []byte) (hostKeyDB, error) {
	where := "known_hosts_ref"
	if file != "" {
		where = fmt.Sprintf("known_hosts_file %q", file)
		read, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		content = read
	}
	if err := requireKnownHostsEntry(content); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}

	path := file
	if path == "" {
		f, err := os.CreateTemp("", "keeper-git-known-hosts-*")
		if err != nil {
			return nil, fmt.Errorf("%s: staging file: %w", where, err)
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(content); err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: staging file: %w", where, err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("%s: staging file: %w", where, err)
		}
		path = f.Name()
	}

	db, err := gitssh.NewKnownHostsDb(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	return db, nil
}

// requireKnownHostsEntry reports an error unless the content holds at least one
// line the reader will try to parse.
//
// It mirrors exactly the reader's own skip rule — trim, then drop empty lines
// and ones starting with `#` (x/crypto/ssh/knownhosts hostKeyDB.Read) — and
// judges nothing else. Everything a line can be wrong about is already an error
// from the reader; the one case that is not, and the only one this closes, is a
// file with no line to read at all.
//
// It deliberately does NOT parse. `ssh.ParseKnownHosts` looked like the
// rigorous choice and is a stricter grammar than the reader: it caps an entry
// at five whitespace fields, while the reader stops at the key blob and ignores
// the rest of the line. A perfectly ordinary `<host> ssh-ed25519 AAAA... deploy
// key for gitlab` is six, so that check refused to start the daemon over a
// trailing comment `ssh` itself is fine with.
func requireKnownHostsEntry(content []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading known_hosts: %w", err)
	}
	return errors.New("holds no known_hosts entry (only blank or comment lines)")
}

// Lookup returns the auth method configured for gitURL's host.
//
// The second result is the whole contract: false means "no configured
// credential applies here", and the caller must then do exactly what it did
// before this package existed — the ssh-agent for an ssh URL, nil for an https
// one. It is false for an unknown host, for an ssh URL on a host configured
// with only a token, and for an https URL on a host configured with only a
// key. An entry therefore never disables a fallback it does not replace.
func (s *Store) Lookup(gitURL string) (transport.AuthMethod, bool) {
	if s == nil {
		return nil, false
	}
	host := Host(gitURL)
	if host == "" {
		return nil, false
	}
	c, ok := s.byHost[host]
	if !ok {
		return nil, false
	}
	if IsSSHURL(gitURL) {
		if c.signer == nil {
			return nil, false
		}
		// HostKeyAlgorithms is set HERE and not at load time because it is
		// derived per target address, and it is set at all because go-git only
		// derives it on the branch we are replacing: with a custom
		// HostKeyCallback it leaves the field alone and says so
		// (plumbing/transport/ssh/common.go — "the user is responsible for
		// populating HostKeyAlgorithms appropriately").
		//
		// Left empty, x/crypto advertises its full default preference order,
		// where ed25519 ranks LAST. A known_hosts pinning only the ed25519 key
		// of a host that also serves ECDSA — what `ssh-keyscan -t ed25519`
		// gives — then negotiates the ECDSA key and the pinned callback
		// reports `knownhosts: key mismatch`: an error that reads like a
		// man-in-the-middle, on a host the ssh-agent path reached a minute
		// earlier.
		//
		// The address is [hostPort]'s, not the URL's, because the list has to
		// be derived under the SAME address go-git hands the callback — which
		// includes resolving an `~/.ssh/config` Host alias on the keeper node.
		// An address the db knows nothing about yields an empty list, left as
		// nil: there is nothing better to advertise, and the callback still
		// refuses the host.
		pk := &gitssh.PublicKeys{User: SSHUser(gitURL), Signer: c.signer}
		pk.HostKeyCallback = c.knownHosts.HostKeyCallback()
		pk.HostKeyAlgorithms = c.knownHosts.HostKeyAlgorithms(hostPort(gitURL))
		return pk, true
	}
	if c.token == "" {
		return nil, false
	}
	return &githttp.BasicAuth{Username: basicAuthUser(gitURL), Password: c.token}, true
}

// basicAuthUser picks the basic-auth username: the one spelled in the URL if
// there is one, else [tokenUser]. Honouring the URL costs nothing and keeps an
// operator who needs a specific account from having to put the token beside it.
func basicAuthUser(gitURL string) string {
	if name := URLUser(gitURL); name != "" {
		return name
	}
	return tokenUser
}
