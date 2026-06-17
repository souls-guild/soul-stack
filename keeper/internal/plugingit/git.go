package plugingit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// git-слой резолвера — pure-Go через go-git. Системный бинарь `git` НЕ форкается:
// на keeper-host не требуется runtime-зависимость от `git`, а git-egress
// (HIGH security-риск, ADR-026) защищён by-design самой библиотекой, без флагов:
//
//   - hooks: go-git НЕ исполняет git-hooks ни при clone, ни при checkout —
//     репозиторий-злоумышленник не запустит post-checkout/pre-commit от
//     service-user-а. Это поведение go-git by design, не настраиваемый флаг.
//   - protocol ext::: транспорта `ext::<cmd>` (исполнение внешней команды как
//     «транспорта») в go-git нет в принципе — вектор отсутствует.
//   - protocol file://: контролируется НЕ go-git-ом, а валидацией схемы входного
//     URL (см. [validateGitScheme]) — allowlist https/ssh, file:// только под
//     env-флагом для dev/test.
//   - submodules: НЕ передаём RecurseSubmodules — default go-git = не рекурсить;
//     вложенные submodule-источники не клонируются.
//   - depth: clone/fetch идут с Depth=1 (shallow). Если remote деградирует до
//     полного клона — это допустимо (не ошибка), резолв всё равно корректен.
//   - timeout: clone/fetch выполняются через *Context-варианты с ctx из
//     fetch_timeout — внешний вызов всегда ограничен по времени.

// shallowDepth — глубина shallow-клона/фетча резолвера (F-fetch: нужен ровно
// один коммит ref-а, не история).
const shallowDepth = 1

// fetchAllRefSpec обновляет в рабочем клоне все ветки force-режимом в
// remote-tracking. Узкий refspec на конкретный ref в go-git — hard-ошибка, если
// ref оказался тегом (heads/<ref> не существует); wildcard + Tags=AllTags ниже
// покрывает и ветки, и теги одной операцией (паттерн artifact/git.go).
const fetchAllRefSpec config.RefSpec = "+refs/heads/*:refs/remotes/origin/*"

// openOrClone открывает рабочий клон под workDir или клонирует его shallow,
// если каталог ещё не репозиторий. Битый/пустой каталог пересоздаётся чистым
// клоном. ctx ограничивает clone по времени (fetch_timeout).
func openOrClone(ctx context.Context, workDir, gitURL string, auth transport.AuthMethod) (*git.Repository, error) {
	repo, err := git.PlainOpen(workDir)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("plugingit: открытие рабочего клона %s: %w", workDir, err)
	}

	// Каталог мог существовать как пустой/битый — убираем перед чистым клоном,
	// чтобы PlainCloneContext не упал на ErrRepositoryAlreadyExists.
	if rmErr := os.RemoveAll(workDir); rmErr != nil {
		return nil, fmt.Errorf("plugingit: очистка перед клоном %s: %w", workDir, rmErr)
	}
	repo, err = git.PlainCloneContext(ctx, workDir, false, &git.CloneOptions{
		URL:   gitURL,
		Auth:  auth,
		Depth: shallowDepth,
		Tags:  git.AllTags,
	})
	if err != nil {
		return nil, fmt.Errorf("plugingit: клонирование %s: %w", gitURL, err)
	}
	return repo, nil
}

// fetch обновляет рабочий клон с remote shallow-фетчем: все ветки в
// remote-tracking + все теги (Tags=AllTags). Покрывает и ветку, и тег одной
// операцией, чтобы [resolveRef] нашёл цель независимо от типа ref-а.
// NoErrAlreadyUpToDate — не ошибка.
func fetch(ctx context.Context, repo *git.Repository, auth transport.AuthMethod) error {
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{fetchAllRefSpec},
		Depth:    shallowDepth,
		Force:    true,
		Tags:     git.AllTags,
		Auth:     auth,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("plugingit: fetch: %w", err)
	}
	return nil
}

// resolveRef резолвит git-ref (tag/branch/commit) в полный 40-hex commit-hash
// через ResolveRevision с суффиксом `^{commit}` (peel: tag-объект → коммит,
// гарантия что ref указывает именно на коммит). Тип plumbing.Hash гарантирует
// 40-hex на выходе.
//
// Порядок кандидатов: тег → remote-tracking-ветка → как есть (полный hash).
// Remote-tracking-форма проверяется ПЕРЕД голым `<ref>`: shallow fetch кладёт
// ветку в `refs/remotes/origin/<ref>` и продвигает её, тогда как локальная
// `refs/heads/<ref>` от исходного клона осталась бы на старом коммите.
func resolveRef(repo *git.Repository, ref string) (string, error) {
	candidates := []plumbing.Revision{
		plumbing.Revision("refs/tags/" + ref + "^{commit}"),
		plumbing.Revision("refs/remotes/origin/" + ref + "^{commit}"),
		plumbing.Revision(ref + "^{commit}"),
	}
	for _, rev := range candidates {
		hash, err := repo.ResolveRevision(rev)
		if err == nil {
			return hash.String(), nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrRefNotResolved, ref)
}

// checkout переводит рабочий клон в detached-HEAD на указанный commit. Force —
// чтобы перезатереть остатки предыдущего checkout-а без ручной чистки worktree.
// go-git НЕ исполняет hooks при checkout (by design).
func checkout(repo *git.Repository, sha1 string) error {
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("plugingit: worktree: %w", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Hash:  plumbing.NewHash(sha1),
		Force: true,
	}); err != nil {
		return fmt.Errorf("plugingit: checkout %s: %w", sha1, err)
	}
	return nil
}

// authFor выбирает auth-метод по схеме URL. SSH (`ssh://` или scp-форма
// `user@host:path`) — через SSH-agent (PM-decision, симметрично artifact/git.go;
// Vault-auth post-MVP). `file://`/`https://` auth не требуют (MVP).
func authFor(gitURL string) (transport.AuthMethod, error) {
	if !isSSHURL(gitURL) {
		return nil, nil
	}
	auth, err := gitssh.NewSSHAgentAuth(sshUser(gitURL))
	if err != nil {
		return nil, fmt.Errorf("plugingit: SSH-agent auth для %s: %w", gitURL, err)
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
