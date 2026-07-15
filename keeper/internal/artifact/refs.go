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

// GitRef — a single entry of a git ref listing: a tag or branch with its
// commit hash.
//
// Name — the bare ref name (`v2.0.0` / `main`), without a `refs/tags/` /
// `refs/heads/` prefix. Type — `"tag"` or `"branch"`. Commit — the full sha1.
// IsDefault — true only for the remote's default branch (HEAD symref); always
// false for tags.
//
// Used by the UI layer (`GET /v1/services/{name}/refs`) to render the
// "git-ref" dropdown in the Upgrade modal: picking from real refs instead of
// free-form input.
type GitRef struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Commit    string `json:"commit"`
	IsDefault bool   `json:"is_default,omitempty"`
}

// Type markers for GitRef.Type (stable for UI/JSON).
const (
	GitRefTypeTag    = "tag"
	GitRefTypeBranch = "branch"
)

// RefsLister — the surface for listing a remote repository's git refs.
// Declared as an interface so that the cacher (`serviceregistry.RefsCache`)
// and the handler can accept a minimal dependency; the implementation is
// [ListRefs].
type RefsLister interface {
	ListRefs(ctx context.Context, gitURL string) ([]GitRef, error)
}

// RefsListerFunc — a functional implementation of [RefsLister] (convenient
// for a handler to pass a plain function instead of building a wrapper
// type).
type RefsListerFunc func(ctx context.Context, gitURL string) ([]GitRef, error)

// ListRefs makes the function implement [RefsLister].
func (f RefsListerFunc) ListRefs(ctx context.Context, gitURL string) ([]GitRef, error) {
	return f(ctx, gitURL)
}

// ListRefs queries the remote `gitURL` and returns all tags + branches
// (ref-prefix analysis). Semantically — `git ls-remote --tags --heads`,
// implemented via go-git's `remote.ListContext` (a single RPC, no cloning;
// SSH auth — via [authFor], the same path the snapshotter uses).
//
// Sorting is deterministic and predictable for the UI dropdown:
//
//   - Tags — semver desc (a valid semver ranks above lex), lex desc on ties;
//     a v-prefix is allowed and compared without it.
//   - Branches — `main`/`master` always come first (marked IsDefault if they
//     match the remote's HEAD symref), the rest are lex asc.
//   - Result: all tags first, then all branches (separate blocks; the UI
//     usually groups them visually).
//
// Auth: SSH via ssh-agent (see [authFor]; Vault auth is post-MVP). The scheme
// is validated via [validateGitScheme] (`file://` only under the env flag).
// git/network errors are returned to the caller as-is — the handler above
// maps them to 502 Bad Gateway (the external git source is unreachable /
// failed).
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

	// In-memory remote: we don't materialize a clone, ls-remote only hits
	// info/refs (HTTP-smart) or ssh-side ls. A Remote with a config and no
	// in-memory storage is enough: ListContext doesn't need an object store.
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

// classifyRefs splits a flat list of [*plumbing.Reference] into sorted blocks
// (tags semver-desc, then branches with the default one first). A pure
// function for testability: needs no remote / network.
//
// The HEAD symref (if the remote sent one) is used to mark IsDefault on the
// matching branch. Annotated tag objects arrive from ls-remote as a pair:
// `refs/tags/<name>` + `refs/tags/<name>^{}` — the peeled form (`^{}`) gives
// the commit hash, the bare one gives the tag object's sha. We take the
// peeled one if present (the commit "the tag points to"), otherwise the bare
// one (a lightweight tag = a direct commit hash).
func classifyRefs(refs []*plumbing.Reference) []GitRef {
	const (
		tagPrefix    = "refs/tags/"
		headPrefix   = "refs/heads/"
		peeledSuffix = "^{}"
	)

	// peeled[<name>] = commit for annotated tags (replaces the tag object's sha).
	peeled := make(map[string]string)
	for _, r := range refs {
		name := r.Name().String()
		if !strings.HasPrefix(name, tagPrefix) || !strings.HasSuffix(name, peeledSuffix) {
			continue
		}
		short := strings.TrimSuffix(strings.TrimPrefix(name, tagPrefix), peeledSuffix)
		peeled[short] = r.Hash().String()
	}

	// HEAD symref for marking IsDefault. During ls-remote, HEAD arrives as a
	// plumbing.Reference SymbolicReference pointing at refs/heads/<branch>.
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
				continue // the peeled form is already accounted for in the map
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

	// Fallback: if the remote didn't send a HEAD symref, mark main/master as
	// default by convention (the UI still needs an anchor for preselection).
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

// sortTags — semver desc; non-semver ones go after semver-valid ones, lex
// desc among themselves. A v-prefix is allowed (`v2.0.0` == `2.0.0` for
// comparison).
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

// sortBranches — the default one first, the rest lex asc.
func sortBranches(branches []GitRef) {
	sort.SliceStable(branches, func(i, j int) bool {
		if branches[i].IsDefault != branches[j].IsDefault {
			return branches[i].IsDefault
		}
		return branches[i].Name < branches[j].Name
	})
}

// pickFallbackDefault — `main` or `master`, if present in the branch list.
// Applied ONLY when the remote didn't return a HEAD symref (old servers,
// `file://` clones without HEAD). Returns the name or an empty string.
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

// semver — a minimal structure for comparing tags: only major/minor/patch
// (sorting pre-release/build-metadata strictly per spec in a UI dropdown
// would be over-engineering). The pre-release suffix is kept in Pre for a
// lex tie-break.
type semver struct {
	Major, Minor, Patch int
	Pre                 string
}

// parseSemver accepts `[v]MAJOR.MINOR.PATCH[-pre]` and returns (semver, true)
// on a successful parse. Any deviation — false (the tag falls into the
// "non-semver" block and is sorted lexicographically).
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

// compareSemver returns -1/0/1: a < b / a == b / a > b. A pre-release version
// (`-rc.1`) is less than a release (per semver-spec §11); pre-releases are
// compared lexicographically among themselves (an MVP simplification, good
// enough for the UI dropdown's desc sort).
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
	// pre == "" — a release, ranks above any pre-release.
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
