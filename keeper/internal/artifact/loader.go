package artifact

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// snapshotter — общая git→снапшот-механика, переиспользуемая
// [ServiceLoader] и [DestinyLoader]: per-имя Mutex сериализует git-операции над
// одним рабочим клоном (разные артефакты идут параллельно), а materialize
// атомарно экспортирует дерево commit-а в immutable-снапшот под cacheRoot.
//
// Контракт безопасности — общий: схема git-URL валидируется через
// [validateGitScheme] (file:// только под SOUL_STACK_ALLOW_FILE_REPOS), имя —
// через [newCacheLayout] (kebab-case, защита cache-пути).
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

// lockFor возвращает (создавая при необходимости) Mutex для имени артефакта.
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

// snapshot материализует immutable-снапшот репозитория gitURL на commit-е, в
// который резолвится ref, и возвращает (sha1, путь снапшота). kind — метка
// артефакта для лог-сообщений ("сервиса"/"destiny").
//
// Алгоритм (PM-decisions): открыть/клонировать рабочий клон → fetch → resolve
// ref в sha1. Если снапшот для sha1 уже есть — переиспользовать (immutable);
// иначе checkout → export дерева в staging → атомарный rename в кеш.
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

// materialize выполняет checkout sha1 в рабочем клоне, экспортирует дерево в
// staging-каталог и атомарно переименовывает его в финальный снапшот.
//
// Атомарность (PM-decision): export идёт в `_tmp-*`, и только полностью готовое
// дерево rename-ится в `<sha1>`. Прерывание оставляет лишь `_tmp-*`, но не
// частичный снапшот. На гонке (другой инстанс успел создать `<sha1>` между
// проверкой и rename) os.Rename на непустой каталог вернёт ошибку — трактуем её
// как «снапшот уже есть» и оставляем чужой результат.
func (s *snapshotter) materialize(layout cacheLayout, repo *gitRepo, sha1 string) error {
	if err := checkout(repo, sha1); err != nil {
		return err
	}
	staging, err := layout.newStagingDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging) // no-op после успешного rename

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
