// Package repo implements the `core.repo` core module ([ADR-015]) — managing
// a package repository (apt/dnf/yum/apk; inspired by Ansible's
// apt_repository/yum_repository, reworked for a safe declarative MVP).
//
// States:
//   - present: repository declared (description file + GPG key in place).
//   - absent:  repository description removed (key is left alone — it may
//     be shared with other repositories).
//
// Backend selected via util.DetectPkgMgr:
//   - apt → /etc/apt/sources.list.d/<name>.list + key in
//     /etc/apt/keyrings/<name>.gpg, referenced by the .list via `signed-by=`
//     (modern form, NOT apt-key — deprecated, adds the key to the shared
//     trust store with no per-repository scoping);
//   - dnf/yum → /etc/yum.repos.d/<name>.repo (ini format);
//   - apk → a line in /etc/apk/repositories.
//
// Idempotency: target file exists, its content byte-matches the desired
// one, and (for apt with gpg_key) the key is in place → changed=false.
//
// Files are written via util.AtomicWritePreserving (preserve-by-default, as
// in the core.line pilot: rewriting an existing file keeps its mode/owner/group).
//
// Security ([ADR-016] "security first", confirmed with the user):
//   - gpg_check=false is ALLOWED (opt-out), but Apply returns a mandatory
//     warning in output — symmetric with the checksum opt-out in core.url;
//   - http:// in uri is ALLOWED (legitimate internal-mirror case, unlike
//     core.url which is https-only), but with a mandatory warning;
//   - gpg_key is supply-chain critical: when set, the key is actually
//     materialized (apt) / written as gpgkey (dnf/yum) and checked for
//     idempotency.
//
// [ADR-015]: docs/adr/0015-core-modules-mvp.md
// [ADR-016]: docs/adr/0016-parity-license.md
package repo

import (
	"context"
	"fmt"
	"net/url"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name is the canonical address prefix.
const Name = "core.repo"

// Canonical backend directories. Exposed as Module fields so unit tests can
// swap in a fake rootfs (t.TempDir) — the module writes absolute paths.
const (
	defaultAptSourcesDir  = "/etc/apt/sources.list.d"
	defaultAptKeyringsDir = "/etc/apt/keyrings"
	defaultYumReposDir    = "/etc/yum.repos.d"
	defaultApkReposFile   = "/etc/apk/repositories"
)

// Module implements sdk/module.SoulModule for core.repo.
//
// Runner is used by util.DetectPkgMgr (backend detection). Directories are
// fields for testability (swap to TempDir, no writes to the real filesystem).
// LookupUser/LookupGroup are injection points for util.AtomicWritePreserving's
// preserve logic.
type Module struct {
	Runner util.Runner

	AptSourcesDir  string
	AptKeyringsDir string
	YumReposDir    string
	ApkReposFile   string

	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		Runner:         util.OSRunner{},
		AptSourcesDir:  defaultAptSourcesDir,
		AptKeyringsDir: defaultAptKeyringsDir,
		YumReposDir:    defaultYumReposDir,
		ApkReposFile:   defaultApkReposFile,
		LookupUser:     user.Lookup,
		LookupGroup:    user.LookupGroup,
	}
}

// repoParams holds the parsed params of one Apply call.
type repoParams struct {
	name       string
	uri        string
	gpgKey     string
	gpgCheck   bool
	components []string
	suite      string
	arch       []string
	enabled    bool
}

// Validate is not fully delegated to util.ValidateAgainstManifest (unlike
// core.exec): core.repo also runs semantic checks the manifest DSL can't
// express — validateName (name becomes a filename, no path traversal) and
// validateURIScheme (http/https only). Both are security-critical (writes
// outside the target dir / illegitimate scheme). known-state/required
// duplicate repo.yaml deliberately.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case "present", "absent":
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want present|absent)", req.State))
	}

	name, nerr := util.StringParam(req.Params, "name")
	if nerr != nil {
		errs = append(errs, nerr.Error())
	} else if verr := validateName(name); verr != nil {
		errs = append(errs, verr.Error())
	}

	// uri is required only for present: absent operates on the filename alone.
	if req.State == "present" {
		uri, uerr := util.StringParam(req.Params, "uri")
		if uerr != nil {
			errs = append(errs, uerr.Error())
		} else if serr := validateURIScheme(uri); serr != nil {
			errs = append(errs, serr.Error())
		}
	}

	if _, err := util.OptStringParam(req.Params, "gpg_key"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptBoolParam(req.Params, "gpg_check"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptStringParam(req.Params, "suite"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptStringSliceParam(req.Params, "components"); err != nil {
		errs = append(errs, err.Error())
	}
	if arch, err := util.OptStringSliceParam(req.Params, "arch"); err != nil {
		errs = append(errs, err.Error())
	} else if verr := validateArch(arch); verr != nil {
		errs = append(errs, verr.Error())
	}
	if _, err := util.OptBoolParam(req.Params, "enabled"); err != nil {
		errs = append(errs, err.Error())
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares core.repo.Plan as pure-read (ADR-031 Scry): reads the
// target file and (for apt) the keyring, never mutates the filesystem.
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): reads the backend's current
// target file and compares against desired content (the same comparison
// applyAptPresent/applyYumPresent/applyApkPresent do), plus the keyring for
// apt. Never mutates: no MkdirAll, no ensureFile/ensureKey.
//
// Backend is selected via util.DetectPkgMgr (read-only call).
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	p, err := m.readParamsFromPlan(req)
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	mgr := util.DetectPkgMgr(ctx, m.Runner)
	if mgr == util.PkgMgrUnknown {
		return util.PlanFailed("core.repo: no supported package manager detected (apt/dnf/yum/apk)")
	}
	switch req.State {
	case "present":
		if p.uri == "" {
			return util.PlanFailed(`param "uri": required for state present`)
		}
		if verr := validateURIScheme(p.uri); verr != nil {
			return util.PlanFailed(verr.Error())
		}
		return m.planPresent(stream, mgr, p)
	case "absent":
		return m.planAbsent(stream, mgr, p)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// readParamsFromPlan extracts params for Plan (PlanRequest, not ApplyRequest).
// Semantics match readParams 1:1.
func (m *Module) readParamsFromPlan(req *pluginv1.PlanRequest) (repoParams, error) {
	var p repoParams
	var err error
	if p.name, err = util.StringParam(req.Params, "name"); err != nil {
		return p, err
	}
	if verr := validateName(p.name); verr != nil {
		return p, verr
	}
	if p.uri, err = util.OptStringParam(req.Params, "uri"); err != nil {
		return p, err
	}
	if p.gpgKey, err = util.OptStringParam(req.Params, "gpg_key"); err != nil {
		return p, err
	}
	if p.suite, err = util.OptStringParam(req.Params, "suite"); err != nil {
		return p, err
	}
	if p.components, err = util.OptStringSliceParam(req.Params, "components"); err != nil {
		return p, err
	}
	if p.arch, err = util.OptStringSliceParam(req.Params, "arch"); err != nil {
		return p, err
	}
	if verr := validateArch(p.arch); verr != nil {
		return p, verr
	}
	p.gpgCheck, err = planBoolDefault(req, "gpg_check", true)
	if err != nil {
		return p, err
	}
	p.enabled, err = planBoolDefault(req, "enabled", true)
	if err != nil {
		return p, err
	}
	return p, nil
}

// planBoolDefault mirrors boolParamDefault for PlanRequest.
func planBoolDefault(req *pluginv1.PlanRequest, key string, def bool) (bool, error) {
	if req.Params == nil || req.Params.Fields == nil {
		return def, nil
	}
	v, ok := req.Params.Fields[key]
	if !ok || v == nil {
		return def, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return def, nil
	}
	return util.OptBoolParam(req.Params, key)
}

func (m *Module) planPresent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], mgr util.PkgMgr, p repoParams) error {
	switch mgr {
	case util.PkgMgrApt:
		listPath := filepath.Join(m.AptSourcesDir, p.name+".list")
		keyPath := filepath.Join(m.AptKeyringsDir, p.name+".gpg")
		want := aptListContent(p, keyPath)
		listDrift, err := fileDrift(listPath, want)
		if err != nil {
			return util.PlanFailed(err.Error())
		}
		if listDrift {
			return util.SendPlanFinal(stream, true)
		}
		if p.gpgKey != "" {
			keyDrift, err := fileDrift(keyPath, []byte(p.gpgKey))
			if err != nil {
				return util.PlanFailed(err.Error())
			}
			if keyDrift {
				return util.SendPlanFinal(stream, true)
			}
		}
		return util.SendPlanFinal(stream, false)
	case util.PkgMgrDnf, util.PkgMgrYum:
		repoPath := filepath.Join(m.YumReposDir, p.name+".repo")
		want := yumRepoContent(p)
		drift, err := fileDrift(repoPath, want)
		if err != nil {
			return util.PlanFailed(err.Error())
		}
		return util.SendPlanFinal(stream, drift)
	case util.PkgMgrApk:
		wantLine := apkLine(p)
		lines, err := readLines(m.ApkReposFile)
		if err != nil {
			return util.PlanFailed(err.Error())
		}
		wantBare := strings.TrimSpace(strings.TrimPrefix(wantLine, "# "))
		for _, l := range lines {
			bare := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "# "))
			if bare == wantBare {
				return util.SendPlanFinal(stream, l != wantLine)
			}
		}
		return util.SendPlanFinal(stream, true)
	default:
		return util.PlanFailed(fmt.Sprintf("core.repo: unsupported package manager %q", mgr))
	}
}

func (m *Module) planAbsent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], mgr util.PkgMgr, p repoParams) error {
	switch mgr {
	case util.PkgMgrApt:
		_, existed, err := readFile(filepath.Join(m.AptSourcesDir, p.name+".list"))
		if err != nil {
			return util.PlanFailed(err.Error())
		}
		return util.SendPlanFinal(stream, existed)
	case util.PkgMgrDnf, util.PkgMgrYum:
		_, existed, err := readFile(filepath.Join(m.YumReposDir, p.name+".repo"))
		if err != nil {
			return util.PlanFailed(err.Error())
		}
		return util.SendPlanFinal(stream, existed)
	case util.PkgMgrApk:
		if p.uri == "" {
			return util.PlanFailed(`param "uri": required for apk repo absent (apk has no per-repo file, removal matches by uri)`)
		}
		lines, err := readLines(m.ApkReposFile)
		if err != nil {
			return util.PlanFailed(err.Error())
		}
		for _, l := range lines {
			bare := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "# "))
			if bare == p.uri {
				return util.SendPlanFinal(stream, true)
			}
		}
		return util.SendPlanFinal(stream, false)
	default:
		return util.PlanFailed(fmt.Sprintf("core.repo: unsupported package manager %q", mgr))
	}
}

// fileDrift reports drift=true if the file is missing OR its content != want.
func fileDrift(path string, want []byte) (bool, error) {
	cur, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	if !existed {
		return true, nil
	}
	return string(cur) != string(want), nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	p, err := m.readParams(req)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	mgr := util.DetectPkgMgr(ctx, m.Runner)
	if mgr == util.PkgMgrUnknown {
		return util.SendFailed(stream, "core.repo: no supported package manager detected (apt/dnf/yum/apk)")
	}

	switch req.State {
	case "present":
		return m.applyPresent(stream, mgr, p)
	case "absent":
		return m.applyAbsent(stream, mgr, p)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

// readParams parses and normalizes params. gpg_check and enabled default to
// true by design: a missing key means "true", so we check explicit presence
// rather than plain OptBoolParam (which defaults to false when absent).
func (m *Module) readParams(req *pluginv1.ApplyRequest) (repoParams, error) {
	var p repoParams
	var err error

	if p.name, err = util.StringParam(req.Params, "name"); err != nil {
		return p, err
	}
	if verr := validateName(p.name); verr != nil {
		return p, verr
	}
	if p.uri, err = util.OptStringParam(req.Params, "uri"); err != nil {
		return p, err
	}
	if p.gpgKey, err = util.OptStringParam(req.Params, "gpg_key"); err != nil {
		return p, err
	}
	if p.suite, err = util.OptStringParam(req.Params, "suite"); err != nil {
		return p, err
	}
	if p.components, err = util.OptStringSliceParam(req.Params, "components"); err != nil {
		return p, err
	}
	if p.arch, err = util.OptStringSliceParam(req.Params, "arch"); err != nil {
		return p, err
	}
	if verr := validateArch(p.arch); verr != nil {
		return p, verr
	}

	p.gpgCheck, err = boolParamDefault(req, "gpg_check", true)
	if err != nil {
		return p, err
	}
	p.enabled, err = boolParamDefault(req, "enabled", true)
	if err != nil {
		return p, err
	}
	return p, nil
}

// boolParamDefault returns the bool param's value, or def if the key is
// absent/null. Needed for gpg_check/enabled, whose default is true (plain
// OptBoolParam would give false when absent). Explicit presence is checked
// before delegating to OptBoolParam (which can't distinguish "absent" from
// "false" on its own).
func boolParamDefault(req *pluginv1.ApplyRequest, key string, def bool) (bool, error) {
	if req.Params == nil || req.Params.Fields == nil {
		return def, nil
	}
	v, ok := req.Params.Fields[key]
	if !ok || v == nil {
		return def, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return def, nil
	}
	return util.OptBoolParam(req.Params, key)
}

// validateName restricts name to a safe character set: it becomes a filename
// (sources.list.d/<name>.list etc.), so slashes and path traversal are
// disallowed (security: no writes outside the target directory).
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("param %q: must not be empty", "name")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("param %q: must not contain path separators or %q, got %q", "name", "..", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("param %q: only [A-Za-z0-9._-] allowed, got %q", "name", name)
		}
	}
	return nil
}

// validateArch санитизирует токены архитектур: значение попадает внутрь
// apt-опций `deb [... arch=<v>]`, поэтому пробел/скобка/`=` сломали бы синтаксис
// опций (инъекция). apt-архитектуры — строчные alnum (amd64/arm64/i386/all/…).
func validateArch(arch []string) error {
	for _, a := range arch {
		if a == "" {
			return fmt.Errorf("param %q: architecture must not be empty", "arch")
		}
		for _, r := range a {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				return fmt.Errorf("param %q: only [a-z0-9] allowed per architecture, got %q", "arch", a)
			}
		}
	}
	return nil
}

// validateURIScheme allows http and https (http is for internal mirrors, by
// design). Any other scheme (file://, ftp://, empty) is an error.
func validateURIScheme(uri string) error {
	u, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("param %q: invalid url %q", "uri", uri)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("param %q: only http:// or https:// allowed, got %q", "uri", uri)
	}
}

// isHTTP reports whether uri uses the unencrypted scheme (for the warning).
func isHTTP(uri string) bool {
	u, err := url.Parse(uri)
	return err == nil && strings.EqualFold(u.Scheme, "http")
}
