package artifact

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// GitRef — одна запись git-ref-листинга: tag или branch с commit-hash-ом.
//
// Name — голое имя ref-а (`v2.0.0` / `main`), без `refs/tags/` / `refs/heads/`-
// префикса. Type — `"tag"` либо `"branch"`. Commit — полный sha1. IsDefault —
// true только для дефолтной ветки remote (HEAD-symref); у tag-ов всегда false.
//
// Используется UI-слоем (`GET /v1/services/{name}/refs`) для рендера dropdown
// «git-ref» в Upgrade-modal: выбор из реальных ref-ов вместо свободного ввода.
type GitRef struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Commit    string `json:"commit"`
	IsDefault bool   `json:"is_default,omitempty"`
}

// Тип-маркеры GitRef.Type (стабильны для UI/JSON).
const (
	GitRefTypeTag    = "tag"
	GitRefTypeBranch = "branch"
)

// RefsLister — поверхность listing-а git-ref-ов remote-репозитория. Объявлено
// интерфейсом, чтобы кешер (`serviceregistry.RefsCache`) и handler могли
// принимать минимальную зависимость; реализация — [ListRefs].
type RefsLister interface {
	ListRefs(ctx context.Context, gitURL string) ([]GitRef, error)
}

// RefsListerFunc — функциональная реализация [RefsLister] (handler-у удобно
// передавать чистую функцию, а не строить wrapper-тип).
type RefsListerFunc func(ctx context.Context, gitURL string) ([]GitRef, error)

// ListRefs делает функцию реализующей [RefsLister].
func (f RefsListerFunc) ListRefs(ctx context.Context, gitURL string) ([]GitRef, error) {
	return f(ctx, gitURL)
}

// ListRefs опрашивает remote `gitURL` и возвращает все теги + ветки (анализ
// ref-prefix-а). Семантически — `git ls-remote --tags --heads`, реализуется
// через go-git `remote.ListContext` (один RPC, без клонирования; SSH-auth —
// через [authFor], тот же путь, что у snapshotter-а).
//
// Сортировка детерминирована и предсказуема для UI dropdown:
//
//   - Tags — semver desc (валидный semver выше lex), при равенстве lex desc;
//     v-prefix допустим и сравнивается без него.
//   - Branches — `main`/`master` всегда первыми (помечены IsDefault, если они
//     совпадают с HEAD-symref remote-а), остальные lex asc.
//   - Результат: сначала все tag-и, затем все branch-и (раздельные блоки;
//     UI обычно группирует их визуально).
//
// Auth: SSH через ssh-agent (см. [authFor]; Vault-auth — post-MVP). Схема
// валидируется через [validateGitScheme] (`file://` только под env-флагом).
// Ошибки git/network возвращаются как есть caller-у — handler выше маппит их
// в 502 Bad Gateway (внешний git-источник unreachable / отказал).
func ListRefs(ctx context.Context, gitURL string) ([]GitRef, error) {
	if gitURL == "" {
		return nil, fmt.Errorf("artifact: git URL пуст")
	}
	if err := validateGitScheme(gitURL); err != nil {
		return nil, err
	}
	auth, err := authFor(gitURL)
	if err != nil {
		return nil, err
	}

	// In-memory remote: не материализуем клон, ls-remote ходит только по
	// info/refs (HTTP-smart) или ssh-side ls. Тип Remote-а с конфигом без
	// in-memory storage достаточно: ListContext не требует object-store.
	remote := git.NewRemote(nil, &config.RemoteConfig{
		Name: "origin",
		URLs: []string{gitURL},
	})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return nil, fmt.Errorf("artifact: ls-remote %s: %w", gitURL, err)
	}
	return classifyRefs(refs), nil
}

// classifyRefs разбирает плоский список [*plumbing.Reference] на отсортированные
// блоки (tags semver-desc, затем branches с дефолтной первой). Чистая функция
// для удобства теста: не требует remote-а / сети.
//
// HEAD-symref (если remote его прислал) используется для пометки IsDefault на
// совпадающей ветке. Annotated-tag-объекты в ls-remote приходят парой
// `refs/tags/<name>` + `refs/tags/<name>^{}` — peeled-форма (`^{}`) даёт commit-
// hash, голая — sha объекта тега. Берём peeled, если он есть (commit «куда
// указывает тег»), иначе голую (lightweight tag = прямой commit-hash).
func classifyRefs(refs []*plumbing.Reference) []GitRef {
	const (
		tagPrefix    = "refs/tags/"
		headPrefix   = "refs/heads/"
		peeledSuffix = "^{}"
	)

	// peeled[<name>] = commit для annotated-тегов (заменяет sha тег-объекта).
	peeled := make(map[string]string)
	for _, r := range refs {
		name := r.Name().String()
		if !strings.HasPrefix(name, tagPrefix) || !strings.HasSuffix(name, peeledSuffix) {
			continue
		}
		short := strings.TrimSuffix(strings.TrimPrefix(name, tagPrefix), peeledSuffix)
		peeled[short] = r.Hash().String()
	}

	// HEAD-symref для пометки IsDefault. plumbing.Reference при ls-remote
	// HEAD приходит как SymbolicReference на refs/heads/<branch>.
	defaultBranch := ""
	for _, r := range refs {
		if r.Name() == plumbing.HEAD && r.Type() == plumbing.SymbolicReference {
			target := r.Target().String()
			if strings.HasPrefix(target, headPrefix) {
				defaultBranch = strings.TrimPrefix(target, headPrefix)
			}
			break
		}
	}

	var tags, branches []GitRef
	for _, r := range refs {
		name := r.Name().String()
		switch {
		case strings.HasPrefix(name, tagPrefix):
			if strings.HasSuffix(name, peeledSuffix) {
				continue // peeled-форма учтена в map-е
			}
			short := strings.TrimPrefix(name, tagPrefix)
			commit := r.Hash().String()
			if c, ok := peeled[short]; ok {
				commit = c
			}
			tags = append(tags, GitRef{Name: short, Type: GitRefTypeTag, Commit: commit})
		case strings.HasPrefix(name, headPrefix):
			short := strings.TrimPrefix(name, headPrefix)
			branches = append(branches, GitRef{
				Name:      short,
				Type:      GitRefTypeBranch,
				Commit:    r.Hash().String(),
				IsDefault: short == defaultBranch,
			})
		}
	}

	// Fallback: если HEAD-symref remote не прислал, помечаем main/master как
	// default по convention (UI всё равно нужен якорь для preselect-а).
	if defaultBranch == "" {
		fallback := pickFallbackDefault(branches)
		if fallback != "" {
			for i := range branches {
				if branches[i].Name == fallback {
					branches[i].IsDefault = true
					break
				}
			}
		}
	}

	sortTags(tags)
	sortBranches(branches)

	out := make([]GitRef, 0, len(tags)+len(branches))
	out = append(out, tags...)
	out = append(out, branches...)
	return out
}

// sortTags — semver desc; не-semver идут после semver-валидных, между собой
// lex desc. v-prefix допустим (`v2.0.0` == `2.0.0` для сравнения).
func sortTags(tags []GitRef) {
	sort.Slice(tags, func(i, j int) bool {
		si, oki := parseSemver(tags[i].Name)
		sj, okj := parseSemver(tags[j].Name)
		switch {
		case oki && okj:
			if c := compareSemver(si, sj); c != 0 {
				return c > 0 // desc
			}
			return tags[i].Name > tags[j].Name
		case oki && !okj:
			return true
		case !oki && okj:
			return false
		default:
			return tags[i].Name > tags[j].Name
		}
	})
}

// sortBranches — default первая, остальные lex asc.
func sortBranches(branches []GitRef) {
	sort.SliceStable(branches, func(i, j int) bool {
		if branches[i].IsDefault != branches[j].IsDefault {
			return branches[i].IsDefault
		}
		return branches[i].Name < branches[j].Name
	})
}

// pickFallbackDefault — `main` либо `master`, если они есть в списке веток.
// Применяется ТОЛЬКО когда remote не вернул HEAD-symref (старые серверы,
// `file://`-clones без HEAD). Возвращает имя или пустую строку.
func pickFallbackDefault(branches []GitRef) string {
	for _, b := range branches {
		if b.Name == "main" {
			return "main"
		}
	}
	for _, b := range branches {
		if b.Name == "master" {
			return "master"
		}
	}
	return ""
}

// semver — минимальная структура для сравнения tag-ов: только major/minor/patch
// (pre-release/build-metadata в UI dropdown сортировать строго по spec-у — over-
// engineering). Pre-release suffix сохраняется в Pre для tie-break-а через lex.
type semver struct {
	Major, Minor, Patch int
	Pre                 string
}

// parseSemver принимает `[v]MAJOR.MINOR.PATCH[-pre]` и возвращает (semver, true)
// при успешном разборе. Любое отклонение — false (тег попадает в «не-semver»-
// блок и сортируется лексикографически).
func parseSemver(tag string) (semver, bool) {
	s := strings.TrimPrefix(tag, "v")
	pre := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return semver{}, false
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return semver{}, false
	}
	pat, err := strconv.Atoi(parts[2])
	if err != nil {
		return semver{}, false
	}
	return semver{Major: maj, Minor: min, Patch: pat, Pre: pre}, true
}

// compareSemver возвращает -1/0/1: a < b / a == b / a > b. Pre-release-версия
// (`-rc.1`) меньше release (по semver-spec §11); между собой pre-releases
// сравниваются лексикографически (упрощение MVP, достаточно для desc-сортировки
// UI dropdown).
func compareSemver(a, b semver) int {
	if a.Major != b.Major {
		return cmpInt(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return cmpInt(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return cmpInt(a.Patch, b.Patch)
	}
	// pre == "" — release, выше любой pre-release.
	if a.Pre == "" && b.Pre != "" {
		return 1
	}
	if a.Pre != "" && b.Pre == "" {
		return -1
	}
	if a.Pre == b.Pre {
		return 0
	}
	if a.Pre < b.Pre {
		return -1
	}
	return 1
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
