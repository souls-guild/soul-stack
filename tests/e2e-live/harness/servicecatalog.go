package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The out-of-tree services this tier drives, and where a run gets them from.
//
// NIM-876. A service is its own repository (NIM-871), which leaves the live tier with a
// subject it does not contain — and two ways to reach one, both with a cost the ticket
// asked to be named rather than absorbed:
//
//   - CLONE IT PER RUN from the published URL. The gate stops being reproducible offline
//     and its verdict starts depending on github.com being up, which is the dependency
//     NIM-542 spent a ticket removing from this same gate.
//   - READ A CHECKOUT ON DISK. Then the gate proves whatever happened to be in that
//     directory: a colleague's half-finished edit, a stale clone, a rebase in flight. A
//     blocking pre-tag gate whose subject is "the working tree next door" has no verdict
//     to give.
//
// So: a PINNED COMMIT, fetched once into a cache outside the repository, extracted to a
// path that carries the commit in its name. The pin is what makes the run reproducible —
// byte for byte, on this machine and on anyone else's — and the cache is what makes it
// offline after the first fill. Neither half works alone: a cache without a pin is the
// second failure above with extra steps, and a pin without a cache is the first.
//
// Untagged on purpose, like setupdecl.go: everything here is a claim about a subject, and
// a claim that only compiles with docker present is one that drifts unobserved. The
// docker-free guards in servicecatalog_test.go hold the pins to their own rules and run in
// `make e2e-live-gate`'s first step.

// serviceCacheEnv — where the cache lives, when the default is wrong (CI with a warm
// shared volume, or a machine whose $HOME is not writable).
const serviceCacheEnv = "SOUL_STACK_E2E_SERVICE_CACHE"

// serviceOfflineEnv — set to 1/true to forbid the primer from reaching the network.
//
// With a warm cache this is how "the gate needs nothing from github.com" is DEMONSTRATED
// rather than asserted: a missing commit then fails by name instead of quietly restoring
// the dependency the pin exists to remove.
const serviceOfflineEnv = "SOUL_STACK_E2E_SERVICE_OFFLINE"

// serviceRemoteEnvPrefix — `SOUL_STACK_E2E_SERVICE_REMOTE_<ALIAS>`: fetch the pinned
// commit from somewhere other than the published URL — a local clone, an internal mirror,
// a machine with no route to github.
//
// This does NOT weaken the verdict and that is the whole point of keeping it separate from
// the override below: the commit is still the pinned one, verified after the fetch, so the
// bytes the gate runs are the same bytes either way. Only their delivery changes.
const serviceRemoteEnvPrefix = "SOUL_STACK_E2E_SERVICE_REMOTE_"

// serviceDirEnvPrefix — `SOUL_STACK_E2E_SERVICE_DIR_<ALIAS>`: run a working tree instead
// of the pin, for developing a change to a service and the engine together.
//
// ★ This DOES weaken the verdict, so `make e2e-live-gate` refuses to start while it is
// set. A gate run under this variable would report on a directory nobody can name, which
// is the failure the pin exists to prevent — and the refusal has to live in the recipe,
// because by the time a test could complain it has already become the thing it is
// complaining about.
const serviceDirEnvPrefix = "SOUL_STACK_E2E_SERVICE_DIR_"

// serviceFetchTimeout — budget for the one-time priming fetch of one service repository.
// Generous: it runs at most once per machine per pin bump, and a partial fetch left in the
// cache would be worse than a slow one.
const serviceFetchTimeout = 5 * time.Minute

// externalService — one out-of-tree service repository, pinned.
type externalService struct {
	// alias — the name the service is registered under in the stand's service registry,
	// and the directory it is cached in. Kept short and equal to the repository name.
	alias string

	// url — the published clone URL. https rather than ssh: the primer runs unattended
	// and on a machine with no key for this repository it must still be able to fill the
	// cache.
	url string

	// commit — the exact tree the gate runs, full 40-hex. A tag would not do: a tag is
	// movable, and "the gate runs v1.2.0" stops being a fact the moment someone retags.
	commit string

	// why — what this service is the subject OF, in one line, for the error a cold cache
	// prints. A reader who has never seen this file gets told what they are missing.
	why string
}

// serviceCatalog — the pinned subjects.
//
// ★ BUMPING A PIN IS A DECISION, not maintenance: the new commit is what the blocking
// pre-tag gate will prove, so it has to have been run. `make e2e-live-gate` against the
// candidate commit is the whole procedure — there is no CI for the service repository and
// nothing else looks at it.
func serviceCatalog() []externalService {
	return []externalService{
		{
			alias:  "redis",
			url:    "https://github.com/soul-stack-services/redis.git",
			commit: "b89c062d5f6ad71e458be752b52f70dc955ca5f2",
			why: "a live service being created and then operated: create_from_souls " +
				"(roster-at-create, ADR-0081) and add_user (day-2 through the plugin channel)",
		},
	}
}

// serviceByAlias finds a catalogued service, or says which aliases exist.
func serviceByAlias(alias string) (externalService, error) {
	cat := serviceCatalog()
	for _, svc := range cat {
		if svc.alias == alias {
			return svc, nil
		}
	}
	names := make([]string, 0, len(cat))
	for _, svc := range cat {
		names = append(names, svc.alias)
	}
	return externalService{}, fmt.Errorf("no external service %q in serviceCatalog(); it carries %s",
		alias, strings.Join(names, ", "))
}

// reFullSHA — a git object name spelled in full. Anything shorter is ambiguous today and
// may be ambiguous against a different commit tomorrow.
var reFullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// serviceCacheDir — the cache root: $SOUL_STACK_E2E_SERVICE_CACHE, else
// $XDG_CACHE_HOME/soul-stack/e2e-live/services, else ~/.cache/… . Outside the repository
// and outside $TMPDIR both, for the reason the artifact cache used to give: it has to
// survive `go clean`, `git clean` and a reboot, or "offline" would mean "clones once per
// run" instead of "clones once".
func serviceCacheDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(serviceCacheEnv)); dir != "" {
		return dir, nil
	}
	base := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("service cache: no %s and no home directory: %w", serviceCacheEnv, err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "soul-stack", "e2e-live", "services"), nil
}

// serviceRepoDir — where the clone of one service lives inside the cache.
func serviceRepoDir(root string, svc externalService) string {
	return filepath.Join(root, "repo", svc.alias+".git")
}

// serviceTreeDir — where the extracted pin lives. The commit is IN THE PATH, and that is
// not cosmetic: an extracted tree is never updated in place, so the directory a test reads
// cannot hold anything but the commit it is named after. A pin bump adds a directory
// instead of rewriting one, which also leaves the previous pin runnable on a bisect.
func serviceTreeDir(root string, svc externalService) string {
	return filepath.Join(root, "tree", svc.alias, svc.commit)
}

// serviceEnvSuffix — the alias as it appears in an environment variable name: upper case,
// every character git allows in a directory name but sh does not allow in an identifier
// replaced by `_`.
func serviceEnvSuffix(alias string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(alias) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// serviceDirOverride returns the working tree to run instead of the pin, if one is named.
func serviceDirOverride(alias string) string {
	return strings.TrimSpace(os.Getenv(serviceDirEnvPrefix + serviceEnvSuffix(alias)))
}

// OverriddenServiceDirs — every alias whose pin is currently overridden by a working tree,
// with the path. Empty when the pins are in force.
//
// Read by the gate recipe through `go run ./cmd/service-cache -check-overrides`: the
// refusal belongs where the run is launched, not inside a test that would already be
// reporting on the wrong subject.
func OverriddenServiceDirs() map[string]string {
	out := map[string]string{}
	for _, svc := range serviceCatalog() {
		if dir := serviceDirOverride(svc.alias); dir != "" {
			out[svc.alias] = dir
		}
	}
	return out
}

// serviceFetchURL — where to fetch this service's pinned commit from.
func serviceFetchURL(svc externalService) string {
	if u := strings.TrimSpace(os.Getenv(serviceRemoteEnvPrefix + serviceEnvSuffix(svc.alias))); u != "" {
		return u
	}
	return svc.url
}

// serviceOffline — is the primer forbidden to reach the network?
func serviceOffline() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(serviceOfflineEnv))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// PrimeServiceCache materializes every catalogued service at its pinned commit and reports
// where the cache is. This is what `make e2e-live-services` runs and what the live gate
// runs as a named step of its own.
//
// NewStack does not prime: a test that needed a subject the cache does not have would
// otherwise discover it after bringing up five containers, and report a cold cache as a
// stand failure. Here it is one line of output and half a minute, before anything starts.
func PrimeServiceCache() (dir string, err error) {
	root, err := serviceCacheDir()
	if err != nil {
		return "", err
	}
	for _, svc := range serviceCatalog() {
		if _, err := ensureServiceTree(root, svc); err != nil {
			return root, err
		}
	}
	return root, nil
}

// ensureServiceTree returns the path holding svc's pinned commit, fetching and extracting
// it if the cache does not have it yet. Idempotent.
func ensureServiceTree(root string, svc externalService) (string, error) {
	if !reFullSHA.MatchString(svc.commit) {
		return "", fmt.Errorf("service %q: pinned commit %q is not a full 40-hex object name",
			svc.alias, svc.commit)
	}

	tree := serviceTreeDir(root, svc)
	if ok, err := dirHasEntries(tree); err != nil {
		return "", err
	} else if ok {
		return tree, nil
	}

	repo := serviceRepoDir(root, svc)
	if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
		return "", fmt.Errorf("service %q: mkdir %s: %w", svc.alias, filepath.Dir(repo), err)
	}
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("service %q: stat %s: %w", svc.alias, repo, err)
		}
		if out, err := runServiceGit("", serviceFetchTimeout, "init", "--bare", "-q", repo); err != nil {
			return "", fmt.Errorf("service %q: git init %s: %w\n%s", svc.alias, repo, err, out)
		}
	}

	if !commitPresent(repo, svc.commit) {
		if serviceOffline() {
			return "", fmt.Errorf("service %q: commit %s is not in the cache and %s forbids fetching it.\n"+
				"  The cache is %s.\n"+
				"  Prime it on a machine with a route to %s:\n"+
				"      make e2e-live-services",
				svc.alias, svc.commit, serviceOfflineEnv, repo, serviceFetchURL(svc))
		}
		// Every ref rather than the bare object: fetching one sha needs the server to
		// allow it, and a service repository is small enough that asking for all of it
		// costs less than a fetch that works on one host and not the next.
		url := serviceFetchURL(svc)
		if out, err := runServiceGit(repo, serviceFetchTimeout,
			"fetch", "--quiet", "--tags", url,
			"+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return "", fmt.Errorf("service %q: git fetch %s: %w\n%s", svc.alias, url, err, out)
		}
	}
	if !commitPresent(repo, svc.commit) {
		return "", fmt.Errorf("service %q: %s does not carry commit %s.\n"+
			"  The pin names a commit that is not published there — the usual cause is a pin bumped\n"+
			"  to a local commit that was never pushed. What this gate proves has to be fetchable by\n"+
			"  whoever runs it next.",
			svc.alias, serviceFetchURL(svc), svc.commit)
	}

	// Extract into a sibling and rename: a run interrupted mid-extraction must not leave a
	// half-populated directory behind, because the check above treats a non-empty tree
	// directory as the finished article.
	staging := tree + ".partial"
	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("service %q: clear %s: %w", svc.alias, staging, err)
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", fmt.Errorf("service %q: mkdir %s: %w", svc.alias, staging, err)
	}
	if out, err := extractCommit(repo, svc.commit, staging); err != nil {
		return "", fmt.Errorf("service %q: extract %s: %w\n%s", svc.alias, svc.commit, err, out)
	}
	if err := os.MkdirAll(filepath.Dir(tree), 0o755); err != nil {
		return "", fmt.Errorf("service %q: mkdir %s: %w", svc.alias, filepath.Dir(tree), err)
	}
	if err := os.Rename(staging, tree); err != nil {
		return "", fmt.Errorf("service %q: rename %s -> %s: %w", svc.alias, staging, tree, err)
	}
	return tree, nil
}

// commitPresent — does this repository hold that commit object?
func commitPresent(repo, commit string) bool {
	_, err := runServiceGit(repo, 30*time.Second, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// extractCommit writes the tree of one commit into dest. `git archive | tar -x` rather
// than a worktree: what comes out is a plain directory with no git metadata and no link
// back to the cache, which is what the fixture copies from — and it cannot be dirtied,
// because there is nothing to dirty.
func extractCommit(repo, commit, dest string) (string, error) {
	archive := exec.Command("git", "--git-dir="+repo, "archive", "--format=tar", commit)
	untar := exec.Command("tar", "-x", "-C", dest)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return "", err
	}
	untar.Stdin = pipe
	var stderr strings.Builder
	archive.Stderr = &stderr
	untar.Stderr = &stderr
	if err := untar.Start(); err != nil {
		return stderr.String(), err
	}
	if err := archive.Run(); err != nil {
		_ = untar.Wait()
		return stderr.String(), err
	}
	if err := untar.Wait(); err != nil {
		return stderr.String(), err
	}
	return stderr.String(), nil
}

// dirHasEntries — does this path exist and hold at least one entry? An empty directory
// counts as absent: it is what a failed extraction leaves, and treating it as a warm cache
// would register a service with no scenarios in it.
func dirHasEntries(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return len(entries) > 0, nil
}

// runServiceGit runs git in a bare repository (or nowhere, for `init`) with a timeout and
// a deterministic environment.
func runServiceGit(repo string, timeout time.Duration, args ...string) (string, error) {
	full := args
	if repo != "" {
		full = append([]string{"--git-dir=" + repo}, args...)
	}
	cmd := exec.Command("git", full...)
	// Terminal prompts off: an unreachable or private URL must fail with a message, not
	// block a gate run forever on a credential prompt nobody is there to answer.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
		return string(out), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return string(out), fmt.Errorf("git %v: timed out after %s", args, timeout)
	}
}
