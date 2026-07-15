package artifact

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// snapshotter — the shared git→snapshot machinery reused by [ServiceLoader]
// and [DestinyLoader]: a per-name Mutex serializes git operations over one
// working clone (different artifacts proceed in parallel), and materialize
// atomically exports a commit's tree into an immutable snapshot under
// cacheRoot.
//
// Security contract is shared: the git URL scheme is validated via
// [validateGitScheme] (file:// only under SOUL_STACK_ALLOW_FILE_REPOS), the
// name via [newCacheLayout] (kebab-case, cache-path guard).
type snapshotter struct {
	cacheRoot string
	logger    *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newSnapshotter(cacheRoot string, logger *slog.Logger) snapshotter {
	if logger == nil {
		logger = slog.Default()
	}
	return snapshotter{
		cacheRoot: cacheRoot,
		logger:    logger,
		locks:     make(map[string]*sync.Mutex),
	}
}

// lockFor returns (creating if needed) the Mutex for an artifact name.
func (s *snapshotter) lockFor(name string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[name]
	if !ok {
		m = &sync.Mutex{}
		s.locks[name] = m
	}
	return m
}

// snapshot materializes an immutable snapshot of the gitURL repository at
// the commit ref resolves to, and returns (sha1, snapshot path). kind — an
// artifact label for log messages ("сервиса"/"destiny").
//
// Algorithm (PM decisions): open/clone the working clone → fetch → resolve
// ref to sha1. If a snapshot for sha1 already exists — reuse it (immutable);
// otherwise checkout → export the tree to staging → atomic rename into the cache.
func (s *snapshotter) snapshot(ctx context.Context, name, gitURL, ref, kind string) (sha1, dir string, err error) {
	if gitURL == "" {
		return "", "", fmt.Errorf("artifact: git URL пуст для %s %q", kind, name)
	}
	if verr := validateGitScheme(gitURL); verr != nil {
		return "", "", verr
	}
	layout, lerr := newCacheLayout(s.cacheRoot, name)
	if lerr != nil {
		return "", "", lerr
	}

	lock := s.lockFor(name)
	lock.Lock()
	defer lock.Unlock()

	if derr := layout.ensureServiceDir(); derr != nil {
		return "", "", derr
	}

	auth, aerr := authFor(gitURL)
	if aerr != nil {
		return "", "", aerr
	}

	repo, oerr := openOrClone(ctx, layout.workDir(), gitURL, auth)
	if oerr != nil {
		return "", "", oerr
	}
	if ferr := fetch(ctx, repo, auth); ferr != nil {
		return "", "", ferr
	}

	sha1, err = resolveRef(repo, ref)
	if err != nil {
		return "", "", err
	}

	if !layout.snapshotExists(sha1) {
		if merr := s.materialize(layout, repo, sha1); merr != nil {
			return "", "", merr
		}
		s.logger.Info("artifact: материализован снапшот "+kind, "name", name, "ref", ref, "sha1", sha1)
	} else {
		s.logger.Debug("artifact: переиспользован снапшот "+kind, "name", name, "ref", ref, "sha1", sha1)
	}
	return sha1, layout.snapshotDir(sha1), nil
}

// materialize checks out sha1 in the working clone, exports the tree to a
// staging directory, and atomically renames it into the final snapshot.
//
// Atomicity (PM decision): export goes into `_tmp-*`, and only the fully
// ready tree gets renamed to `<sha1>`. An interruption leaves only `_tmp-*`
// behind, never a partial snapshot. On a race (another instance created
// `<sha1>` between the check and the rename), os.Rename onto a non-empty
// directory returns an error — we treat that as "snapshot already exists"
// and keep the other instance's result.
func (s *snapshotter) materialize(layout cacheLayout, repo *gitRepo, sha1 string) error {
	if err := checkout(repo, sha1); err != nil {
		return err
	}
	staging, err := layout.newStagingDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging) // no-op after a successful rename

	if err := exportTree(layout.workDir(), staging); err != nil {
		return err
	}

	target := layout.snapshotDir(sha1)
	if err := os.Rename(staging, target); err != nil {
		if layout.snapshotExists(sha1) {
			return nil
		}
		return fmt.Errorf("artifact: rename снапшота %s: %w", sha1, err)
	}
	return nil
}
