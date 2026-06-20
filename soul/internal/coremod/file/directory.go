package file

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// applyDirectory — state `directory`: декларативное создание каталога
// (замена `core.exec.run install -d`). Идемпотентность:
//   - каталога нет → создать (MkdirAll при parents, иначе Mkdir) → changed=true;
//   - каталог есть, owner/group/mode совпадают → no-op (changed=false);
//   - каталог есть, но owner/group/mode дрейфят → починить (chmod/chown),
//     changed=true (паритет с applyPresent);
//   - путь существует, но это НЕ каталог (файл/симлинк) → ошибка, без перезаписи.
//
// recurse (рекурсивное выставление прав на содержимое) сознательно НЕ
// реализован в MVP — управляется только сам каталог.
func (m *Module) applyDirectory(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	modeStr, err := util.OptStringParam(req.Params, "mode")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	owner, err := util.OptStringParam(req.Params, "owner")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	group, err := util.OptStringParam(req.Params, "group")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	parents, err := util.OptBoolParam(req.Params, "parents")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	mode, perr := util.ParseMode(modeStr)
	if perr != nil {
		return util.SendFailed(stream, perr.Error())
	}

	created, modeChanged, ownerChanged := false, false, false

	info, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		if !info.IsDir() {
			// Путь занят файлом/симлинком — не перезаписываем (паритет с
			// поведением `mkdir`, которое падает на существующем файле).
			return util.SendFailed(stream, fmt.Sprintf("path %s exists and is not a directory", path))
		}
		if modeStr != "" && info.Mode().Perm() != mode {
			if cerr := os.Chmod(path, mode); cerr != nil {
				return util.SendFailed(stream, fmt.Sprintf("chmod %s: %v", path, cerr))
			}
			modeChanged = true
		}
	case errors.Is(statErr, fs.ErrNotExist):
		mkErr := mkdir(path, mode, parents)
		if mkErr != nil {
			return util.SendFailed(stream, mkErr.Error())
		}
		created = true
		// MkdirAll/Mkdir применяют mode с поправкой на umask, поэтому при явном
		// mode выставляем точные права отдельным chmod.
		if modeStr != "" {
			if cerr := os.Chmod(path, mode); cerr != nil {
				return util.SendFailed(stream, fmt.Sprintf("chmod %s: %v", path, cerr))
			}
		}
	default:
		return util.SendFailed(stream, fmt.Sprintf("stat %s: %v", path, statErr))
	}

	if owner != "" || group != "" {
		changed, oerr := util.ApplyOwnership(path, owner, group, m.LookupUser, m.LookupGroup)
		if oerr != nil {
			return util.SendFailed(stream, oerr.Error())
		}
		ownerChanged = changed
	}

	changed := created || modeChanged || ownerChanged
	return util.SendFinal(stream, changed, map[string]any{
		"path":    path,
		"mode":    fmt.Sprintf("%04o", mode),
		"created": created,
	})
}

// planDirectory — pure-read drift для state `directory` (ADR-031 Scry):
// тот же stat + perm/ownership-сравнение, что в начале applyDirectory, но без
// Mkdir/chmod/chown. Конфликт типа (путь — не каталог) — ошибка плана
// (util.PlanFailed), а НЕ false-clean.
func (m *Module) planDirectory(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	modeStr, err := util.OptStringParam(req.Params, "mode")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	owner, err := util.OptStringParam(req.Params, "owner")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	group, err := util.OptStringParam(req.Params, "group")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	mode, perr := util.ParseMode(modeStr)
	if perr != nil {
		return util.PlanFailed(perr.Error())
	}

	info, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		// Каталога нет — Apply создал бы его (drift).
		return util.SendPlanFinal(stream, true)
	case statErr != nil:
		return util.PlanFailed(fmt.Sprintf("stat %s: %v", path, statErr))
	}

	if !info.IsDir() {
		return util.PlanFailed(fmt.Sprintf("path %s exists and is not a directory", path))
	}
	if modeStr != "" && info.Mode().Perm() != mode {
		return util.SendPlanFinal(stream, true)
	}
	if owner != "" || group != "" {
		drift, _, _, oerr := util.OwnershipDrift(path, owner, group, m.LookupUser, m.LookupGroup)
		if oerr != nil {
			return util.PlanFailed(oerr.Error())
		}
		if drift {
			return util.SendPlanFinal(stream, true)
		}
	}
	return util.SendPlanFinal(stream, false)
}

// mkdir создаёт каталог: при parents — MkdirAll (промежуточные каталоги, как
// `mkdir -p`), иначе Mkdir (отсутствующий родитель → ошибка). mode применяется с
// поправкой на umask; точные права выставляет вызывающий chmod-ом при явном mode.
func mkdir(path string, mode fs.FileMode, parents bool) error {
	if parents {
		if err := os.MkdirAll(path, mode); err != nil {
			return fmt.Errorf("mkdir -p %s: %v", path, err)
		}
		return nil
	}
	if err := os.Mkdir(path, mode); err != nil {
		return fmt.Errorf("mkdir %s: %v", path, err)
	}
	return nil
}
