// Package plugingit — git resolver for Keeper-side plugin sources
// (ADR-026 Sigil, F-fetch approach, slice A1-S1).
//
// Before A1-S1 catalog `keeper.yml::plugins.{cloud_drivers,ssh_providers}` carried
// `source`/`ref`, but they were NOT used — binary reached cache slot outside
// Soul Stack. Resolver closes this: for each catalog entry Keeper itself
// git-resolves `source`+`ref` to commit_sha slot, extracting ALREADY-BUILT binary
// from `dist/<binary-name>` (F-fetch — no compilation on Keeper).
//
// Cache layout (R-nested layout, A1-S1):
//
//	<cacheRoot>/
//	  <ns>-<name>/
//	    current -> <commit_sha>        # symlink to active slot (atomic)
//	    <commit_sha>/                  # immutable slot (commit_sha unique)
//	      manifest.yaml
//	      soul-cloud-<name>            # or soul-ssh-<name>
//
// git-egress — HIGH security risk. Git operations via go-git (pure-Go,
// no system `git` fork): hooks NOT executed by design, `ext::` transport
// does not exist, submodules not recursive by default, `file://` locked by
// scheme-allowlist ([validateGitScheme]); clone/fetch — shallow (Depth=1)
// under context timeout. Hardening invariant details — in [git.go].
// Resolved binary NOT executed and NOT marked trusted — trust given separately
// via `plugin.allow` + Sigil (S3/S4/S6), resolver only populates cache.
package plugingit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// currentLink — name of symlink to active commit_sha slot inside <ns>-<name>/.
const currentLink = "current"

// Sentinel errors for resolve of one catalog entry. ResolveCatalog maps them to
// per-entry warnings (fail-closed: broken entry skipped, Keeper does not crash).
var (
	// ErrRefNotResolved — ResolveRevision(<ref>^{commit}) found no commit (ref
	// does not exist as tag, branch, or full hash).
	ErrRefNotResolved = errors.New("plugingit: ref does not resolve to a commit")
	// ErrManifestNotFound — checkout has no manifest.yaml at root.
	ErrManifestNotFound = errors.New("plugingit: manifest.yaml not found in checkout")
	// ErrArtifactNotFound — expected built binary dist/<binary-name> missing
	// or not a regular file (F-fetch requires ALREADY-built artifact).
	ErrArtifactNotFound = errors.New("plugingit: built artifact not found in dist/")
	// ErrSourceUnavailable — git clone/fetch/checkout of source failed
	// (remote unavailable, auth, timeout).
	ErrSourceUnavailable = errors.New("plugingit: git source unavailable")
	// ErrArtifactTooLarge — binary dist/<binary-name> exceeds
	// plugins.max_artifact_size_mb. git-egress hardening (ADR-026(g)):
	// adversarial/huge artifact must not fill keeper-host cache. Fail-closed
	// — slot not created.
	ErrArtifactTooLarge = errors.New("plugingit: built artifact exceeds size limit")
	// ErrCloneTooLarge — total size of clone working tree (checkout + .git)
	// exceeds plugins.max_clone_size_mb. git-egress hardening (ADR-026(g)):
	// huge repository must not fill work_root. Fail-closed — workdir
	// cleaned, slot not created.
	ErrCloneTooLarge = errors.New("plugingit: clone tree exceeds size limit")
)

// DefaultGitTimeout — default timeout for resolver git-operation chain
// (clone/fetch → resolve → checkout). Matches config default
// [config.DefaultPluginFetchTimeout].
const DefaultGitTimeout = config.DefaultPluginFetchTimeout

// artifactSubdir — subdirectory of built artifact in plugin repository
// (F-fetch: binary already built in dist/, Keeper does not compile).
const artifactSubdir = "dist"

// ResolvedSlot — result of successful resolve of one catalog entry: where
// immutable slot was placed and how it is identified.
type ResolvedSlot struct {
	// Namespace / Name — plugin key (from checkout manifest, not catalog:
	// catalog carries only `name`, namespace taken from manifest itself).
	Namespace string
	Name      string
	// Ref — operator-asserted label from catalog (`ref:`), as-is.
	Ref string
	// CommitSHA — 40-hex commit to which ref resolved. Slot identifier
	// (immutable: one commit_sha → one slot directory).
	CommitSHA string
	// SlotDir — absolute path of immutable slot
	// `<cacheRoot>/<ns>-<name>/<commit_sha>/`.
	SlotDir string
	// BinarySHA256 — SHA-256 (hex, lowercase) of binary in slot.
	BinarySHA256 string
}

// Resolver — git resolver for plugin catalog. cacheRoot — root of slot cache;
// workRoot — root of working clones (STRICTLY outside cacheRoot, so .git and checkout
// not in cache directory read by Discover/ReadSlot).
//
// maxArtifactSize / maxCloneSize — size limits for git-egress hardening (ADR-026(g),
// bytes): ceiling of single extracted binary and total clone working tree.
// Protection of keeper-host disk from adversarial/huge repository (timeout
// bounds egress by time, these — by volume). Exceeded — fail-closed.
type Resolver struct {
	cacheRoot       string
	workRoot        string
	gitTimeout      time.Duration
	maxArtifactSize int64
	maxCloneSize    int64
	logger          *slog.Logger
}

// NewResolver constructs resolver. gitTimeout <= 0 → [DefaultGitTimeout].
// maxArtifactSize / maxCloneSize <= 0 → defaults [config.DefaultPluginMaxArtifactSizeMB]
// / [config.DefaultPluginMaxCloneSizeMB] (resolve symmetric to Resolved* config methods).
// logger nil → slog.Default(). Git operations — go-git (pure-Go, no
// system `git` fork).
func NewResolver(cacheRoot, workRoot string, gitTimeout time.Duration, maxArtifactSize, maxCloneSize int64, logger *slog.Logger) *Resolver {
	if gitTimeout <= 0 {
		gitTimeout = DefaultGitTimeout
	}
	if maxArtifactSize <= 0 {
		maxArtifactSize = int64(config.DefaultPluginMaxArtifactSizeMB) * bytesPerMiB
	}
	if maxCloneSize <= 0 {
		maxCloneSize = int64(config.DefaultPluginMaxCloneSizeMB) * bytesPerMiB
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{
		cacheRoot:       cacheRoot,
		workRoot:        workRoot,
		gitTimeout:      gitTimeout,
		maxArtifactSize: maxArtifactSize,
		maxCloneSize:    maxCloneSize,
		logger:          logger,
	}
}

// bytesPerMiB — multiplier MiB→bytes, local copy of [config.bytesPerMiB]
// (unexported in shared/config), to default limits in bytes without
// import workaround.
const bytesPerMiB = 1024 * 1024

// ResolveCatalog resolves entire catalog cloud_drivers + ssh_providers +
// soul_modules. Per-entry errors converted to warnings (fail-closed):
// broken entry skipped, Keeper does not crash. Returns (successfully
// resolved slots, warnings, fatal error). fatal — only what breaks
// resolve IN PRINCIPLE (e.g., unable to create workRoot); nil plugins →
// empty result.
func (r *Resolver) ResolveCatalog(ctx context.Context, plugins *config.KeeperPlugins) ([]ResolvedSlot, []string, error) {
	if plugins == nil {
		return nil, nil, nil
	}
	var (
		slots    []ResolvedSlot
		warnings []string
	)
	entries := make([]config.PluginCatalogEntry, 0,
		len(plugins.CloudDrivers)+len(plugins.SSHProviders)+len(plugins.SoulModules))
	entries = append(entries, plugins.CloudDrivers...)
	entries = append(entries, plugins.SSHProviders...)
	entries = append(entries, plugins.SoulModules...)

	for _, e := range entries {
		slot, err := r.ResolveEntry(ctx, e)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"plugin %q (source=%q ref=%q): %v", e.Name, e.Source, e.Ref, err))
			continue
		}
		slots = append(slots, slot)
	}
	return slots, warnings, nil
}

// ResolveEntry resolves one catalog entry to immutable commit_sha slot.
// Flow (F-fetch — no compilation):
//
//  1. validateGitScheme(source): allowlist https/ssh/scp (file:// — only under
//     env flag); disallowed scheme → ErrSourceUnavailable;
//  2. workdir := <workRoot>/<name>/ (outside cacheRoot, mode 0700);
//     go-git shallow clone (or fetch if clone exists);
//  3. commit_sha := resolveRef(<ref>^{commit}) (40-hex guaranteed by
//     plumbing.Hash type; unresolvable ref → ErrRefNotResolved);
//  4. checkout detached-HEAD to commit_sha (go-git, hooks not executed);
//  5. parse <workdir>/manifest.yaml (missing → ErrManifestNotFound) → kind →
//     BinaryName() → expected dist/<binary-name> (missing/not file → ErrArtifactNotFound);
//  6. dst := <cacheRoot>/<ns>-<name>/<commit_sha>/; if already valid → skip
//     (commit_sha immutable);
//  7. staging directory on same fs → copy(manifest+binary)+fsync → atomic rename;
//  8. atomic update symlink <cacheRoot>/<ns>-<name>/current → <commit_sha>;
//  9. binary_sha256 := sha256(<dst>/<binary-name>).
//
// `<ns>` taken from manifest AFTER checkout: before parse namespace unknown,
// so workdir named by catalog `name` (deterministic before parse), and
// move to namespace-aware slot done on step 6 (cacheRoot), where namespace
// already read.
func (r *Resolver) ResolveEntry(ctx context.Context, e config.PluginCatalogEntry) (ResolvedSlot, error) {
	ctx, cancel := context.WithTimeout(ctx, r.gitTimeout)
	defer cancel()

	if e.Source == "" {
		return ResolvedSlot{}, fmt.Errorf("%w: empty source", ErrSourceUnavailable)
	}
	if e.Ref == "" {
		return ResolvedSlot{}, fmt.Errorf("%w: empty ref", ErrRefNotResolved)
	}
	if err := validateGitScheme(e.Source); err != nil {
		return ResolvedSlot{}, err
	}

	// workdir named by catalog name (namespace not yet known before parse).
	workdir := filepath.Join(r.workRoot, sanitizeSegment(e.Name))
	commitSHA, err := r.prepareCheckout(ctx, workdir, e.Source, e.Ref)
	if err != nil {
		return ResolvedSlot{}, err
	}

	manifestPath := filepath.Join(workdir, sharedplugin.FileName)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ResolvedSlot{}, fmt.Errorf("%w: %s", ErrManifestNotFound, manifestPath)
		}
		return ResolvedSlot{}, fmt.Errorf("plugingit: read manifest %q: %w", manifestPath, err)
	}
	m, diags := sharedplugin.LoadFromBytes(manifestPath, manifestBytes)
	if err := firstManifestError(diags); err != nil {
		return ResolvedSlot{}, fmt.Errorf("plugingit: invalid manifest %q: %w", manifestPath, err)
	}
	binName := m.BinaryName()
	if binName == "" {
		return ResolvedSlot{}, fmt.Errorf("plugingit: manifest %q has no binary convention for kind=%q", manifestPath, m.Kind)
	}

	artifactPath := filepath.Join(workdir, artifactSubdir, binName)
	ast, err := os.Stat(artifactPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ResolvedSlot{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, artifactPath)
		}
		return ResolvedSlot{}, fmt.Errorf("plugingit: stat artifact %q: %w", artifactPath, err)
	}
	if !ast.Mode().IsRegular() {
		return ResolvedSlot{}, fmt.Errorf("%w: %s is not a regular file", ErrArtifactNotFound, artifactPath)
	}
	// git-egress hardening (ADR-026(g)): cut off huge binary BEFORE copy to
	// cache by os.Stat size (LimitReader in copyFileSync — defense-in-
	// depth below). Fail-closed: slot not materialized.
	if ast.Size() > r.maxArtifactSize {
		return ResolvedSlot{}, fmt.Errorf("%w: %s = %d bytes > limit %d bytes", ErrArtifactTooLarge, artifactPath, ast.Size(), r.maxArtifactSize)
	}

	slotKey := m.Namespace + "-" + m.Name
	pluginDir := filepath.Join(r.cacheRoot, slotKey)
	dst := filepath.Join(pluginDir, commitSHA)

	// commit_sha immutable: if slot already valid — skip extraction,
	// only update current and calculate digest of existing binary.
	if !r.slotValid(dst, binName) {
		if err := r.materializeSlot(pluginDir, dst, manifestPath, artifactPath, binName, r.maxArtifactSize); err != nil {
			return ResolvedSlot{}, err
		}
	}

	if err := updateCurrentSymlink(pluginDir, commitSHA); err != nil {
		return ResolvedSlot{}, err
	}

	digest, err := fileDigest(filepath.Join(dst, binName))
	if err != nil {
		return ResolvedSlot{}, err
	}

	return ResolvedSlot{
		Namespace:    m.Namespace,
		Name:         m.Name,
		Ref:          e.Ref,
		CommitSHA:    commitSHA,
		SlotDir:      dst,
		BinarySHA256: digest,
	}, nil
}

// prepareCheckout prepares working clone at workdir on ref and returns
// resolved 40-hex commit_sha. Shallow-clones (Depth=1) if clone missing,
// otherwise shallow fetch exactly ref; resolves ref to commit; then
// checkout detached-HEAD to this commit. workdir created mode 0700
// (service-user-only).
//
// transport/auth/timeout failures on clone/fetch → ErrSourceUnavailable;
// unresolvable ref → ErrRefNotResolved (from [resolveRef]).
func (r *Resolver) prepareCheckout(ctx context.Context, workdir, source, ref string) (string, error) {
	if err := os.MkdirAll(r.workRoot, 0o700); err != nil {
		return "", fmt.Errorf("plugingit: mkdir work root %q: %w", r.workRoot, err)
	}

	auth, err := authFor(source)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	repo, err := openOrClone(ctx, workdir, source, auth)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if err := fetch(ctx, repo, auth); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	commitSHA, err := resolveRef(repo, ref)
	if err != nil {
		return "", err
	}
	if err := checkout(repo, commitSHA); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	// git-egress hardening (ADR-026(g)): go-git no byte-cap on clone, so
	// measure working tree (checkout + .git) AFTER extraction but BEFORE copy
	// artifact to cache. Shallow Depth=1 already cuts history; this walk catches
	// huge working tree (junk files / inflated artifact). Exceeded —
	// fail-closed: clean workdir, slot not created.
	size, err := dirSize(workdir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if size > r.maxCloneSize {
		_ = os.RemoveAll(workdir)
		return "", fmt.Errorf("%w: %s = %d bytes > limit %d bytes", ErrCloneTooLarge, workdir, size, r.maxCloneSize)
	}
	return commitSHA, nil
}

// dirSize sums size of regular files in root subtree (du-like, excluding
// directories/symlinks). Interrupts on first walk error — partial traverse
// would make limit-check unreliable.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("plugingit: measure clone tree %q: %w", root, err)
	}
	return total, nil
}

// slotValid — true if dst already contains manifest.yaml and binary binName
// (commit_sha slot immutable; re-resolve of same commit — skip).
func (r *Resolver) slotValid(dst, binName string) bool {
	if st, err := os.Stat(filepath.Join(dst, sharedplugin.FileName)); err != nil || st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(dst, binName)); err != nil || !st.Mode().IsRegular() {
		return false
	}
	return true
}

// materializeSlot extracts manifest+binary to immutable slot dst atomically:
// build in staging directory ON SAME fs as dst (rename atomic only within
// one fs), fsync files, then os.Rename(staging, dst). artifactMax
// — byte-cap for binary copy (ADR-026(g)): copy via LimitReader, on
// exceeded staging cleaned and ErrArtifactTooLarge returned (fail-closed).
func (r *Resolver) materializeSlot(pluginDir, dst, manifestSrc, artifactSrc, binName string, artifactMax int64) error {
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return fmt.Errorf("plugingit: mkdir plugin dir %q: %w", pluginDir, err)
	}
	staging := filepath.Join(pluginDir, ".staging-"+randSuffix())
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("plugingit: mkdir staging %q: %w", staging, err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }

	// manifest no cap (admittedly small, loader validates its form).
	if err := copyFileSync(manifestSrc, filepath.Join(staging, sharedplugin.FileName), 0o644, 0); err != nil {
		cleanup()
		return err
	}
	if err := copyFileSync(artifactSrc, filepath.Join(staging, binName), 0o755, artifactMax); err != nil {
		cleanup()
		return err
	}

	if err := os.Rename(staging, dst); err != nil {
		cleanup()
		// Race of two resolvers on same commit_sha: winner already created dst —
		// not an error (slot immutable, content identical).
		if r.slotValid(dst, binName) {
			return nil
		}
		return fmt.Errorf("plugingit: atomic rename staging→slot %q: %w", dst, err)
	}
	return nil
}

// fileDigest calculates file SHA-256 streaming (plugin binaries — tens of MB).
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("plugingit: open binary for digest %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("plugingit: read binary for digest %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFileSync copies src→dst with given mode and fsync (slot must survive
// crash before current switches to it). maxBytes > 0 — byte-cap
// (ADR-026(g) git-egress hardening): copy via LimitReader(maxBytes+1) and
// exceeded returns ErrArtifactTooLarge fail-closed (dst stays in staging,
// caller cleans). maxBytes <= 0 — no limit (manifest).
func copyFileSync(src, dst string, mode os.FileMode, maxBytes int64) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("plugingit: open %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("plugingit: create %q: %w", dst, err)
	}
	var reader io.Reader = in
	if maxBytes > 0 {
		// +1 byte: if LimitReader gives exactly maxBytes+1 — source exceeds cap.
		reader = io.LimitReader(in, maxBytes+1)
	}
	written, err := io.Copy(out, reader)
	if err != nil {
		_ = out.Close()
		return fmt.Errorf("plugingit: copy %q→%q: %w", src, dst, err)
	}
	if maxBytes > 0 && written > maxBytes {
		_ = out.Close()
		return fmt.Errorf("%w: %s > limit %d bytes", ErrArtifactTooLarge, src, maxBytes)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("plugingit: fsync %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("plugingit: close %q: %w", dst, err)
	}
	// OpenFile with mode subject to umask — set mode explicitly.
	if err := os.Chmod(dst, mode); err != nil {
		return fmt.Errorf("plugingit: chmod %q: %w", dst, err)
	}
	return nil
}

// updateCurrentSymlink atomically swaps <pluginDir>/current → commitSHA:
// creates temp-symlink nearby and os.Rename-s it to current (rename of symlink
// atomic within directory). Target — relative (commitSHA), so slot
// movable together with pluginDir.
func updateCurrentSymlink(pluginDir, commitSHA string) error {
	tmp := filepath.Join(pluginDir, ".current-"+randSuffix())
	if err := os.Symlink(commitSHA, tmp); err != nil {
		return fmt.Errorf("plugingit: create temp symlink in %q: %w", pluginDir, err)
	}
	if err := os.Rename(tmp, filepath.Join(pluginDir, currentLink)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("plugingit: atomic swap current symlink in %q: %w", pluginDir, err)
	}
	return nil
}

// randSuffix — short random suffix for staging/temp names (avoid
// collisions of parallel resolvers). crypto/rand — not for crypto-strength,
// but so two processes don't pick same suffix.
func randSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sanitizeSegment protects path segment name from `/`/`..`: directory receives
// only `name` from catalog (kebab-case by manifest validation), but workdir
// built before parse — so guard against path-traversal in config value.
func sanitizeSegment(s string) string {
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	s = strings.TrimPrefix(s, ".")
	if s == "" {
		return "_"
	}
	return s
}

// firstManifestError returns first error-level diagnostic of manifest
// (warning/hint ignored). nil — no fatal errors.
func firstManifestError(diags []diag.Diagnostic) error {
	for _, d := range diags {
		if d.Level == diag.LevelError {
			return errors.New(d.Code + ": " + d.Message)
		}
	}
	return nil
}
