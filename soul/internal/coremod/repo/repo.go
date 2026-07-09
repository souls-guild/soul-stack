// Package repo реализует core-модуль `core.repo` ([ADR-015]) — управление
// пакетным репозиторием (apt/dnf/yum/apk; аналог идеи ansible
// apt_repository/yum_repository, переработанный под безопасный декларативный
// MVP).
//
// Состояния:
//   - present: репозиторий объявлен (файл-описание + GPG-ключ на месте).
//   - absent:  описание репозитория удалено (ключ не трогаем — он может
//     использоваться другими репозиториями).
//
// Backend выбирается по util.DetectPkgMgr:
//   - apt → /etc/apt/sources.list.d/<name>.list + ключ в
//     /etc/apt/keyrings/<name>.gpg, на который .list ссылается через
//     `signed-by=` (современный формат, НЕ apt-key — apt-key deprecated и
//     добавляет ключ в общий trust store без привязки к репозиторию);
//   - dnf/yum → /etc/yum.repos.d/<name>.repo (ini-формат);
//   - apk → строка в /etc/apk/repositories.
//
// Идемпотентность: целевой файл существует, его содержимое побайтово совпадает
// с желаемым и (для apt с gpg_key) ключ лежит на месте → changed=false.
//
// Запись файлов — через util.AtomicWritePreserving (preserve-by-default, как в
// пилоте core.line: повторная запись существующего файла сохраняет его
// mode/owner/group).
//
// Безопасность ([ADR-016] «безопасность на первом месте», подтверждено
// пользователем):
//   - gpg_check=false РАЗРЕШЁН (opt-out), но Apply возвращает обязательный
//     warning в output — симметрично checksum-opt-out в core.url;
//   - http:// в uri ДОПУСТИМ (легитимный кейс внутреннего зеркала, в отличие
//     от core.url, где https-only), но с обязательным warning;
//   - gpg_key критичен для supply-chain: если задан, ключ реально
//     материализуется (apt) / прописывается как gpgkey (dnf/yum) и проверяется
//     при idempotency.
//
// [ADR-015]: docs/adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список
// [ADR-016]: docs/adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack
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

// Name — каноническая верхушка адреса.
const Name = "core.repo"

// Канонические каталоги backend-ов. Вынесены в поля Module для подмены в
// unit-тестах поддельным rootfs (t.TempDir): модуль пишет абсолютные пути.
const (
	defaultAptSourcesDir  = "/etc/apt/sources.list.d"
	defaultAptKeyringsDir = "/etc/apt/keyrings"
	defaultYumReposDir    = "/etc/yum.repos.d"
	defaultApkReposFile   = "/etc/apk/repositories"
)

// Module — реализация sdk/module.SoulModule для core.repo.
//
// Runner нужен для util.DetectPkgMgr (определение backend-а). Каталоги вынесены
// в поля для тестабельности (подмена на TempDir без записи в реальную систему).
// LookupUser/LookupGroup — точки подмены preserve-логики util.AtomicWritePreserving.
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

// repoParams — разобранные параметры одного Apply-вызова.
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

// Validate НЕ делегирован целиком в util.ValidateAgainstManifest (в отличие от
// core.exec): сверх known-state + required у core.repo есть семантические
// проверки, которые manifest-DSL не выражает — validateName (имя становится
// именем файла, path-traversal недопустим) и validateURIScheme (только http/https).
// Они критичны (security: запись вне целевого каталога / нелегитимная схема).
// known-state/required дублируются с repo.yaml осознанно.
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

	// uri обязателен только для present: absent оперирует именем файла.
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

// PlanReadSafe объявляет, что core.repo.Plan — pure-read (ADR-031 Scry):
// читает target-файл и (для apt) keyring, НЕ мутирует ФС (маркер для host-а,
// default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущий target-файл бэкенда
// и сравнивает с желаемым содержимым (тот же сравнительный read, что в
// applyAptPresent/applyYumPresent/applyApkPresent), плюс keyring (apt). НЕ
// мутирует: ни MkdirAll, ни ensureFile/ensureKey.
//
// Backend выбирается через util.DetectPkgMgr (read-only вызов).
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

// readParamsFromPlan — extractor params для Plan (PlanRequest, не ApplyRequest).
// Семантика 1:1 с readParams.
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

// planBoolDefault — параллель boolParamDefault для PlanRequest.
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

// fileDrift — drift=true, если файла нет ИЛИ его содержимое != want.
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

// readParams разбирает и нормализует параметры. gpg_check и enabled — default
// true (как в дизайне): отсутствие ключа трактуется как «по умолчанию true»,
// поэтому читаем явное наличие, а не голый OptBoolParam (он даёт false при
// отсутствии).
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

// boolParamDefault возвращает значение bool-параметра или def, если ключ
// отсутствует/null. Нужен для gpg_check/enabled, у которых дефолт true (голый
// OptBoolParam дал бы false на отсутствие). Наличие явного ключа определяется
// до делегирования в OptBoolParam (тот сам не различает «нет ключа» и «false»).
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

// validateName ограничивает name безопасным набором символов: имя становится
// именем файла (sources.list.d/<name>.list и т.п.), поэтому слэши и
// path-traversal недопустимы (security: запись вне целевого каталога).
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

// validateURIScheme: http и https допустимы (http — внутреннее зеркало, по
// дизайну). Любая другая схема (file://, ftp://, пустая) — ошибка.
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

// isHTTP сообщает, что uri использует незащищённую схему (для warning).
func isHTTP(uri string) bool {
	u, err := url.Parse(uri)
	return err == nil && strings.EqualFold(u.Scheme, "http")
}
