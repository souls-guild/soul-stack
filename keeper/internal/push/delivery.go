package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

// Хост-side layout доставки. Фиксирован, чтобы Soul-side искал бинарь и плагины
// по одному и тому же пути в pull/push (ADR-004, docs/keeper/push.md). Менять —
// PM-decision (затрагивает Soul-агента).
const (
	hostSoulDir    = "/var/lib/soul-stack/bin"
	hostModulesDir = "/var/lib/soul-stack/modules"
	hostSoulFile   = "soul"
	hostFileMode   = "0755"
)

// moduleNameRe ограничивает имя модуля безопасным алфавитом. Имя приходит из
// keeper-конфига (не от Soul), но даже доверенный источник лучше валидировать —
// fail-closed на любом отклонении (точки, слэши, `..`, кавычки), чтобы не дать
// никаких injection-возможностей в shell-команду на хосте.
var moduleNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Deliverer выкладывает soul-бинарь и зарегистрированные модули на push-хост по
// фиксированному layout-у (`/var/lib/soul-stack/{bin,modules}/`) с дедупом по
// SHA-256: если файл уже на хосте и совпадает — skip; иначе перезаписываем.
//
// Вызывается диспетчером ПЕРЕД exec-ом `soul apply`. Fail-closed: любая ошибка
// доставки прерывает прогон до запуска applier-а (нет смысла запускать staled
// бинарь).
type Deliverer interface {
	Deliver(ctx context.Context, session Session, spec SoulSpec) error
}

// SoulSpec — что доставить на хост: путь к локальному soul-бинарю + плагины
// (`soul-mod-*`). Версии фиксированы git-tag-ом (ADR-007); резолв конкретных
// файлов на keeper-стороне — зона выше (S3/runner).
type SoulSpec struct {
	// SoulBinaryPath — абсолютный путь к локальному ./soul-бинарю на keeper-узле.
	// Доставляется как `<hostSoulDir>/soul` с mode 0755.
	SoulBinaryPath string
	// Modules — нечё доставлять в `<hostModulesDir>/<Name>`. Порядок безразличен.
	Modules []ModuleSpec
}

// ModuleSpec — один плагин (`soul-mod-*`).
type ModuleSpec struct {
	// Name — имя файла на хосте (без каталога). Валидируется по moduleNameRe.
	Name string
	// Path — абсолютный путь к локальному файлу.
	Path string
}

// ShaDeliverer — дефолтная реализация Deliverer через Session.Run (ssh exec).
// Не тянет отдельную SFTP-зависимость: всё, что нужно — `mkdir -p`, `sha256sum`,
// запись файла через `cat > path` со stdin, `chmod`. Это и проще для in-process
// тестов (мок Session покрывает 100% поверхности), и не добавляет внешнего
// модуля под фичу.
//
// Семантика идемпотентна: если на хосте уже лежит файл с тем же SHA-256 — skip
// upload (только проверка). Это hot-path при повторных прогонах.
type ShaDeliverer struct{}

// NewShaDeliverer — конструктор для DI-явности (Deps.Deliverer). Возвращает
// указатель на пустую struct, чтобы swap на новую реализацию был тривиален.
func NewShaDeliverer() *ShaDeliverer { return &ShaDeliverer{} }

// Deliver проверяет каждый файл SoulSpec на хосте и докатывает несовпадающие.
// Шаги: validate spec → mkdir -p {bin,modules} → для каждого file: local sha256 →
// remote sha256 (sha256sum) → если совпадает skip, иначе upload + chmod 0755.
func (d *ShaDeliverer) Deliver(ctx context.Context, session Session, spec SoulSpec) error {
	if session == nil {
		return errors.New("push/delivery: session is nil")
	}
	if spec.SoulBinaryPath == "" {
		return errors.New("push/delivery: SoulBinaryPath обязателен")
	}
	for _, m := range spec.Modules {
		if !moduleNameRe.MatchString(m.Name) {
			return fmt.Errorf("push/delivery: недопустимое имя модуля %q (ожидался [a-zA-Z0-9._-]+)", m.Name)
		}
		if m.Path == "" {
			return fmt.Errorf("push/delivery: пустой Path у модуля %q", m.Name)
		}
	}

	// Один mkdir -p создаёт оба каталога — экономит roundtrip.
	if _, err := session.Run(ctx, fmt.Sprintf("mkdir -p %s %s", hostSoulDir, hostModulesDir), nil); err != nil {
		return fmt.Errorf("push/delivery: mkdir %s %s: %w", hostSoulDir, hostModulesDir, err)
	}

	if err := d.deliverFile(ctx, session, spec.SoulBinaryPath, path.Join(hostSoulDir, hostSoulFile)); err != nil {
		return fmt.Errorf("push/delivery: soul-бинарь: %w", err)
	}
	for _, m := range spec.Modules {
		remote := path.Join(hostModulesDir, m.Name)
		if err := d.deliverFile(ctx, session, m.Path, remote); err != nil {
			return fmt.Errorf("push/delivery: модуль %q: %w", m.Name, err)
		}
	}
	return nil
}

// deliverFile — sha256-сверка локального и удалённого файла + upload при
// расхождении. Sha-256 — компромисс по криптостойкости/скорости: для дедупа
// артефактов достаточно, коллизий нет.
func (d *ShaDeliverer) deliverFile(ctx context.Context, session Session, localPath, remotePath string) error {
	localSum, err := fileSha256(localPath)
	if err != nil {
		return fmt.Errorf("локальный sha256 %s: %w", localPath, err)
	}
	remoteSum, err := remoteSha256(ctx, session, remotePath)
	if err != nil {
		return fmt.Errorf("удалённый sha256 %s: %w", remotePath, err)
	}
	if remoteSum == localSum {
		return nil
	}

	// Шифт через stdin: `cat > path` не имеет лимита размера и не оставляет
	// файла в /tmp. shell-escape пути — не нужен, путь жёстко контролируется
	// (hostSoulDir + проверенное moduleNameRe).
	data, err := os.ReadFile(localPath) //nolint:gosec // путь — наш собственный артефакт keeper-стороны
	if err != nil {
		return fmt.Errorf("чтение локального файла %s: %w", localPath, err)
	}
	// `set -e; cat > path; chmod 0755 path` в одной команде: на отказ chmod
	// прогон тоже фейлится (без отдельного roundtrip).
	cmd := fmt.Sprintf("set -e; cat > %s && chmod %s %s", remotePath, hostFileMode, remotePath)
	if _, err := session.Run(ctx, cmd, data); err != nil {
		return fmt.Errorf("upload %s: %w", remotePath, err)
	}
	// Пост-верификация: убеждаемся, что записанный файл соответствует sha-сумме
	// (защита от усечения/искажения в транспорте). Дёшево относительно upload-а.
	got, err := remoteSha256(ctx, session, remotePath)
	if err != nil {
		return fmt.Errorf("проверка sha256 после upload %s: %w", remotePath, err)
	}
	if got != localSum {
		return fmt.Errorf("sha256 после upload %s не совпал: got %s, want %s", remotePath, got, localSum)
	}
	return nil
}

// fileSha256 считает sha256-хеш локального файла потоково (без полной загрузки
// в память для крупных бинарей).
func fileSha256(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // путь — наш собственный артефакт keeper-стороны
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// remoteSha256 запрашивает sha256-хеш файла на хосте. Если файла нет — пустая
// строка (== «никогда не совпадёт с локальным» → upload). На иной ошибке —
// возврат err (fail-closed: не доставляем «вслепую», если sha256sum/permissions
// сбойнули).
//
// Парсинг: `sha256sum <path>` печатает `<hex>  <path>\n`; нас интересует первое
// поле.
func remoteSha256(ctx context.Context, session Session, p string) (string, error) {
	// `[ -f path ] && sha256sum path || echo MISSING` — но shell-эскейп
	// раздражает; проще проверить отдельной командой.
	// Single-quote вокруг %s — профилактика shell-injection при будущем
	// расширении regex для `m.Name`; сейчас path под нашим контролем.
	stdout, err := session.Run(ctx, fmt.Sprintf("test -f '%s' && sha256sum '%s' || echo MISSING", p, p), nil)
	if err != nil {
		// `||` гарантирует exit 0, sshd возвращает не nil только на проблемах
		// channel/exec — это уже не «нет файла», это сбой транспорта.
		return "", fmt.Errorf("ssh exec sha256sum: %w", err)
	}
	out := strings.TrimSpace(stdout)
	if out == "" || out == "MISSING" {
		return "", nil
	}
	fields := strings.Fields(out)
	if len(fields) < 1 {
		return "", fmt.Errorf("неожиданный вывод sha256sum: %q", out)
	}
	hexSum := fields[0]
	if len(hexSum) != sha256.Size*2 {
		return "", fmt.Errorf("неожиданный sha256 в выводе: %q", hexSum)
	}
	return hexSum, nil
}
