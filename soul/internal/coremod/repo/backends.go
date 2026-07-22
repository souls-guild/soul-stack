package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// applyPresent materializes the repo description for the chosen backend.
// Idempotency: target file + (for apt) key match → changed=false.
func (m *Module) applyPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	if p.uri == "" {
		return util.SendFailed(stream, `param "uri": required for state present`)
	}
	if serr := validateURIScheme(p.uri); serr != nil {
		return util.SendFailed(stream, serr.Error())
	}

	switch mgr {
	case util.PkgMgrApt:
		return m.applyAptPresent(stream, mgr, p)
	case util.PkgMgrDnf, util.PkgMgrYum:
		return m.applyYumPresent(stream, mgr, p)
	case util.PkgMgrApk:
		return m.applyApkPresent(stream, mgr, p)
	default:
		return util.SendFailed(stream, fmt.Sprintf("core.repo: unsupported package manager %q", mgr))
	}
}

// applyAbsent removes the repo description. The GPG key is deliberately NOT
// removed: it may be shared by other repos; manual key cleanup is a separate,
// explicit operator step.
func (m *Module) applyAbsent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	switch mgr {
	case util.PkgMgrApt:
		return m.removeFile(stream, m.aptListPath(p))
	case util.PkgMgrDnf, util.PkgMgrYum:
		return m.removeFile(stream, m.yumRepoPath(p))
	case util.PkgMgrApk:
		return m.applyApkAbsent(stream, p)
	default:
		return util.SendFailed(stream, fmt.Sprintf("core.repo: unsupported package manager %q", mgr))
	}
}

// --- apt ---

// applyAptPresent writes /etc/apt/sources.list.d/<name>.list in the modern
// deb822 one-line format with signed-by= pointing at the keyring. The key
// (if set) is materialized at /etc/apt/keyrings/<name>.gpg.
func (m *Module) applyAptPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	listPath := m.aptListPath(p)
	keyPath := filepath.Join(m.AptKeyringsDir, p.name+".gpg")

	var warnings []string
	addRepoWarnings(&warnings, mgr, p)

	// gpg_key_path (variant B): reference an existing on-host key via
	// signed-by=; guard its existence but do not copy it.
	if p.gpgKeyPath != "" {
		if err := statExists("gpg_key_path", p.gpgKeyPath); err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}

	// Inline key: materialize it first — .list references keyPath via
	// signed-by=, and .list's idempotency depends on that reference existing.
	keyChanged := false
	if p.gpgKeyPath == "" && p.gpgKey != "" {
		ch, kerr := m.ensureKey(keyPath, p.gpgKey)
		if kerr != nil {
			return util.SendFailed(stream, kerr.Error())
		}
		keyChanged = ch
	}

	wantContent := aptListContent(p, keyPath)
	fileChanged, ferr := m.ensureFile(listPath, wantContent)
	if ferr != nil {
		return util.SendFailed(stream, ferr.Error())
	}

	return finalOutput(stream, fileChanged || keyChanged, map[string]any{
		"name":    p.name,
		"backend": "apt",
		"path":    listPath,
	}, warnings)
}

// aptListContent builds a single sources.list.d line. Format:
//
//	deb [signed-by=<keyPath> arch=...] <uri> <suite> <components...>
//
// signed-by/arch are present only if set (signed-by binds trust to the repo;
// arch is for multi-arch repos, [ADR-071]). enabled=false → the line is
// commented out (apt has no enabled flag in the one-line format; comment-out
// is the standard practice).
func aptListContent(p repoParams, keyPath string) []byte {
	var opts []string
	if sb := aptSignedBy(p, keyPath); sb != "" {
		opts = append(opts, "signed-by="+sb)
	}
	if len(p.arch) > 0 {
		opts = append(opts, "arch="+strings.Join(p.arch, ","))
	}
	var b strings.Builder
	if !p.enabled {
		b.WriteString("# ")
	}
	b.WriteString("deb ")
	if len(opts) > 0 {
		b.WriteString("[" + strings.Join(opts, " ") + "] ")
	}
	b.WriteString(p.uri)
	if p.suite != "" {
		b.WriteString(" " + p.suite)
	}
	if len(p.components) > 0 {
		b.WriteString(" " + strings.Join(p.components, " "))
	}
	b.WriteString("\n")
	return []byte(b.String())
}

// aptSignedBy resolves the keyring path for signed-by=: an operator-supplied
// gpg_key_path (variant B, referenced as-is, no copy) wins over the inline
// key materialized at keyPath. Empty when no key is configured.
func aptSignedBy(p repoParams, keyPath string) string {
	if p.gpgKeyPath != "" {
		return p.gpgKeyPath
	}
	if p.gpgKey != "" {
		return keyPath
	}
	return ""
}

// --- dnf / yum ---

// applyYumPresent writes /etc/yum.repos.d/<name>.repo in ini format.
func (m *Module) applyYumPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	repoPath := m.yumRepoPath(p)

	var warnings []string
	addRepoWarnings(&warnings, mgr, p)

	// gpg_key_path (variant B): gpgkey= references it; guard existence.
	if p.gpgKeyPath != "" {
		if err := statExists("gpg_key_path", p.gpgKeyPath); err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}

	wantContent := yumRepoContent(p)
	changed, ferr := m.ensureFile(repoPath, wantContent)
	if ferr != nil {
		return util.SendFailed(stream, ferr.Error())
	}

	return finalOutput(stream, changed, map[string]any{
		"name":    p.name,
		"backend": "yum",
		"path":    repoPath,
	}, warnings)
}

// yumRepoContent builds the repo's ini section. gpgcheck/enabled are 0/1.
// gpgkey is written only if gpg_key is set (for yum this is a URL or
// file:// path; we write the value as-is — the operator supplies the key URL).
func yumRepoContent(p repoParams) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", p.name)
	fmt.Fprintf(&b, "name=%s\n", p.name)
	fmt.Fprintf(&b, "baseurl=%s\n", p.uri)
	fmt.Fprintf(&b, "enabled=%s\n", boolDigit(p.enabled))
	fmt.Fprintf(&b, "gpgcheck=%s\n", boolDigit(p.gpgCheck))
	// gpgkey accepts a local path or URL as-is; gpg_key_path (variant B) takes
	// precedence over an inline/URL gpg_key.
	if gpgkey := p.gpgKeyPath; gpgkey != "" {
		fmt.Fprintf(&b, "gpgkey=%s\n", gpgkey)
	} else if p.gpgKey != "" {
		fmt.Fprintf(&b, "gpgkey=%s\n", p.gpgKey)
	}
	return []byte(b.String())
}

// --- apk ---

// applyApkPresent adds/updates a line in /etc/apk/repositories.
// apk stores repos one per line; idempotency means an exact line match.
// Line format: `<uri>` (apk doesn't use suite/components in the URL — the
// operator puts the full URL in uri). enabled=false → line is commented out.
func (m *Module) applyApkPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, p repoParams) error {
	if perr := apkRejectUnsupported(p); perr != nil {
		return util.SendFailed(stream, perr.Error())
	}
	wantLine := apkLine(p)
	changed, err := m.upsertApkLine(wantLine)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	var warnings []string
	// apk gpg_check can't be expressed at the repo-line level (keys live in
	// /etc/apk/keys/); we still warn on gpg_check=false for symmetry.
	addRepoWarnings(&warnings, mgr, p)

	return finalOutput(stream, changed, map[string]any{
		"name":    p.name,
		"backend": "apk",
		"path":    m.ApkReposFile,
	}, warnings)
}

// applyApkAbsent removes a repo line. apk doesn't store the repo name in the
// file, so identity is the uri: absent for apk REQUIRES uri (unlike
// apt/yum, which have a <name> file). Without uri, removal would be a guess
// and risk deleting the wrong line.
func (m *Module) applyApkAbsent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], p repoParams) error {
	if p.uri == "" {
		return util.SendFailed(stream, `param "uri": required for apk repo absent (apk has no per-repo file, removal matches by uri)`)
	}
	changed, err := m.removeApkLine(p.uri)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return finalOutput(stream, changed, map[string]any{
		"name":    p.name,
		"backend": "apk",
		"path":    m.ApkReposFile,
	}, nil)
}

func apkLine(p repoParams) string {
	if !p.enabled {
		return "# " + p.uri
	}
	return p.uri
}

// apkRejectUnsupported rejects apt/dnf-only params for apk: apk stores repos as
// bare lines in a single /etc/apk/repositories file (no per-repo signed-by, no
// per-repo description file), so gpg_key_path and dest are meaningless here.
func apkRejectUnsupported(p repoParams) error {
	if p.gpgKeyPath != "" {
		return fmt.Errorf(`param "gpg_key_path": not supported for apk (repo lines have no signed-by; place keys in /etc/apk/keys/)`)
	}
	if p.dest != "" {
		return fmt.Errorf(`param "dest": not supported for apk (single repositories file, no per-repo description)`)
	}
	return nil
}

// upsertApkLine idempotently inserts/updates a repo line. apk doesn't store
// the repo name in the file, so matching is by uri (accounting for a
// possible `# ` prefix). If the line already exactly equals want — no-op.
func (m *Module) upsertApkLine(want string) (bool, error) {
	lines, err := readLines(m.ApkReposFile)
	if err != nil {
		return false, err
	}
	wantBare := strings.TrimSpace(strings.TrimPrefix(want, "# "))
	for i, l := range lines {
		bare := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "# "))
		if bare == wantBare {
			if l == want {
				return false, nil
			}
			lines[i] = want
			return true, m.writeLines(m.ApkReposFile, lines)
		}
	}
	lines = append(lines, want)
	return true, m.writeLines(m.ApkReposFile, lines)
}

// removeApkLine removes the line with the given uri (accounting for a
// possible `# ` prefix on a commented-out line). Returns changed.
func (m *Module) removeApkLine(uri string) (bool, error) {
	lines, err := readLines(m.ApkReposFile)
	if err != nil {
		return false, err
	}
	out := make([]string, 0, len(lines))
	changed := false
	for _, l := range lines {
		bare := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "# "))
		if bare == uri {
			changed = true
			continue
		}
		out = append(out, l)
	}
	if !changed {
		return false, nil
	}
	return true, m.writeLines(m.ApkReposFile, out)
}

// --- shared file operations ---

// ensureFile writes content to path if the file is missing or its content
// differs. Writes are preserve-by-default (util.AtomicWritePreserving):
// an existing file's perms/owner are kept. Returns changed.
func (m *Module) ensureFile(path string, content []byte) (bool, error) {
	cur, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if existed && string(cur) == string(content) {
		return false, nil
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return false, fmt.Errorf("mkdir %s: %v", filepath.Dir(path), mkErr)
	}
	if werr := util.AtomicWritePreserving(path, content, "", "", "", m.LookupUser, m.LookupGroup); werr != nil {
		return false, werr
	}
	return true, nil
}

// ensureKey materializes a GPG key at keyPath if it's missing or its content
// differs. gpgKey is treated as inline key content (PEM/ASCII-armored or a
// binary keyring — written as-is). The key is critical for supply-chain
// integrity.
//
// Note: gpgKey-as-URL fetching is deliberately NOT done here ([ADR-071] §(g),
// variant B) — network/SSRF stays in core.url.fetched (network_outbound +
// SSRF-guard + checksum), core.repo stays pure-FS. The key is always passed
// inline (CEL can substitute content via ${ file(...) } or vault); see
// docs/module/core/repo/README.md for the core.url.fetched → gpg_key pattern.
func (m *Module) ensureKey(keyPath, gpgKey string) (bool, error) {
	cur, existed, err := readFile(keyPath)
	if err != nil {
		return false, err
	}
	want := []byte(gpgKey)
	if existed && string(cur) == string(want) {
		return false, nil
	}
	if mkErr := os.MkdirAll(filepath.Dir(keyPath), 0o755); mkErr != nil {
		return false, fmt.Errorf("mkdir %s: %v", filepath.Dir(keyPath), mkErr)
	}
	// Key is read by apt — mode 0644 (world-readable public key).
	if werr := util.AtomicWrite(keyPath, want, 0o644); werr != nil {
		return false, werr
	}
	return true, nil
}

// aptListPath / yumRepoPath resolve the description-file path: the
// operator-supplied dest overrides the backend default. Shared by Plan and Apply.
func (m *Module) aptListPath(p repoParams) string {
	if p.dest != "" {
		return p.dest
	}
	return filepath.Join(m.AptSourcesDir, p.name+".list")
}

func (m *Module) yumRepoPath(p repoParams) string {
	if p.dest != "" {
		return p.dest
	}
	return filepath.Join(m.YumReposDir, p.name+".repo")
}

// statExists is the variant-B guard for gpg_key_path: the module only
// references the key (signed-by= / gpgkey=), it does not deliver it, so a
// missing path must fail loudly rather than silently write a repo whose
// verification is broken. Reads metadata only (never the key content).
func statExists(field, path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s %q does not exist — the key must be delivered to the host first (e.g. core.url.fetched / core.file)", field, path)
		}
		return fmt.Errorf("stat %s %q: %v", field, path, err)
	}
	return nil
}

func (m *Module) removeFile(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], path string) error {
	_, existed, err := readFile(path)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !existed {
		return finalOutput(stream, false, map[string]any{"path": path}, nil)
	}
	if rerr := os.Remove(path); rerr != nil {
		return util.SendFailed(stream, fmt.Sprintf("remove %s: %v", path, rerr))
	}
	return finalOutput(stream, true, map[string]any{"path": path}, nil)
}

// addRepoWarnings adds mandatory opt-out warnings (gpg_check=false, http uri)
// — symmetric with the checksum opt-out in core.url. The warning lands in
// output (core.line pattern) instead of being lost.
func addRepoWarnings(warnings *[]string, mgr util.PkgMgr, p repoParams) {
	if !p.gpgCheck {
		*warnings = append(*warnings, fmt.Sprintf("repo %q: gpg_check disabled — packages will NOT be cryptographically verified (supply-chain risk)", p.name))
	}
	if p.gpgCheck && p.gpgKey == "" {
		*warnings = append(*warnings, fmt.Sprintf("repo %q: gpg_check enabled but no gpg_key set — %s", p.name, gpgNoKeyDetail(mgr)))
	}
	if isHTTP(p.uri) {
		*warnings = append(*warnings, fmt.Sprintf("repo %q: uri uses plain http:// — traffic is unencrypted (use https unless this is a trusted internal mirror)", p.name))
	}
}

// gpgNoKeyDetail returns the backend-specific continuation of the
// "gpg_check enabled but no gpg_key set" warning. dnf/yum strictly require
// gpgkey= when gpgcheck=1 (otherwise install fails); apt and apk fall back
// to their own trust stores (/etc/apt/keyrings + global keyring, /etc/apk/keys).
func gpgNoKeyDetail(mgr util.PkgMgr) string {
	switch mgr {
	case util.PkgMgrDnf, util.PkgMgrYum:
		return "gpgcheck=1 without gpgkey will fail package install on the host"
	case util.PkgMgrApk:
		return "signature verification relies on keys in /etc/apk/keys"
	default: // apt and others
		return "signature verification relies on the system/global trust store"
	}
}

func boolDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func readFile(path string) (content []byte, existed bool, err error) {
	b, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		return b, true, nil
	case errors.Is(rerr, fs.ErrNotExist):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("read %s: %v", path, rerr)
	}
}

func readLines(path string) ([]string, error) {
	data, _, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// writeLines writes /etc/apk/repositories line by line. This is an in-place
// edit of an existing file, so writes are preserve-by-default
// (util.AtomicWritePreserving): the existing file's perms/owner are kept
// (symmetric with ensureFile for apt/yum and fstab in core.mount).
func (m *Module) writeLines(path string, lines []string) error {
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return fmt.Errorf("mkdir %s: %v", filepath.Dir(path), mkErr)
	}
	content := strings.Join(lines, "\n") + "\n"
	if werr := util.AtomicWritePreserving(path, []byte(content), "", "", "", m.LookupUser, m.LookupGroup); werr != nil {
		return werr
	}
	return nil
}

// finalOutput builds the final ApplyEvent with changed and (if any) warnings.
func finalOutput(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, output map[string]any, warnings []string) error {
	output["changed"] = changed
	if len(warnings) > 0 {
		ws := make([]any, len(warnings))
		for i, w := range warnings {
			ws[i] = w
		}
		output["warnings"] = ws
	}
	return util.SendFinal(stream, changed, output)
}
