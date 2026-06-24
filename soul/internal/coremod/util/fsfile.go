package util

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// ParseMode разбирает октальный mode-параметр (например, "0644"). Пустая
// строка → дефолт 0o644. Невалидная восьмеричная строка → ошибка с именем
// параметра. Биты сверх ModePerm отбрасываются.
//
// Единая точка для всех core-модулей, материализующих файлы (core.file,
// core.url, …); не дублировать локальными копиями.
func ParseMode(modeStr string) (fs.FileMode, error) {
	if modeStr == "" {
		return fs.FileMode(0o644), nil
	}
	parsed, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("param %q: invalid octal mode %q", "mode", modeStr)
	}
	return fs.FileMode(parsed) & fs.ModePerm, nil
}

// ReadRegularFile читает содержимое regular-файла по абсолютному пути src.
// Используется core-модулями, которые копируют содержимое уже лежащего на хосте
// файла (core.file.present с src). Контракт (security-граница):
//
//   - src обязан быть абсолютным (filepath.IsAbs) — относительный reject, чтобы
//     resolve относительно cwd Soul-демона (обычно root) не стал footgun-ом;
//   - тип проверяется через os.Lstat + IsRegular(), ИМЕННО Lstat: симлинк
//     reject-ится, а не следуется — защита от подмены источника симлинком на
//     чувствительный файл между объявлением и применением;
//   - каталог / симлинк / device / socket / fifo → explicit-reject (MVP — только
//     regular file);
//   - отсутствие / permission-ошибка чтения пробрасываются как есть.
//
// Файл читается в память один раз; вызывающий хэширует и пишет тот же буфер
// (без двойного чтения — защита от TOCTOU между сверкой и записью).
func ReadRegularFile(src string) ([]byte, error) {
	if !filepath.IsAbs(src) {
		return nil, fmt.Errorf("src must be absolute: %q", src)
	}
	info, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read src %s: no such file", src)
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("src %s is not a regular file", src)
	}
	return os.ReadFile(src)
}

// AtomicWrite материализует data в path атомарно: temp-файл в той же
// директории + rename. Гарантирует, что наблюдатель видит либо старый, либо
// полный новый файл, но не частично записанный. На любой ошибке temp удаляется.
//
// Единая точка для core-модулей, которым важна атомарность записи (core.file
// rendered, core.url fetched); не дублировать локальными копиями.
func AtomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("fsync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// AtomicWritePreserving материализует data в path атомарно (как AtomicWrite),
// но с правилом preserve-by-default для in-place правки СУЩЕСТВУЮЩЕГО файла
// ([ADR-015], пилот core.line, паттерн для будущих in-place core — core.repo):
//
//   - mode:  modeStr=="" и файл существовал → сохранить текущий mode исходного
//     файла; modeStr задан → ParseMode(modeStr) (override). Файла не было →
//     ParseMode(modeStr) (для "" это дефолт 0644).
//   - owner/group: не заданы и файл существовал → восстановить старые uid/gid;
//     заданы → ApplyOwnership (override). Файла не было → ApplyOwnership только
//     если что-то задано (иначе остаётся owner текущего процесса).
//
// rename теряет mode/owner оригинала (temp создаётся с правами процесса),
// поэтому preserve восстанавливается ЯВНО после записи. Снимок mode+uid+gid
// делается ДО записи (Stat исходного файла).
//
// lookupUser/lookupGroup — точки подмены для unit-тестов (как в ApplyOwnership).
// Единая наследуемая форма: in-place core-модули вызывают эту функцию, не
// дублируя preserve-логику локально.
func AtomicWritePreserving(
	path string, data []byte, modeStr, owner, group string,
	lookupUser func(name string) (*user.User, error),
	lookupGroup func(name string) (*user.Group, error),
) error {
	var (
		prevMode    fs.FileMode
		prevUID     int
		prevGID     int
		hadOwnerSys bool
	)
	info, statErr := os.Stat(path)
	existed := statErr == nil
	if existed {
		prevMode = info.Mode().Perm()
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			prevUID = int(sys.Uid)
			prevGID = int(sys.Gid)
			hadOwnerSys = true
		}
	}

	mode, err := ParseMode(modeStr)
	if err != nil {
		return err
	}
	if modeStr == "" && existed {
		mode = prevMode
	}

	if err := AtomicWrite(path, data, mode); err != nil {
		return err
	}

	if owner != "" || group != "" {
		if _, err := ApplyOwnership(path, owner, group, lookupUser, lookupGroup); err != nil {
			return err
		}
		return nil
	}
	// owner/group не заданы: для существовавшего файла rename сбросил владельца
	// на процесс — восстанавливаем исходные uid/gid. Для нового файла оставляем
	// владельца процесса (поведение AtomicWrite без ownership).
	if existed && hadOwnerSys {
		if err := os.Chown(path, prevUID, prevGID); err != nil {
			return fmt.Errorf("restore ownership %s: %v", path, err)
		}
	}
	return nil
}

// ApplyOwnership резолвит owner/group → uid/gid, сравнивает с текущими, делает
// chown если расходится. changed=true только если хотя бы одно значение
// действительно поменялось. lookupUser/lookupGroup — точки подмены для
// unit-тестов (в проде — user.Lookup / user.LookupGroup).
//
// Единая точка для core-модулей, выставляющих owner/group на файлах
// (core.file, core.url); не дублировать локальными копиями.
func ApplyOwnership(
	path, owner, group string,
	lookupUser func(name string) (*user.User, error),
	lookupGroup func(name string) (*user.Group, error),
) (bool, error) {
	drift, wantUID, wantGID, err := OwnershipDrift(path, owner, group, lookupUser, lookupGroup)
	if err != nil {
		return false, err
	}
	if !drift {
		return false, nil
	}
	if err := os.Chown(path, wantUID, wantGID); err != nil {
		return false, fmt.Errorf("chown %s: %v", path, err)
	}
	return true, nil
}

// OwnershipDrift — pure-read половина ApplyOwnership (ADR-031 Scry): резолвит
// owner/group → uid/gid и сравнивает с текущими БЕЗ chown. Возвращает drift
// (хотя бы одно значение расходится) и целевые wantUID/wantGID (для последующего
// chown в ApplyOwnership). Чистое чтение — ApplyOwnership строится поверх неё,
// Plan-путь использует её напрямую (drift без мутации).
func OwnershipDrift(
	path, owner, group string,
	lookupUser func(name string) (*user.User, error),
	lookupGroup func(name string) (*user.Group, error),
) (drift bool, wantUID, wantGID int, err error) {
	info, serr := os.Stat(path)
	if serr != nil {
		return false, 0, 0, fmt.Errorf("stat %s: %v", path, serr)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// На системах, где Sys() не Stat_t (теоретически — Windows), chown
		// не имеет смысла. Soul-агент таргетит unix, это не блокер.
		return false, 0, 0, fmt.Errorf("chown not supported on this platform")
	}
	wantUID = int(sys.Uid)
	wantGID = int(sys.Gid)
	if owner != "" {
		u, lerr := lookupUser(owner)
		if lerr != nil {
			return false, 0, 0, fmt.Errorf("lookup user %q: %v", owner, lerr)
		}
		uid, _ := strconv.Atoi(u.Uid)
		if uid != int(sys.Uid) {
			wantUID = uid
			drift = true
		}
	}
	if group != "" {
		g, lerr := lookupGroup(group)
		if lerr != nil {
			return false, 0, 0, fmt.Errorf("lookup group %q: %v", group, lerr)
		}
		gid, _ := strconv.Atoi(g.Gid)
		if gid != int(sys.Gid) {
			wantGID = gid
			drift = true
		}
	}
	return drift, wantUID, wantGID, nil
}
