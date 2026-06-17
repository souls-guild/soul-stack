package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// gitRepo — алиас открытого репозитория go-git. Держит go-git-импорт внутри
// git.go: остальной пакет работает с репозиторием как с непрозрачным handle.
type gitRepo = git.Repository

// fetchAllRefSpec обновляет в рабочем клоне все ветки и теги форс-режимом.
// Без этого ветка, продвинувшаяся на remote, не была бы видна локальному
// ResolveRevision (PM-decision: всегда fetch + checkout).
const fetchAllRefSpec config.RefSpec = "+refs/heads/*:refs/remotes/origin/*"

// openOrClone открывает рабочий клон под workDir или клонирует его, если каталог
// ещё не репозиторий либо существующий клон битый (нет origin-remote — keeper
// убит mid-clone). Возвращает открытый репозиторий.
func openOrClone(ctx context.Context, workDir, gitURL string, auth transport.AuthMethod) (*gitRepo, error) {
	repo, err := git.PlainOpen(workDir)
	if err == nil {
		// Self-heal битого клона: keeper, убитый mid-clone, оставляет каталог,
		// который PlainOpen открывает, но без origin-remote — последующий fetch
		// падает ErrRemoteNotFound НАВСЕГДА до ручной чистки. Ловим строго
		// ErrRemoteNotFound (транзиентные ошибки чтения config не сносим) и
		// пере-клонируем. Доступ к workDir сериализован per-name mutex-ом в
		// snapshotter.snapshot, гонки с этим RemoveAll нет.
		_, rerr := repo.Remote("origin")
		if rerr == nil {
			return repo, nil
		}
		if !errors.Is(rerr, git.ErrRemoteNotFound) {
			return nil, fmt.Errorf("artifact: проверка origin-remote клона %s: %w", workDir, rerr)
		}
		// Падаем в общую clone-ветку ниже через RemoveAll + PlainCloneContext.
	} else if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("artifact: открытие рабочего клона %s: %w", workDir, err)
	}

	// Каталог мог существовать как пустой/битый — убираем перед чистым клоном,
	// чтобы PlainCloneContext не упал на ErrRepositoryAlreadyExists.
	if rmErr := os.RemoveAll(workDir); rmErr != nil {
		return nil, fmt.Errorf("artifact: очистка перед клоном %s: %w", workDir, rmErr)
	}
	repo, err = git.PlainCloneContext(ctx, workDir, false, &git.CloneOptions{
		URL:  gitURL,
		Auth: auth,
		Tags: git.AllTags,
	})
	if err != nil {
		return nil, fmt.Errorf("artifact: клонирование %s: %w", gitURL, err)
	}
	return repo, nil
}

// fetch обновляет рабочий клон с remote. NoErrAlreadyUpToDate — не ошибка.
func fetch(ctx context.Context, repo *gitRepo, auth transport.AuthMethod) error {
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{fetchAllRefSpec},
		Tags:     git.AllTags,
		Force:    true,
		Auth:     auth,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("artifact: fetch: %w", err)
	}
	return nil
}

// resolveRef резолвит git-ref (tag/branch/HEAD) в commit-hash. Пустой ref — HEAD.
//
// Remote-tracking-форма (`refs/remotes/origin/<ref>`) проверяется ПЕРЕД голым
// `<ref>`: fetch кладёт ветки в `refs/remotes/origin/*` (см. fetchAllRefSpec) и
// продвигает их, тогда как локальная `refs/heads/<ref>` от исходного клона
// остаётся на старом коммите (мы делаем fetch, а не pull). Без этого
// приоритета продвинувшаяся ветка резолвилась бы в устаревший tip.
//
// Порядок: тег → remote-tracking-ветка → как есть (полный hash / HEAD).
func resolveRef(repo *gitRepo, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	candidates := []plumbing.Revision{
		plumbing.Revision("refs/tags/" + ref),
		plumbing.Revision("refs/remotes/origin/" + ref),
		plumbing.Revision(ref),
	}
	var lastErr error
	for _, rev := range candidates {
		hash, err := repo.ResolveRevision(rev)
		if err == nil {
			return hash.String(), nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("artifact: ref %q не резолвится в репозитории: %w", ref, lastErr)
}

// checkout переводит рабочий клон в detached-HEAD на указанный commit. Force —
// чтобы перезатереть остатки предыдущего checkout-а без ручной чистки worktree.
func checkout(repo *gitRepo, sha1 string) error {
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("artifact: worktree: %w", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Hash:  plumbing.NewHash(sha1),
		Force: true,
	}); err != nil {
		return fmt.Errorf("artifact: checkout %s: %w", sha1, err)
	}
	return nil
}

// exportTree копирует дерево файлов рабочего клона из src в dst, опуская каталог
// `.git`. Снапшот остаётся чистым деревом сервиса без git-метаданных.
func exportTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// `.git` и всё под ним пропускаем целиком.
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			// Симлинки и спец-файлы в снапшот не переносим — рендер читает
			// только обычные файлы, символьная цель — потенциальный escape.
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// authFor выбирает auth-метод по схеме URL. SSH (`ssh://` или scp-форма
// `user@host:path`) — через SSH-agent (PM-decision; Vault-auth post-MVP).
// `file://`/`https://` auth не требуют (MVP).
func authFor(gitURL string) (transport.AuthMethod, error) {
	if !isSSHURL(gitURL) {
		return nil, nil
	}
	user := sshUser(gitURL)
	auth, err := gitssh.NewSSHAgentAuth(user)
	if err != nil {
		return nil, fmt.Errorf("artifact: SSH-agent auth для %s: %w", gitURL, err)
	}
	return auth, nil
}

// isSSHURL распознаёт ssh-схему и scp-подобную форму `git@host:org/repo.git`.
func isSSHURL(gitURL string) bool {
	if strings.HasPrefix(gitURL, "ssh://") {
		return true
	}
	if strings.Contains(gitURL, "://") {
		return false
	}
	// scp-форма: `user@host:path`, двоеточие после хоста, без схемы.
	at := strings.Index(gitURL, "@")
	colon := strings.Index(gitURL, ":")
	return at >= 0 && colon > at
}

// sshUser извлекает имя пользователя из ssh-URL; по умолчанию `git`.
func sshUser(gitURL string) string {
	if strings.HasPrefix(gitURL, "ssh://") {
		if u, err := url.Parse(gitURL); err == nil && u.User != nil {
			if name := u.User.Username(); name != "" {
				return name
			}
		}
		return "git"
	}
	if at := strings.Index(gitURL, "@"); at > 0 {
		return gitURL[:at]
	}
	return "git"
}
