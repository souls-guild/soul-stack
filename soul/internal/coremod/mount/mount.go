// Package mount реализует core-модуль `core.mount` ([ADR-015]).
//
// Состояния:
//   - present:   запись в /etc/fstab + смонтировано.
//   - absent:    размонтировано + удалено из /etc/fstab.
//   - mounted:   только смонтировано «как есть» (без правки fstab) — runtime-mount.
//   - unmounted: только размонтировано (запись в fstab остаётся, не autoremove).
//
// Идемпотентность: парсим /etc/fstab, ищем запись по mount-point. Если есть и
// совпадает source/fstype/opts — fstab не трогаем. Текущий mount-статус через
// `findmnt --target <path>` (util-linux/busybox).
//
// Запись fstab — preserve-by-default (util.AtomicWritePreserving, паттерн пилота
// core.line, [ADR-015]): fstab правится in-place, его mode/владелец сохраняются,
// модуль не сбрасывает их в 0644/процесс. owner/group fstab модуль не принимает
// параметрами — fstab всегда сохраняет текущие.
package mount

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.mount"

// FstabPath — путь к каноничному fstab; подменяется в unit-тестах.
const FstabPath = "/etc/fstab"

// Module — реализация sdk/module.SoulModule для core.mount.
//
// LookupUser / LookupGroup вынесены в поля для тестабельности (симметрично
// core.line / core.repo); передаются в util.AtomicWritePreserving. Поскольку
// fstab не принимает owner/group параметрами, override-ветка lookup-функций не
// задействуется — preserve восстанавливает владельца напрямую по uid/gid.
type Module struct {
	Runner      util.Runner
	FstabPath   string
	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		Runner:      util.OSRunner{},
		FstabPath:   FstabPath,
		LookupUser:  user.Lookup,
		LookupGroup: user.LookupGroup,
	}
}

// Validate — known-state + per-state required-params (path; present/mounted →
// source/fstype) делегированы в shared/coremanifest/mount.yaml (единый источник
// с soul-lint). Cross-field-инвариантов сверх per-state required нет.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe объявляет, что core.mount.Plan — pure-read (ADR-031 Scry):
// читает findmnt и fstab, НЕ мутирует хост (маркер для host-а, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущий mount-статус
// (`findmnt --target`) + fstab-строку (тот же read, что в начале Apply) и шлёт
// PlanEvent.changed — «Apply изменил бы хост?». НЕ мутирует: ни mount/umount, ни
// запись fstab.
//
// `findmnt --target <path>` — read-only вызов (только чтение /proc/self/mountinfo),
// он же используется в Apply для idempotency. fstab читается через readFstabLines.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	switch req.State {
	case "present":
		return m.planPresent(ctx, stream, req, path)
	case "absent":
		return m.planAbsent(ctx, stream, path)
	case "mounted":
		return util.SendPlanFinal(stream, !m.isMounted(ctx, path))
	case "unmounted":
		return util.SendPlanFinal(stream, m.isMounted(ctx, path))
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// planPresent — pure-read drift для state present: drift = fstab-запись
// отсутствует/отличается ИЛИ path не смонтирован. Тот же сравнительный read,
// что в applyPresent (upsertFstab + isMounted), без записи и mount.
func (m *Module) planPresent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	source, _ := util.StringParam(req.Params, "source")
	fstype, _ := util.StringParam(req.Params, "fstype")
	opts, err := util.OptStringParam(req.Params, "opts")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if opts == "" {
		opts = "defaults"
	}
	want := fstabEntry{source: source, target: path, fstype: fstype, opts: opts, dump: "0", pass: "0"}
	wantLine := want.String()

	lines, rerr := readFstabLines(m.FstabPath)
	if rerr != nil {
		return util.PlanFailed(rerr.Error())
	}
	fstabMatch := false
	for _, line := range lines {
		parsed, ok := parseFstabLine(line)
		if !ok {
			continue
		}
		if parsed.target == want.target && line == wantLine {
			fstabMatch = true
			break
		}
	}
	if !fstabMatch {
		return util.SendPlanFinal(stream, true)
	}
	if !m.isMounted(ctx, path) {
		return util.SendPlanFinal(stream, true)
	}
	return util.SendPlanFinal(stream, false)
}

// planAbsent — pure-read drift для state absent: drift = path смонтирован
// ИЛИ fstab содержит запись с этим target.
func (m *Module) planAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.PlanEvent], path string) error {
	if m.isMounted(ctx, path) {
		return util.SendPlanFinal(stream, true)
	}
	lines, rerr := readFstabLines(m.FstabPath)
	if rerr != nil {
		return util.PlanFailed(rerr.Error())
	}
	for _, line := range lines {
		parsed, ok := parseFstabLine(line)
		if ok && parsed.target == path {
			return util.SendPlanFinal(stream, true)
		}
	}
	return util.SendPlanFinal(stream, false)
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	switch req.State {
	case "present":
		return m.applyPresent(ctx, stream, req, path)
	case "absent":
		return m.applyAbsent(ctx, stream, path)
	case "mounted":
		return m.applyMounted(ctx, stream, req, path)
	case "unmounted":
		return m.applyUnmounted(ctx, stream, path)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyPresent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	source, _ := util.StringParam(req.Params, "source")
	fstype, _ := util.StringParam(req.Params, "fstype")
	opts, err := util.OptStringParam(req.Params, "opts")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if opts == "" {
		opts = "defaults"
	}

	wantEntry := fstabEntry{source: source, target: path, fstype: fstype, opts: opts, dump: "0", pass: "0"}
	fstabChanged, err := m.upsertFstab(wantEntry)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	mountChanged, err := m.ensureMounted(ctx, path, source, fstype, opts)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, fstabChanged || mountChanged, map[string]any{
		"path":     path,
		"source":   source,
		"fstype":   fstype,
		"mounted":  true,
		"in_fstab": true,
	})
}

func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], path string) error {
	unmountChanged, err := m.ensureUnmounted(ctx, path)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	fstabChanged, err := m.removeFstab(path)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, fstabChanged || unmountChanged, map[string]any{
		"path":     path,
		"mounted":  false,
		"in_fstab": false,
	})
}

func (m *Module) applyMounted(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	source, _ := util.StringParam(req.Params, "source")
	fstype, _ := util.StringParam(req.Params, "fstype")
	opts, err := util.OptStringParam(req.Params, "opts")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if opts == "" {
		opts = "defaults"
	}
	changed, err := m.ensureMounted(ctx, path, source, fstype, opts)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, changed, map[string]any{
		"path":    path,
		"mounted": true,
	})
}

func (m *Module) applyUnmounted(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], path string) error {
	changed, err := m.ensureUnmounted(ctx, path)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, changed, map[string]any{
		"path":    path,
		"mounted": false,
	})
}

// ensureMounted: если findmnt видит mount на path — no-op; иначе вызываем
// `mount -t <fstype> -o <opts> <source> <path>`. mount-point создаём при
// необходимости (стандартное поведение).
func (m *Module) ensureMounted(ctx context.Context, path, source, fstype, opts string) (bool, error) {
	if m.isMounted(ctx, path) {
		return false, nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %v", path, err)
	}
	// `--` отделяет позиционные source/path от опций (security review L1):
	// source/path, начинающиеся с `-`, иначе распарсятся mount как опции.
	args := []string{"-t", fstype, "-o", opts, "--", source, path}
	r := m.Runner.Run(ctx, "mount", args...)
	if r.Err != nil {
		return false, fmt.Errorf("mount: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return false, fmt.Errorf("mount %s: exit %d: %s", path, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return true, nil
}

func (m *Module) ensureUnmounted(ctx context.Context, path string) (bool, error) {
	if !m.isMounted(ctx, path) {
		return false, nil
	}
	r := m.Runner.Run(ctx, "umount", "--", path)
	if r.Err != nil {
		return false, fmt.Errorf("umount: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return false, fmt.Errorf("umount %s: exit %d: %s", path, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return true, nil
}

func (m *Module) isMounted(ctx context.Context, path string) bool {
	r := m.Runner.Run(ctx, "findmnt", "--target", path)
	return r.Err == nil && r.ExitCode == 0
}

// upsertFstab — читает FstabPath, ищет строку с тем же target. Если совпадает
// полностью — fstab не трогается. Если отличается — заменяет. Если нет —
// добавляет в конец. Возвращает changed.
func (m *Module) upsertFstab(want fstabEntry) (bool, error) {
	lines, err := readFstabLines(m.FstabPath)
	if err != nil {
		return false, err
	}
	wantLine := want.String()
	for i, line := range lines {
		parsed, ok := parseFstabLine(line)
		if !ok {
			continue
		}
		if parsed.target == want.target {
			if line == wantLine {
				return false, nil
			}
			lines[i] = wantLine
			return true, m.writeFstab(lines)
		}
	}
	lines = append(lines, wantLine)
	return true, m.writeFstab(lines)
}

func (m *Module) removeFstab(target string) (bool, error) {
	lines, err := readFstabLines(m.FstabPath)
	if err != nil {
		return false, err
	}
	out := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		parsed, ok := parseFstabLine(line)
		if ok && parsed.target == target {
			changed = true
			continue
		}
		out = append(out, line)
	}
	if !changed {
		return false, nil
	}
	return true, m.writeFstab(out)
}

type fstabEntry struct {
	source, target, fstype, opts, dump, pass string
}

func (e fstabEntry) String() string {
	dump, pass := e.dump, e.pass
	if dump == "" {
		dump = "0"
	}
	if pass == "" {
		pass = "0"
	}
	return fmt.Sprintf("%s %s %s %s %s %s", e.source, e.target, e.fstype, e.opts, dump, pass)
}

// parseFstabLine — fstab формат: SOURCE TARGET FSTYPE OPTS DUMP PASS.
// Возвращает (entry, true) для data-строк, (zero, false) для пустых/комментариев.
func parseFstabLine(line string) (fstabEntry, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return fstabEntry{}, false
	}
	f := strings.Fields(trimmed)
	if len(f) < 4 {
		return fstabEntry{}, false
	}
	e := fstabEntry{source: f[0], target: f[1], fstype: f[2], opts: f[3]}
	if len(f) >= 5 {
		e.dump = f[4]
	}
	if len(f) >= 6 {
		e.pass = f[5]
	}
	return e, true
}

func readFstabLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %v", path, err)
	}
	// strings.Split по "\n" даёт лишний "" в конце; срезаем.
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// writeFstab атомарно перезаписывает fstab с preserve-by-default: mode и
// владелец существующего файла сохраняются (паттерн пилота core.line). Запись
// вызывается только когда содержимое реально поменялось (upsert/remove вернули
// changed=true) — idempotent no-op fstab не трогает. owner/group не передаются
// (всегда ""), поэтому preserve восстанавливает исходные uid/gid; для нового
// fstab (его не было) применяется дефолтный mode 0644.
func (m *Module) writeFstab(lines []string) error {
	content := strings.Join(lines, "\n") + "\n"
	return util.AtomicWritePreserving(
		m.FstabPath, []byte(content),
		"", "", "", m.LookupUser, m.LookupGroup,
	)
}
